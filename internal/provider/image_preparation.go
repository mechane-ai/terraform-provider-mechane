package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/containers"
)

func (r *containerImageResource) prepareImage(ctx context.Context, config imageModel) error {
	image := imageReference(config)
	for {
		prepared, err := r.client.Containers.Images.Prepare(ctx, containers.ImagePrepareParams{
			AccountID: cloudflare.F(config.AccountID.ValueString()),
			Image:     cloudflare.F(image),
		})
		if err != nil {
			return errors.New(describeCloudflareError(err))
		}
		if prepared.Image != image {
			return errors.New("Cloudflare returned preparation for a different image. No ready image was accepted.")
		}
		switch prepared.Status {
		case containers.ImagePrepareResponseStatusReady:
			if prepared.ArtifactDigest == "" {
				return errors.New("Cloudflare reported a ready image without its prepared runtime artifact digest.")
			}
			return nil
		case containers.ImagePrepareResponseStatusError:
			return fmt.Errorf("Cloudflare could not prepare the container image: %s", prepared.Reason)
		case containers.ImagePrepareResponseStatusPending:
		default:
			return errors.New("Cloudflare returned an unknown image preparation status. No ready image was accepted.")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Container image preparation did not finish: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
