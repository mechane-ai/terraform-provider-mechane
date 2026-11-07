package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"

	version "github.com/hashicorp/go-version"
)

const maximumWorkerManifestBytes = 2 * 1024 * 1024

var workerSHA256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var workerRevisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var workerReleaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
var workerRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`)

type workerFormats struct {
	Wire       map[string][]string `json:"wire"`
	Migrations map[string][]string `json:"migrations"`
}

type workerReleaseManifest struct {
	FormatVersion int    `json:"format_version"`
	IsPreview     *bool  `json:"is_preview"`
	Version       string `json:"version"`
	Repository    string `json:"repository"`
	PublicCommit  string `json:"public_commit"`
	Worker        struct {
		SHA256             string   `json:"sha256"`
		Bytes              int64    `json:"bytes"`
		Path               string   `json:"path"`
		CompatibilityDate  string   `json:"compatibility_date"`
		CompatibilityFlags []string `json:"compatibility_flags"`
		ContainerClasses   []string `json:"container_classes"`
		Bindings           []struct {
			Name      string `json:"name"`
			ClassName string `json:"class_name"`
		} `json:"bindings"`
		Exports map[string]workerExport `json:"exports"`
	} `json:"worker"`
	Formats workerFormats `json:"formats"`
}

type workerReleaseMarker struct {
	SourceRevision string
	Fingerprint    string
	ManifestSHA256 string
}

func parseWorkerReleaseVersion(value string) (*version.Version, error) {
	if workerReleaseVersionPattern.MatchString(value) {
		parsed, err := version.NewSemver(value)
		if err == nil {
			return parsed, nil
		}
	}
	return nil, errors.New("Worker releases must use a complete semantic version without a prefix or build metadata.")
}

func parseWorkerReleaseMarker(message string) (workerReleaseMarker, bool, error) {
	if !strings.HasPrefix(message, "mechane:") {
		return workerReleaseMarker{}, false, nil
	}
	parts := strings.Split(message, ":")
	if len(parts) != 4 || !workerRevisionPattern.MatchString(parts[1]) || !workerSHA256Pattern.MatchString(parts[2]) || !workerSHA256Pattern.MatchString(parts[3]) {
		return workerReleaseMarker{}, true, errors.New("The live Worker has malformed native release metadata.")
	}
	return workerReleaseMarker{SourceRevision: parts[1], Fingerprint: parts[2], ManifestSHA256: parts[3]}, true, nil
}

func validateWorkerFormats(formats workerFormats) error {
	for _, entries := range []map[string][]string{formats.Wire, formats.Migrations} {
		if len(entries) == 0 {
			return errors.New("Release compatibility metadata must contain both complete wire and migration maps.")
		}
		for name, values := range entries {
			if strings.TrimSpace(name) == "" || values == nil {
				return errors.New("Compatibility maps require nonempty category names and arrays of strings.")
			}
			seen := map[string]bool{}
			for _, value := range values {
				if strings.TrimSpace(value) == "" || seen[value] {
					return errors.New("Compatibility categories must contain distinct nonempty strings; an empty category array is allowed.")
				}
				seen[value] = true
			}
		}
	}
	return nil
}

func decodeWorkerManifest(content []byte) (workerReleaseManifest, error) {
	var manifest workerReleaseManifest
	if json.Unmarshal(content, &manifest) != nil {
		return manifest, errors.New("The installation manifest is not valid release metadata.")
	}
	if manifest.FormatVersion != 1 || manifest.IsPreview == nil || *manifest.IsPreview {
		return manifest, errors.New("Use a published, non-preview installation manifest with format_version 1.")
	}
	if !workerRepositoryPattern.MatchString(manifest.Repository) || !workerRevisionPattern.MatchString(manifest.PublicCommit) || !workerSHA256Pattern.MatchString(manifest.Worker.SHA256) {
		return manifest, errors.New("The installation manifest must identify a public repository, full source revision, and Worker SHA-256.")
	}
	if _, err := parseWorkerReleaseVersion(manifest.Version); err != nil {
		return manifest, err
	}
	return manifest, validateWorkerFormats(manifest.Formats)
}

func readWorkerManifestBytes(reader io.Reader) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, maximumWorkerManifestBytes+1))
	if err != nil || len(content) > maximumWorkerManifestBytes {
		return nil, errors.New("The installation manifest is unreadable or exceeds 2 MiB.")
	}
	return content, nil
}

func readWorkerManifest(model workerReleaseModel) (workerReleaseManifest, error) {
	file, err := os.Open(model.ReleaseManifestFile.ValueString())
	if err != nil {
		return workerReleaseManifest{}, errors.New("The installation manifest could not be opened. Restore the immutable release artifact before applying.")
	}
	defer file.Close()
	content, err := readWorkerManifestBytes(file)
	if err != nil {
		return workerReleaseManifest{}, err
	}
	if workerDigest(content) != model.ReleaseManifestSHA256.ValueString() {
		return workerReleaseManifest{}, errors.New("The installation manifest does not match release_manifest_sha256.")
	}
	manifest, err := decodeWorkerManifest(content)
	if err != nil {
		return manifest, err
	}
	if manifest.Worker.Path != "worker.js" || manifest.Worker.Bytes <= 0 || manifest.Worker.CompatibilityDate == "" || manifest.Worker.CompatibilityFlags == nil || manifest.Worker.ContainerClasses == nil || len(manifest.Worker.Bindings) == 0 || len(manifest.Worker.Exports) == 0 {
		return manifest, errors.New("The release manifest must contain complete Worker artifact, compatibility, binding, export, and container metadata.")
	}
	if _, err := time.Parse(time.DateOnly, manifest.Worker.CompatibilityDate); err != nil {
		return manifest, errors.New("The release compatibility date must use YYYY-MM-DD.")
	}
	for name, export := range manifest.Worker.Exports {
		if name == "" || export.Type != "durable-object" || export.Storage != "sqlite" || export.State != "created" {
			return manifest, errors.New("Release exports must name live SQLite Durable Object classes.")
		}
	}
	seen := map[string]bool{}
	for _, binding := range manifest.Worker.Bindings {
		if _, declared := manifest.Worker.Exports[binding.ClassName]; !declared || binding.Name == "" || seen[binding.Name] {
			return manifest, errors.New("Release bindings must have distinct names and refer to declared SQLite exports.")
		}
		seen[binding.Name] = true
	}
	for _, class := range manifest.Worker.ContainerClasses {
		if _, declared := manifest.Worker.Exports[class]; !declared {
			return manifest, errors.New("Release container classes must refer to declared SQLite exports.")
		}
	}
	return manifest, nil
}

func downloadWorkerManifest(ctx context.Context, repository string, live workerReleaseConfiguration) (workerReleaseManifest, error) {
	if !workerRepositoryPattern.MatchString(repository) {
		return workerReleaseManifest{}, errors.New("The release repository is not a public GitHub repository identifier.")
	}
	if _, err := parseWorkerReleaseVersion(live.ReleaseVersion); err != nil {
		return workerReleaseManifest{}, err
	}
	address := "https://github.com/" + repository + "/releases/download/v" + live.ReleaseVersion + "/installation.json"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return workerReleaseManifest{}, errors.New("The public release manifest request could not be constructed.")
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(request *http.Request, previous []*http.Request) error {
		host := request.URL.Hostname()
		allowedHost := host == "github.com" || host == "release-assets.githubusercontent.com" || host == "objects.githubusercontent.com"
		if len(previous) > 3 || request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Port() != "" || !allowedHost {
			return errors.New("The public manifest redirected outside GitHub release storage.")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return workerReleaseManifest{}, errors.New("The recorded public installation manifest could not be retrieved. Restore its release asset before applying.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return workerReleaseManifest{}, fmt.Errorf("The recorded public installation manifest returned HTTP %d. Restore its release asset before applying.", response.StatusCode)
	}
	content, err := readWorkerManifestBytes(response.Body)
	if err != nil {
		return workerReleaseManifest{}, err
	}
	if workerDigest(content) != live.ReleaseManifestSHA256 {
		return workerReleaseManifest{}, errors.New("The published installation manifest does not match the live Worker's recorded SHA-256.")
	}
	manifest, err := decodeWorkerManifest(content)
	if err != nil {
		return manifest, err
	}
	if manifest.Repository != repository || manifest.Version != live.ReleaseVersion || manifest.PublicCommit != live.SourceRevision || manifest.Worker.SHA256 != live.BundleSHA256 {
		return manifest, errors.New("The recorded installation manifest disagrees with the live Worker version, source revision, or actual bundle bytes.")
	}
	return manifest, nil
}

// Only verified release metadata supplies immutable Worker configuration.
func (model *workerReleaseModel) acceptManifest(ctx context.Context, manifest workerReleaseManifest) error {
	model.BundleFile = types.StringValue(filepath.Join(filepath.Dir(model.ReleaseManifestFile.ValueString()), manifest.Worker.Path))
	model.BundleSHA256 = types.StringValue(manifest.Worker.SHA256)
	model.ReleaseVersion = types.StringValue(manifest.Version)
	model.SourceRevision = types.StringValue(manifest.PublicCommit)
	model.CompatibilityDate = types.StringValue(manifest.Worker.CompatibilityDate)
	flags, diagnostics := types.SetValueFrom(ctx, types.StringType, manifest.Worker.CompatibilityFlags)
	if diagnostics.HasError() {
		return errors.New("The release compatibility flags are invalid.")
	}
	model.CompatibilityFlags = flags
	classes, diagnostics := types.SetValueFrom(ctx, types.StringType, manifest.Worker.ContainerClasses)
	if diagnostics.HasError() {
		return errors.New("The release container classes are invalid.")
	}
	model.ContainerClasses = classes
	exports := map[string]workerExportModel{}
	for name, export := range manifest.Worker.Exports {
		exports[name] = workerExportModel{Type: types.StringValue(export.Type), Storage: types.StringValue(export.Storage), State: types.StringValue(export.State)}
	}
	model.Exports, diagnostics = types.MapValueFrom(ctx, workerExportObjectType(), exports)
	if diagnostics.HasError() {
		return errors.New("The release exports are invalid.")
	}
	bindings := []workerBindingModel{}
	for _, binding := range manifest.Worker.Bindings {
		bindings = append(bindings, workerBindingModel{Name: types.StringValue(binding.Name), Type: types.StringValue("durable_object_namespace"), ClassName: types.StringValue(binding.ClassName)})
	}
	model.DurableObjectBindings, diagnostics = types.SetValueFrom(ctx, workerBindingObjectType(), bindings)
	if diagnostics.HasError() {
		return errors.New("The release Durable Object bindings are invalid.")
	}
	return nil
}
