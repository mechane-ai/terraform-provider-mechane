package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type workerBindingModel struct {
	Name         types.String `tfsdk:"name"`
	Type         types.String `tfsdk:"type"`
	Text         types.String `tfsdk:"text"`
	ClassName    types.String `tfsdk:"class_name"`
	BucketName   types.String `tfsdk:"bucket_name"`
	Jurisdiction types.String `tfsdk:"jurisdiction"`
	StoreID      types.String `tfsdk:"store_id"`
	SecretName   types.String `tfsdk:"secret_name"`
}

type workerExportModel struct {
	Type    types.String `tfsdk:"type"`
	Storage types.String `tfsdk:"storage"`
	State   types.String `tfsdk:"state"`
}

type workerBinding struct {
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Text         *string `json:"text,omitempty"`
	ClassName    *string `json:"class_name,omitempty"`
	BucketName   *string `json:"bucket_name,omitempty"`
	Jurisdiction *string `json:"jurisdiction,omitempty"`
	StoreID      *string `json:"store_id,omitempty"`
	SecretName   *string `json:"secret_name,omitempty"`
	NamespaceID  string  `json:"namespace_id,omitempty"`
}

type workerExport struct {
	Type    string `json:"type"`
	Storage string `json:"storage,omitempty"`
	State   string `json:"state"`
}

type workerContainer struct {
	ClassName string `json:"class_name"`
}

type workerReleaseConfiguration struct {
	BundleSHA256          string                  `json:"bundle_sha256"`
	MainModule            string                  `json:"main_module"`
	CompatibilityDate     string                  `json:"compatibility_date"`
	CompatibilityFlags    []string                `json:"compatibility_flags"`
	Bindings              []workerBinding         `json:"bindings"`
	Exports               map[string]workerExport `json:"exports"`
	Containers            []workerContainer       `json:"containers"`
	CPUMilliseconds       int64                   `json:"cpu_ms"`
	ReleaseVersion        string                  `json:"release_version"`
	SourceRevision        string                  `json:"source_revision"`
	ReleaseManifestSHA256 string                  `json:"release_manifest_sha256"`
}

func workerConfiguration(ctx context.Context, model workerReleaseModel) (workerReleaseConfiguration, error) {
	config := workerReleaseConfiguration{
		BundleSHA256: model.BundleSHA256.ValueString(), MainModule: "worker.js",
		CompatibilityDate: model.CompatibilityDate.ValueString(), CompatibilityFlags: []string{},
		Bindings: []workerBinding{}, Exports: map[string]workerExport{}, Containers: []workerContainer{},
		CPUMilliseconds: model.CPUMilliseconds.ValueInt64(),
		ReleaseVersion:  model.ReleaseVersion.ValueString(), SourceRevision: model.SourceRevision.ValueString(),
		ReleaseManifestSHA256: model.ReleaseManifestSHA256.ValueString(),
	}
	var bindings []workerBindingModel
	if diagnostics := model.Bindings.ElementsAs(ctx, &bindings, false); diagnostics.HasError() {
		return config, errors.New("Worker bindings must be fully known before applying.")
	}
	seen := map[string]bool{}
	for _, binding := range bindings {
		value := workerBinding{
			Name: binding.Name.ValueString(), Type: binding.Type.ValueString(), Text: binding.Text.ValueStringPointer(),
			ClassName: binding.ClassName.ValueStringPointer(), BucketName: binding.BucketName.ValueStringPointer(),
			Jurisdiction: binding.Jurisdiction.ValueStringPointer(), StoreID: binding.StoreID.ValueStringPointer(),
			SecretName: binding.SecretName.ValueStringPointer(),
		}
		if value.Name == "" || seen[value.Name] {
			return config, errors.New("Worker bindings must have distinct nonempty names.")
		}
		seen[value.Name] = true
		if err := validateWorkerBinding(value); err != nil {
			return config, err
		}
		config.Bindings = append(config.Bindings, value)
	}
	var exports map[string]workerExportModel
	if diagnostics := model.Exports.ElementsAs(ctx, &exports, false); diagnostics.HasError() {
		return config, errors.New("Worker exports must be fully known before applying.")
	}
	for name, export := range exports {
		value := workerExport{Type: export.Type.ValueString(), Storage: export.Storage.ValueString(), State: export.State.ValueString()}
		if value.State == "" {
			value.State = "created"
		}
		if name == "" || value.Type != "durable-object" || value.Storage != "sqlite" || value.State != "created" {
			return config, errors.New("Release exports must name live SQLite Durable Object classes. Deletion, renaming, and transfer require a separate explicit migration.")
		}
		config.Exports[name] = value
	}
	for _, binding := range config.Bindings {
		if binding.Type == "durable_object_namespace" {
			if _, declared := config.Exports[*binding.ClassName]; !declared {
				return config, errors.New("Every Durable Object binding must name a declared SQLite export of this Worker.")
			}
		}
	}
	var classes []string
	if !model.ContainerClasses.IsNull() {
		if diagnostics := model.ContainerClasses.ElementsAs(ctx, &classes, false); diagnostics.HasError() {
			return config, errors.New("Container classes must be fully known before applying.")
		}
	}
	for _, class := range classes {
		if _, exists := config.Exports[class]; !exists {
			return config, errors.New("Each container class must be a declared SQLite Durable Object export.")
		}
		config.Containers = append(config.Containers, workerContainer{ClassName: class})
	}
	if !model.CompatibilityFlags.IsNull() {
		if diagnostics := model.CompatibilityFlags.ElementsAs(ctx, &config.CompatibilityFlags, false); diagnostics.HasError() {
			return config, errors.New("Compatibility flags must be fully known before applying.")
		}
	}
	config.normalize()
	return config, nil
}

