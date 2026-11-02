package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"github.com/cloudflare/cloudflare-go/v7/option"
)

type workerConsoleArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type workerAssetConfiguration struct {
	BasePath         string `json:"base_path"`
	HTMLHandling     string `json:"html_handling"`
	NotFoundHandling string `json:"not_found_handling"`
	RunWorkerFirst   bool   `json:"run_worker_first"`
}

type workerAssets struct {
	JWT    string                   `json:"jwt,omitempty"`
	Config workerAssetConfiguration `json:"config"`
}

func consoleAssetConfiguration() workerAssetConfiguration {
	return workerAssetConfiguration{BasePath: "/", HTMLHandling: "none", NotFoundHandling: "none", RunWorkerFirst: true}
}

type workerConsoleFile struct {
	workerConsoleArtifact
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"`
}

type workerAssetEntry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}
type workerAssetSession struct {
	JWT     string     `json:"jwt"`
	Buckets [][]string `json:"buckets"`
}

var consoleAssetPath = regexp.MustCompile(`^/console/[A-Za-z0-9_/-]+\.[A-Za-z0-9]+$`)

// The immutable manifest binds every UI byte to the same release as the Worker.
func readWorkerConsole(model workerReleaseModel) ([]workerConsoleFile, error) {
	manifest, err := readWorkerManifest(model)
	if err != nil {
		return nil, err
	}
	artifact := manifest.Console
	if artifact == nil || artifact.Path != "console.json" || !workerSHA256Pattern.MatchString(artifact.SHA256) || artifact.Bytes <= 0 || artifact.Bytes > 64*1024*1024 {
		return nil, errors.New("This release needs a verified console.json artifact in its installation manifest.")
	}
	file, err := os.Open(filepath.Join(filepath.Dir(model.ReleaseManifestFile.ValueString()), artifact.Path))
	if err != nil {
		return nil, errors.New("The console artifact is missing. Restore the immutable release module.")
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, artifact.Bytes+1))
	if err != nil || int64(len(content)) != artifact.Bytes || workerDigest(content) != artifact.SHA256 {
		return nil, errors.New("The console artifact differs from the installation manifest.")
	}
	var bundle struct {
		Files []workerConsoleFile `json:"files"`
	}
	if json.Unmarshal(content, &bundle) != nil || len(bundle.Files) == 0 || len(bundle.Files) > 1000 {
		return nil, errors.New("The console artifact has an invalid file inventory.")
	}
	paths := map[string]bool{}
	for _, asset := range bundle.Files {
		body, err := base64.StdEncoding.Strict().DecodeString(asset.ContentBase64)
		if !consoleAssetPath.MatchString(asset.Path) || paths[asset.Path] || asset.ContentType == "" || err != nil || int64(len(body)) != asset.Bytes || workerDigest(body) != asset.SHA256 {
			return nil, errors.New("A console file has an unsafe path, duplicate name, or mismatched digest.")
		}
		paths[asset.Path] = true
	}
	if !paths["/console/index.html"] {
		return nil, errors.New("The console artifact has no index.html.")
	}
	return bundle.Files, nil
}

func (r *workerReleaseResource) uploadWorkerAssets(ctx context.Context, model workerReleaseModel) (string, error) {
	files, err := readWorkerConsole(model)
	if err != nil {
		return "", err
	}
	manifest := map[string]workerAssetEntry{}
	byHash := map[string]workerConsoleFile{}
	for _, file := range files {
		hash := workerDigest([]byte(file.ContentBase64 + file.ContentType))[:32]
		manifest[file.Path] = workerAssetEntry{Hash: hash, Size: file.Bytes}
		byHash[hash] = file
	}
	payload, err := json.Marshal(struct {
		Manifest map[string]workerAssetEntry `json:"manifest"`
	}{manifest})
	if err != nil {
		return "", errors.New("The console asset manifest could not be encoded.")
	}
	var session workerEnvelope[workerAssetSession]
	path := fmt.Sprintf("accounts/%s/workers/scripts/%s/assets-upload-session", url.PathEscape(model.AccountID.ValueString()), url.PathEscape(model.WorkerName.ValueString()))
	err = r.client.Post(ctx, path, nil, &session, option.WithRequestBody("application/json", payload))
	if err != nil {
		return "", errors.New(describeCloudflareError(err))
	}
	if !session.Success || session.Result.JWT == "" {
		return "", errors.New("Cloudflare did not create the console asset upload session.")
	}
	if len(session.Result.Buckets) == 0 {
		return session.Result.JWT, nil
	}
	completion := ""
	for _, bucket := range session.Result.Buckets {
		contentType, body, err := workerAssetMultipart(bucket, byHash)
		if err != nil {
			return "", err
		}
		var uploaded workerEnvelope[struct {
			JWT string `json:"jwt"`
		}]
		path := fmt.Sprintf("accounts/%s/workers/assets/upload", url.PathEscape(model.AccountID.ValueString()))
		err = r.client.Post(ctx, path, nil, &uploaded, option.WithQuery("base64", "true"), option.WithHeader("Authorization", "Bearer "+session.Result.JWT), option.WithRequestBody(contentType, body))
		if err != nil {
			return "", errors.New("Cloudflare could not upload the console assets. Retry the apply to resume content-addressed uploads.")
		}
		if !uploaded.Success {
			return "", errors.New("Cloudflare rejected the console asset upload.")
		}
		if uploaded.Result.JWT != "" {
			completion = uploaded.Result.JWT
		}
	}
	if completion == "" {
		return "", errors.New("Cloudflare did not confirm the complete console asset upload.")
	}
	return completion, nil
}

func workerAssetMultipart(hashes []string, files map[string]workerConsoleFile) (string, []byte, error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for _, hash := range hashes {
		file, exists := files[hash]
		if !exists {
			return "", nil, errors.New("Cloudflare requested a console asset outside the verified manifest.")
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, hash, hash))
		header.Set("Content-Type", file.ContentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return "", nil, errors.New("The console upload part could not be created.")
		}
		if _, err := part.Write([]byte(file.ContentBase64)); err != nil {
			return "", nil, errors.New("The console upload part could not be encoded.")
		}
	}
	if writer.Close() != nil {
		return "", nil, errors.New("The console upload could not be finalized.")
	}
	return writer.FormDataContentType(), buffer.Bytes(), nil
}
