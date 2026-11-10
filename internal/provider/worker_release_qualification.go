package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var qualifiedFilePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]+$`)

type qualifiedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type workerQualification struct {
	FormatVersion int                        `json:"format_version"`
	Version       string                     `json:"version"`
	PublicCommit  string                     `json:"public_commit"`
	Artifacts     qualifiedFile              `json:"artifacts"`
	Installation  qualifiedFile              `json:"installation"`
	Inputs        map[string][]qualifiedFile `json:"inputs"`
	Checks        []struct {
		Name     string        `json:"name"`
		Outcome  string        `json:"outcome"`
		Evidence qualifiedFile `json:"evidence"`
	} `json:"checks"`
}

type workerArtifactReceipt struct {
	FormatVersion int    `json:"format_version"`
	Version       string `json:"version"`
	Repository    string `json:"repository"`
	PublicCommit  string `json:"public_commit"`
	Assets        []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
	} `json:"assets"`
}

type workerCheckEvidence struct {
	FormatVersion int           `json:"format_version"`
	Name          string        `json:"name"`
	Outcome       string        `json:"outcome"`
	PublicCommit  string        `json:"public_commit"`
	Artifacts     qualifiedFile `json:"artifacts"`
	Installation  qualifiedFile `json:"installation"`
	InputsSHA256  string        `json:"inputs_sha256"`
}

func qualificationInputsDigest(inputs map[string][]qualifiedFile) (string, error) {
	normalized := struct {
		Toolchain     []qualifiedFile `json:"toolchain"`
		Fixtures      []qualifiedFile `json:"fixtures"`
		Policy        []qualifiedFile `json:"policy"`
		Configuration []qualifiedFile `json:"configuration"`
	}{Toolchain: sortedQualifiedFiles(inputs["toolchain"]), Fixtures: sortedQualifiedFiles(inputs["fixtures"]), Policy: sortedQualifiedFiles(inputs["policy"]), Configuration: sortedQualifiedFiles(inputs["configuration"])}
	content, err := json.Marshal(normalized)
	if err != nil {
		return "", errors.New("The qualification input inventory could not be encoded.")
	}
	return workerDigest(append(content, '\n')), nil
}

func sortedQualifiedFiles(files []qualifiedFile) []qualifiedFile {
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b qualifiedFile) int { return strings.Compare(a.Path, b.Path) })
	return sorted
}

func verifyQualifiedFile(ctx context.Context, root string, entry qualifiedFile) error {
	if !qualifiedFilePattern.MatchString(entry.Path) || !workerSHA256Pattern.MatchString(entry.SHA256) || entry.Bytes <= 0 {
		return errors.New("Qualification entries require flat artifact filenames, exact SHA-256 digests, and positive byte counts.")
	}
	file, err := openWorkerArtifact(root, entry.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	bytes, err := io.Copy(hash, io.LimitReader(artifactReader{context: ctx, reader: file}, entry.Bytes+1))
	if err != nil || bytes != entry.Bytes || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return errors.New("A qualification input, evidence file, or release artifact changed. Qualify the exact candidate bytes before applying.")
	}
	return nil
}

func readQualifiedDocument(ctx context.Context, root string, entry qualifiedFile, value any) error {
	if entry.Bytes > maximumWorkerManifestBytes {
		return errors.New("The qualification document exceeds 2 MiB.")
	}
	if err := verifyQualifiedFile(ctx, root, entry); err != nil {
		return err
	}
	file, err := openWorkerArtifact(root, entry.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	content, err := readWorkerManifestBytes(file)
	if err != nil || workerDigest(content) != entry.SHA256 || int64(len(content)) != entry.Bytes || json.Unmarshal(content, value) != nil {
		return errors.New("The pinned qualification document is not valid immutable metadata.")
	}
	return nil
}

func verifyWorkerQualification(ctx context.Context, model workerReleaseModel, manifest workerReleaseManifest) error {
	path := model.QualificationFile.ValueString()
	file, err := os.Open(path)
	if err != nil {
		return errors.New("The qualification receipt could not be opened. Supply the receipt for these exact release bytes.")
	}
	defer file.Close()
	content, err := readWorkerManifestBytes(file)
	if err != nil || workerDigest(content) != model.QualificationSHA256.ValueString() {
		return errors.New("The qualification receipt does not match qualification_sha256.")
	}
	var qualification workerQualification
	if json.Unmarshal(content, &qualification) != nil || qualification.FormatVersion != 1 || qualification.Version != manifest.Version || qualification.PublicCommit != manifest.PublicCommit {
		return errors.New("The qualification receipt does not identify this installation's source and version.")
	}
	if qualification.Artifacts.Path != "artifacts.json" || qualification.Installation.Path != "installation.json" || qualification.Installation.SHA256 != model.ReleaseManifestSHA256.ValueString() {
		return errors.New("Qualification must pin this exact installation.json and its artifacts.json inventory.")
	}
	root := filepath.Dir(path)
	var inventory workerArtifactReceipt
	if err := readQualifiedDocument(ctx, root, qualification.Artifacts, &inventory); err != nil {
		return err
	}
	if inventory.FormatVersion != 1 || inventory.Version != manifest.Version || inventory.PublicCommit != manifest.PublicCommit || inventory.Repository != manifest.Repository || len(inventory.Assets) == 0 {
		return errors.New("The qualified artifact inventory disagrees with this installation's source and version.")
	}
	seen := map[string]bool{}
	hasInstallation := false
	for _, asset := range inventory.Assets {
		if seen[asset.Name] {
			return errors.New("The qualified artifact inventory contains duplicate paths.")
		}
		seen[asset.Name] = true
		if err := verifyQualifiedFile(ctx, root, qualifiedFile{Path: asset.Name, SHA256: asset.SHA256, Bytes: asset.Bytes}); err != nil {
			return err
		}
		if asset.Name == qualification.Installation.Path {
			hasInstallation = asset.SHA256 == qualification.Installation.SHA256 && asset.Bytes == qualification.Installation.Bytes
		}
	}
	if !hasInstallation {
		return errors.New("The qualified inventory does not contain this exact installation manifest.")
	}
	if len(qualification.Inputs) != 4 {
		return errors.New("Qualification must declare exactly toolchain, fixture, policy, and configuration input categories.")
	}
	for _, category := range []string{"toolchain", "fixtures", "policy", "configuration"} {
		entries := qualification.Inputs[category]
		if len(entries) == 0 {
			return errors.New("Qualification must bind nonempty toolchain, fixture, policy, and configuration inputs.")
		}
		for _, entry := range entries {
			if err := verifyQualifiedFile(ctx, root, entry); err != nil {
				return err
			}
		}
	}
	checks := map[string]bool{}
	inputsDigest, err := qualificationInputsDigest(qualification.Inputs)
	if err != nil {
		return err
	}
	for _, check := range qualification.Checks {
		if check.Name == "" || checks[check.Name] || check.Outcome != "passed" {
			return errors.New("Qualification requires distinct successful checks with digest-bound evidence.")
		}
		var evidence workerCheckEvidence
		if err := readQualifiedDocument(ctx, root, check.Evidence, &evidence); err != nil {
			return err
		}
		if evidence.FormatVersion != 1 || evidence.Name != check.Name || evidence.Outcome != "passed" || evidence.PublicCommit != qualification.PublicCommit || evidence.Artifacts != qualification.Artifacts || evidence.Installation != qualification.Installation || evidence.InputsSHA256 != inputsDigest {
			return errors.New("A qualification check report belongs to different source, artifacts, installation metadata, or inputs. Qualify this exact candidate before applying.")
		}
		checks[check.Name] = true
	}
	for _, name := range []string{"complete", "artifact-scenarios", "shipped-formats"} {
		if !checks[name] {
			return errors.New("Qualification must include complete, artifact-scenarios, and shipped-formats checks.")
		}
	}
	return nil
}