func validateWorkerBinding(binding workerBinding) error {
	valid := false
	switch binding.Type {
	case "plain_text":
		valid = binding.Text != nil && binding.ClassName == nil && binding.BucketName == nil && binding.Jurisdiction == nil && binding.StoreID == nil && binding.SecretName == nil
	case "durable_object_namespace":
		valid = binding.ClassName != nil && *binding.ClassName != "" && binding.Text == nil && binding.BucketName == nil && binding.Jurisdiction == nil && binding.StoreID == nil && binding.SecretName == nil
	case "r2_bucket":
		valid = binding.BucketName != nil && *binding.BucketName != "" && binding.Text == nil && binding.ClassName == nil && binding.StoreID == nil && binding.SecretName == nil
	case "secrets_store_secret":
		valid = binding.StoreID != nil && *binding.StoreID != "" && binding.SecretName != nil && *binding.SecretName != "" && binding.Text == nil && binding.ClassName == nil && binding.BucketName == nil && binding.Jurisdiction == nil
	}
	if !valid {
		return errors.New("Use plain_text, durable_object_namespace, r2_bucket, or secrets_store_secret bindings with only their corresponding fields. Credential values and secret_text bindings are not accepted.")
	}
	return nil
}

func (config *workerReleaseConfiguration) normalize() {
	if config.CompatibilityFlags == nil {
		config.CompatibilityFlags = []string{}
	}
	if config.Bindings == nil {
		config.Bindings = []workerBinding{}
	}
	if config.Exports == nil {
		config.Exports = map[string]workerExport{}
	}
	if config.Containers == nil {
		config.Containers = []workerContainer{}
	}
	slices.Sort(config.CompatibilityFlags)
	for index := range config.Bindings {
		config.Bindings[index].NamespaceID = ""
	}
	slices.SortFunc(config.Bindings, func(a, b workerBinding) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(config.Containers, func(a, b workerContainer) int { return strings.Compare(a.ClassName, b.ClassName) })
	for name, export := range config.Exports {
		if export.State == "" {
			export.State = "created"
			config.Exports[name] = export
		}
	}
}

func (config workerReleaseConfiguration) fingerprint() (string, error) {
	bytes, err := json.Marshal(config)
	if err != nil {
		return "", errors.New("The Worker release metadata could not be encoded.")
	}
	return workerDigest(bytes), nil
}

func workerDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func readWorkerBundle(model workerReleaseModel) ([]byte, error) {
	file, err := os.Open(model.BundleFile.ValueString())
	if err != nil {
		return nil, errors.New("The release bundle could not be opened. Restore the immutable release artifact and retry.")
	}
	defer file.Close()
	const maximumBundleBytes = 64 * 1024 * 1024
	content, err := io.ReadAll(io.LimitReader(file, maximumBundleBytes+1))
	if err != nil || len(content) > maximumBundleBytes {
		return nil, errors.New("The release bundle is unreadable or exceeds 64 MiB.")
	}
	if workerDigest(content) != model.BundleSHA256.ValueString() {
		return nil, errors.New("The release bundle does not match bundle_sha256. Restore the immutable release artifact before applying.")
	}
	return content, nil
}

func workerBindingObjectType() types.ObjectType {
	attributes := map[string]attr.Type{}
	for _, name := range []string{"name", "type", "text", "class_name", "bucket_name", "jurisdiction", "store_id", "secret_name"} {
		attributes[name] = types.StringType
	}
	return types.ObjectType{AttrTypes: attributes}
}

func workerExportObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{"type": types.StringType, "storage": types.StringType, "state": types.StringType}}
}

