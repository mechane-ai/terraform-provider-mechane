package provider

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/validate"
)

const maximumArchiveBytes int64 = 8 << 30
const maximumMetadataBytes int64 = 1 << 20

type archiveReader struct {
	context context.Context
	reader  io.Reader
}

func (r archiveReader) Read(buffer []byte) (int, error) {
	if err := r.context.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func downloadImageArchive(ctx context.Context, config imageModel) (string, func(), error) {
	directory, err := os.MkdirTemp("", "mechane-image-")
	if err != nil {
		return "", nil, errors.New("Could not create temporary image storage.")
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	archivePath := filepath.Join(directory, "image.oci.tar")
	err = downloadArchive(ctx, config, archivePath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return archivePath, cleanup, nil
}

func downloadArchive(ctx context.Context, config imageModel, archivePath string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, config.SourceURL.ValueString(), nil)
	if err != nil {
		return errors.New("The release archive URL is invalid.")
	}
	client := &http.Client{
		Timeout: 2 * time.Hour,
		CheckRedirect: func(request *http.Request, previous []*http.Request) error {
			if request.URL.Scheme != "https" || len(previous) >= 10 {
				return errors.New("Release downloads require HTTPS and at most ten redirects.")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Could not download the release archive. Check the public HTTPS release URL and connectivity.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("The release archive URL did not return HTTP 200.")
	}
	archive, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("Could not write the release archive to temporary storage.")
	}
	defer archive.Close()
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(archive, digest), io.LimitReader(response.Body, maximumArchiveBytes+1))
	if err != nil {
		return errors.New("The release archive download was interrupted.")
	}
	if written > maximumArchiveBytes {
		return errors.New("The release archive exceeds the 8 GiB installation limit.")
	}
	if hex.EncodeToString(digest.Sum(nil)) != config.SourceSHA256.ValueString() {
		return errors.New("The release archive SHA-256 does not match the release manifest. No image was uploaded.")
	}
	return nil
}

func readArchiveImage(ctx context.Context, archivePath, expectedDigest string) (v1.Image, error) {
	layoutPath := filepath.Join(filepath.Dir(archivePath), "layout")
	if err := unpackArchive(ctx, archivePath, layoutPath); err != nil {
		return nil, err
	}
	index, err := layout.ImageIndexFromPath(layoutPath)
	if err != nil {
		return nil, errors.New("The release archive is not an OCI image layout.")
	}
	manifest, err := index.IndexManifest()
	if err != nil || len(manifest.Manifests) != 1 || manifest.Manifests[0].Digest.String() != expectedDigest {
		return nil, errors.New("The OCI archive must contain exactly the image digest declared by the release manifest.")
	}
	if !isMetadataBlob(layoutPath, manifest.Manifests[0].Digest.Hex) {
		return nil, errors.New("The image manifest exceeds the 1 MiB metadata limit.")
	}
	image, err := index.Image(manifest.Manifests[0].Digest)
	if err != nil {
		return nil, errors.New("The release archive is missing its declared image.")
	}
	imageManifest, err := image.Manifest()
	if err != nil || !isMetadataBlob(layoutPath, imageManifest.Config.Digest.Hex) {
		return nil, errors.New("The image configuration is missing or exceeds the 1 MiB metadata limit.")
	}
	configuration, err := image.ConfigFile()
	if err != nil || configuration.OS != "linux" || configuration.Architecture != "amd64" {
		return nil, errors.New("The release image must target Linux amd64.")
	}
	// Extraction verifies every compressed blob against its filename. Fast validation
	// checks OCI metadata without executing an unbounded layer decompression.
	if err := validate.Image(image, validate.Fast); err != nil {
		return nil, errors.New("The release image contains a corrupt manifest, configuration, or layer. No image was uploaded.")
	}
	return image, nil
}

func isMetadataBlob(layoutPath, digest string) bool {
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(digest) {
		return false
	}
	metadata, err := os.Stat(filepath.Join(layoutPath, "blobs", "sha256", digest))
	return err == nil && metadata.Size() <= maximumMetadataBytes
}

func unpackArchive(ctx context.Context, archivePath, destination string) error {
	if err := os.Mkdir(destination, 0700); err != nil {
		return errors.New("Could not create temporary OCI layout storage.")
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return errors.New("Could not open temporary OCI layout storage.")
	}
	defer root.Close()
	archive, err := os.Open(archivePath)
	if err != nil {
		return errors.New("Could not open the downloaded image archive.")
	}
	defer archive.Close()
	reader := tar.NewReader(archiveReader{context: ctx, reader: archive})
	blobPath := regexp.MustCompile(`^blobs/sha256/[a-f0-9]{64}$`)
	var totalBytes int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.New("The release image archive is truncated or malformed.")
		}
		entry := path.Clean(header.Name)
		if header.Typeflag == tar.TypeDir && (entry == "." || entry == "blobs" || entry == "blobs/sha256") {
			continue
		}
		if header.Typeflag != tar.TypeReg || (entry != "oci-layout" && entry != "index.json" && !blobPath.MatchString(entry)) {
			return errors.New("The release archive contains a link or a path outside the OCI layout.")
		}
		totalBytes += header.Size
		if header.Size < 0 || totalBytes > maximumArchiveBytes {
			return errors.New("The unpacked image exceeds the 8 GiB installation limit.")
		}
		if (entry == "index.json" || entry == "oci-layout") && header.Size > maximumMetadataBytes {
			return errors.New("The OCI layout metadata exceeds the 1 MiB metadata limit.")
		}
		if err := root.MkdirAll(path.Dir(entry), 0700); err != nil {
			return errors.New("Could not create an OCI blob directory.")
		}
		file, err := root.OpenFile(entry, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("The image archive repeats an entry or cannot be extracted.")
		}
		digest := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(file, digest), reader)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return errors.New("Could not extract a complete OCI blob.")
		}
		if strings.HasPrefix(entry, "blobs/") && hex.EncodeToString(digest.Sum(nil)) != path.Base(entry) {
			return errors.New("An OCI blob does not match its declared SHA-256 digest. No image was uploaded.")
		}
	}
}
