package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"strconv"

	"github.com/cloudflare/cloudflare-go/v7/option"
)

type workerAssets struct {
	JWT    string            `json:"jwt,omitempty"`
	Config workerAssetConfig `json:"config"`
}

type workerAssetSession struct {
	JWT     string     `json:"jwt"`
	Buckets [][]string `json:"buckets"`
}

func (r *workerReleaseResource) uploadWorkerAssets(ctx context.Context, model workerReleaseModel, artifacts verifiedWorkerArtifacts) (string, error) {
	payload, err := json.Marshal(struct {
		Manifest map[string]workerAssetEntry `json:"manifest"`
	}{Manifest: artifacts.Manifest})
	if err != nil {
		return "", errors.New("The Console asset upload manifest could not be encoded.")
	}
	path := fmt.Sprintf("accounts/%s/workers/scripts/%s/assets-upload-session", url.PathEscape(model.AccountID.ValueString()), url.PathEscape(model.WorkerName.ValueString()))
	var session workerEnvelope[workerAssetSession]
	if err := r.client.Post(ctx, path, nil, &session, option.WithRequestBody("application/json", payload)); err != nil {
		return "", errors.New(describeCloudflareError(err))
	}
	if !session.Success || session.Result.JWT == "" {
		return "", errors.New("Cloudflare did not return a Console asset upload session.")
	}
	if len(session.Result.Buckets) == 0 {
		return session.Result.JWT, nil
	}
	seen := map[string]bool{}
	for _, bucket := range session.Result.Buckets {
		if len(bucket) == 0 {
			return "", errors.New("Cloudflare returned an empty Console asset upload bucket.")
		}
		for _, hash := range bucket {
			if _, found := artifacts.Assets[hash]; !found || seen[hash] {
				return "", errors.New("Cloudflare requested an unknown or repeated Console asset hash.")
			}
			seen[hash] = true
		}
	}
	completion := ""
	for _, bucket := range session.Result.Buckets {
		body, contentType, err := encodeWorkerAssetBucket(bucket, artifacts.Assets)
		if err != nil {
			return "", err
		}
		var uploaded workerEnvelope[struct {
			JWT string `json:"jwt"`
		}]
		path := fmt.Sprintf("accounts/%s/workers/assets/upload", url.PathEscape(model.AccountID.ValueString()))
		err = r.client.Post(ctx, path, nil, &uploaded, option.WithQuery("base64", "true"), option.WithAPIToken(session.Result.JWT), option.WithRequestBody(contentType, body))
		if err != nil {
			return "", errors.New(describeCloudflareError(err))
		}
		if !uploaded.Success {
			return "", errors.New("Cloudflare did not accept a Console asset upload bucket.")
		}
		completion = uploaded.Result.JWT
	}
	if completion == "" {
		return "", errors.New("Cloudflare did not confirm the complete Console asset set. No Worker version was uploaded.")
	}
	return completion, nil
}

func encodeWorkerAssetBucket(hashes []string, assets map[string]verifiedWorkerAsset) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, hash := range hashes {
		asset := assets[hash]
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", "form-data; name="+strconv.Quote(hash)+"; filename="+strconv.Quote(hash))
		header.Set("Content-Type", asset.ContentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", errors.New("The Console asset multipart request could not be created.")
		}
		encoder := base64.NewEncoder(base64.StdEncoding, part)
		if _, err := encoder.Write(asset.Content); err != nil {
			return nil, "", errors.New("The Console asset bytes could not be encoded.")
		}
		if err := encoder.Close(); err != nil {
			return nil, "", errors.New("The Console asset encoding could not be completed.")
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", errors.New("The Console asset multipart request could not be completed.")
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}