func (model *workerReleaseModel) readWorkerVersion(ctx context.Context, version workerVersion) error {
	config, err := version.configuration()
	if err != nil {
		return err
	}
	model.ID = types.StringValue(model.AccountID.ValueString() + "/" + model.WorkerName.ValueString())
	model.VersionID = types.StringValue(version.ID)
	model.BundleSHA256 = types.StringValue(config.BundleSHA256)
	model.CompatibilityDate = types.StringValue(config.CompatibilityDate)
	model.CPUMilliseconds = types.Int64Value(config.CPUMilliseconds)
	model.ReleaseVersion = types.StringValue(config.ReleaseVersion)
	model.SourceRevision = types.StringValue(config.SourceRevision)
	model.ReleaseManifestSHA256 = types.StringValue(config.ReleaseManifestSHA256)
	flags, diagnostics := types.SetValueFrom(ctx, types.StringType, config.CompatibilityFlags)
	if diagnostics.HasError() {
		return errors.New("The Worker compatibility flags could not be decoded.")
	}
	model.CompatibilityFlags = flags
	bindings := []workerBindingModel{}
	for _, binding := range config.Bindings {
		text := types.StringNull()
		if binding.Type == "plain_text" {
			text = types.StringPointerValue(binding.Text)
		}
		bindings = append(bindings, workerBindingModel{
			Name: types.StringValue(binding.Name), Type: types.StringValue(binding.Type), Text: text,
			ClassName: types.StringPointerValue(binding.ClassName), BucketName: types.StringPointerValue(binding.BucketName),
			Jurisdiction: types.StringPointerValue(binding.Jurisdiction), StoreID: types.StringPointerValue(binding.StoreID),
			SecretName: types.StringPointerValue(binding.SecretName),
		})
	}
	model.Bindings, diagnostics = types.SetValueFrom(ctx, workerBindingObjectType(), bindings)
	if diagnostics.HasError() {
		return errors.New("The Worker binding metadata could not be decoded.")
	}
	exports := map[string]workerExportModel{}
	for name, export := range config.Exports {
		exports[name] = workerExportModel{Type: types.StringValue(export.Type), Storage: types.StringValue(export.Storage), State: types.StringValue(export.State)}
	}
	model.Exports, diagnostics = types.MapValueFrom(ctx, workerExportObjectType(), exports)
	if diagnostics.HasError() {
		return errors.New("The Worker export metadata could not be decoded.")
	}
	classes := []string{}
	for _, container := range config.Containers {
		classes = append(classes, container.ClassName)
	}
	model.ContainerClasses, diagnostics = types.SetValueFrom(ctx, types.StringType, classes)
	if diagnostics.HasError() {
		return errors.New("The Worker container metadata could not be decoded.")
	}
	return model.readWorkerNamespaces(ctx, version)
}

func (model *workerReleaseModel) readWorkerNamespaces(ctx context.Context, version workerVersion) error {
	namespaces := map[string]string{}
	for _, binding := range version.Bindings {
		if binding.Type != "durable_object_namespace" {
			continue
		}
		if binding.ClassName == nil || binding.NamespaceID == "" {
			return errors.New("Cloudflare did not return a namespace ID for a bound Durable Object class.")
		}
		if previous, found := namespaces[*binding.ClassName]; found && previous != binding.NamespaceID {
			return errors.New("Cloudflare returned conflicting namespace IDs for a Durable Object class.")
		}
		namespaces[*binding.ClassName] = binding.NamespaceID
	}
	value, diagnostics := types.MapValueFrom(ctx, types.StringType, namespaces)
	if diagnostics.HasError() {
		return errors.New("The Worker namespace identities could not be decoded.")
	}
	model.DurableObjectNamespaces = value
	return nil
}

func (version workerVersion) configuration() (workerReleaseConfiguration, error) {
	config := workerReleaseConfiguration{
		MainModule: version.MainModule, CompatibilityDate: version.CompatibilityDate, CompatibilityFlags: version.CompatibilityFlags,
		Bindings: slices.Clone(version.Bindings), Exports: version.Exports, Containers: slices.Clone(version.Containers),
		CPUMilliseconds: version.Limits.CPUMilliseconds, ReleaseVersion: strings.TrimPrefix(version.Annotations["workers/tag"], "v"),
	}
	if version.ID == "" || len(version.Modules) != 1 || version.Modules[0].Name != version.MainModule {
		return config, errors.New("The Worker version must contain exactly one main module.")
	}
	content, err := base64.StdEncoding.DecodeString(version.Modules[0].ContentBase64)
	if err != nil {
		return config, errors.New("The Worker main module could not be decoded.")
	}
	config.BundleSHA256 = workerDigest(content)
	marker, marked, err := parseWorkerReleaseMarker(version.Annotations["workers/message"])
	if err != nil {
		return config, err
	}
	if marked {
		config.SourceRevision = marker.SourceRevision
		config.ReleaseManifestSHA256 = marker.ManifestSHA256
	}
	config.normalize()
	return config, nil
}

func workerReleaseMessage(config workerReleaseConfiguration, fingerprint string) string {
	return fmt.Sprintf("mechane:%s:%s:%s", config.SourceRevision, fingerprint, config.ReleaseManifestSHA256)
}
