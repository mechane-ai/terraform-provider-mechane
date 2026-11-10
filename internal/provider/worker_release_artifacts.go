package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const maximumWorkerModuleBytes = 64 * 1024 * 1024
const maximumWorkerAssetBytes = 25 * 1024 * 1024
const maximumWorkerAssetsBytes = 512 * 1024 * 1024

type verifiedWorkerAsset struct {
	Hash        string
	ContentType string
	Content     []byte
}

type verifiedWorkerArtifacts struct {
	Modules  []workerModule
	Manifest map[string]workerAssetEntry
	Assets   map[string]verifiedWorkerAsset
}

type workerAssetEntry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

func validWorkerArtifactPath(path string) bool {
	return fs.ValidPath(path) && path != "." && !strings.ContainsAny(path, "\\\r\n\x00")
}

func validateWorkerArtifact(artifact workerArtifact, maximumBytes int64) error {
	if !validWorkerArtifactPath(artifact.Path) || !workerSHA256Pattern.MatchString(artifact.SHA256) || artifact.Bytes < 0 || artifact.Bytes > maximumBytes {
		return errors.New("Release artifacts require safe relative paths, exact SHA-256 digests, and bounded byte counts.")
	}
	if _, _, err := mime.ParseMediaType(artifact.ContentType); err != nil || strings.ContainsAny(artifact.ContentType, "\r\n") {
		return errors.New("Release artifact content types must be valid media types.")
	}
	return nil
}

func validateWorkerArtifacts(manifest workerReleaseManifest) error {
	if len(manifest.Worker.Modules) == 0 || manifest.Worker.Assets == nil || manifest.Worker.Triggers == nil || len(manifest.Worker.Triggers.Crons) == 0 {
		return errors.New("The installation must inventory every Worker module, Console asset, and control-delivery schedule.")
	}
	seen := map[string]bool{}
	hasMain := false
	for _, module := range manifest.Worker.Modules {
		if err := validateWorkerArtifact(module, maximumWorkerModuleBytes); err != nil {
			return err
		}
		if seen[module.Path] {
			return errors.New("Worker module paths must be distinct.")
		}
		seen[module.Path] = true
		if module.Path == manifest.Worker.Path {
			if module.SHA256 != manifest.Worker.SHA256 || module.Bytes != manifest.Worker.Bytes || module.ContentType != "application/javascript+module" {
				return errors.New("The main Worker module disagrees with its primary artifact metadata.")
			}
			hasMain = true
		}
	}
	assets := manifest.Worker.Assets
	if !hasMain || !validWorkerArtifactPath(assets.Directory) || assets.Binding != "ASSETS" || len(assets.Files) == 0 || assets.Config.BasePath != "" || assets.Config.HTMLHandling != "none" || assets.Config.NotFoundHandling != "none" || !assets.Config.RunWorkerFirst {
		return errors.New("The application requires its primary Worker module and a Worker-first ASSETS binding with explicit Console routing.")
	}
	seen = map[string]bool{}
	var totalBytes int64
	for _, asset := range assets.Files {
		if err := validateWorkerArtifact(asset, maximumWorkerAssetBytes); err != nil {
			return err
		}
		if seen[asset.Path] {
			return errors.New("Console asset paths must be distinct.")
		}
		seen[asset.Path] = true
		totalBytes += asset.Bytes
	}
	if totalBytes > maximumWorkerAssetsBytes {
		return errors.New("Console assets exceed the 512 MiB local upload limit.")
	}
	return nil
}

func workerAssetsDigest(assets workerAssetManifest) (string, error) {
	assets.Files = slices.Clone(assets.Files)
	slices.SortFunc(assets.Files, func(a, b workerArtifact) int { return strings.Compare(a.Path, b.Path) })
	content, err := json.Marshal(assets)
	if err != nil {
		return "", errors.New("The Console asset inventory could not be encoded.")
	}
	return workerDigest(content), nil
}

func openWorkerArtifact(root, path string) (*os.File, error) {
	if !validWorkerArtifactPath(path) {
		return nil, errors.New("The release contains an unsafe artifact path.")
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("The immutable release artifact directory could not be opened.")
	}
	defer base.Close()
	file, err := base.Open(filepath.FromSlash(path))
	if err != nil {
		return nil, errors.New("An inventoried release artifact could not be opened within its release directory.")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("Every inventoried release artifact must be a regular file.")
	}
	return file, nil
}

func readWorkerArtifact(ctx context.Context, root string, artifact workerArtifact, maximumBytes int64) ([]byte, error) {
	file, err := openWorkerArtifact(root, artifact.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(artifactReader{context: ctx, reader: file}, maximumBytes+1))
	if err != nil || int64(len(content)) != artifact.Bytes || workerDigest(content) != artifact.SHA256 {
		return nil, errors.New("Release artifact bytes differ from the verified digest or byte count. Restore the qualified bytes before applying.")
	}
	return content, nil
}

func readWorkerArtifacts(ctx context.Context, model workerReleaseModel, manifest workerReleaseManifest) (verifiedWorkerArtifacts, error) {
	root := filepath.Dir(model.ReleaseManifestFile.ValueString())
	artifacts := verifiedWorkerArtifacts{Manifest: map[string]workerAssetEntry{}, Assets: map[string]verifiedWorkerAsset{}}
	for _, module := range manifest.Worker.Modules {
		content, err := readWorkerArtifact(ctx, root, module, maximumWorkerModuleBytes)
		if err != nil {
			return artifacts, err
		}
		artifacts.Modules = append(artifacts.Modules, workerModule{Name: module.Path, ContentType: module.ContentType, ContentBase64: base64.StdEncoding.EncodeToString(content)})
	}
	for _, asset := range manifest.Worker.Assets.Files {
		content, err := readWorkerArtifact(ctx, root, workerArtifact{Path: manifest.Worker.Assets.Directory + "/" + asset.Path, ContentType: asset.ContentType, SHA256: asset.SHA256, Bytes: asset.Bytes}, maximumWorkerAssetBytes)
		if err != nil {
			return artifacts, err
		}
		// Cloudflare accepts a 32-character hexadecimal file identity. Include the media type because it is stored with the bytes.
		hash := workerDigest(append(append([]byte(asset.ContentType), 0), content...))[:32]
		if existing, found := artifacts.Assets[hash]; found && (existing.ContentType != asset.ContentType || workerDigest(existing.Content) != asset.SHA256) {
			return artifacts, errors.New("Distinct Console assets collided in their Cloudflare upload identity.")
		}
		artifacts.Manifest["/"+asset.Path] = workerAssetEntry{Hash: hash, Size: asset.Bytes}
		artifacts.Assets[hash] = verifiedWorkerAsset{Hash: hash, ContentType: asset.ContentType, Content: content}
	}
	return artifacts, nil
}
