package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/containers"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

func (r *containerImageResource) registryOptions(ctx context.Context, accountID string, push bool) ([]remote.Option, error) {
	permissions := []containers.RegistryCredentialGenerateParamsPermission{containers.RegistryCredentialGenerateParamsPermissionPull}
	if push {
		permissions = append(permissions, containers.RegistryCredentialGenerateParamsPermissionPush)
	}
	credentials, err := r.client.Containers.Registries.Credentials.Generate(ctx, "registry.cloudflare.com", containers.RegistryCredentialGenerateParams{
		AccountID:         cloudflare.F(accountID),
		ExpirationMinutes: cloudflare.F(int64(120)),
		Permissions:       cloudflare.F(permissions),
	})
	if err != nil {
		return nil, errors.New(describeCloudflareError(err))
	}
	if credentials.RegistryHost != "registry.cloudflare.com" || credentials.Username == "" || credentials.Password == "" {
		return nil, errors.New("Cloudflare did not return credentials for its managed registry.")
	}
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuth(&authn.Basic{Username: credentials.Username, Password: credentials.Password}),
		remote.WithUserAgent("terraform-provider-mechane"),
	}, nil
}

func (r *containerImageResource) readImage(ctx context.Context, state imageModel) (bool, error) {
	options, err := r.registryOptions(ctx, state.AccountID.ValueString(), false)
	if err != nil {
		return false, err
	}
	return readRegistryImage(state, options)
}

func readRegistryImage(config imageModel, options []remote.Option) (bool, error) {
	reference, err := name.NewDigest(imageReference(config), name.StrictValidation)
	if err != nil {
		return false, errors.New("The configured registry image reference is invalid.")
	}
	descriptor, err := remote.Head(reference, options...)
	var registryError *transport.Error
	if errors.As(err, &registryError) && registryError.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, describeRegistryError(err)
	}
	if descriptor.Digest.String() != config.Digest.ValueString() {
		return false, errors.New("The registry returned an image with a different digest. No application was changed.")
	}
	return true, nil
}

func (r *containerImageResource) copyImage(ctx context.Context, config imageModel) error {
	options, err := r.registryOptions(ctx, config.AccountID.ValueString(), true)
	if err != nil {
		return err
	}
	found, err := readRegistryImage(config, options)
	if err != nil || found {
		return err
	}
	archive, cleanup, err := downloadImageArchive(ctx, config)
	if err != nil {
		return err
	}
	defer cleanup()
	image, err := readArchiveImage(ctx, archive, config.Digest.ValueString())
	if err != nil {
		return err
	}
	tagName := "registry.cloudflare.com/" + config.AccountID.ValueString() + "/" + config.Repository.ValueString() + ":sha256-" + strings.TrimPrefix(config.Digest.ValueString(), "sha256:")
	tag, err := name.NewTag(tagName, name.StrictValidation)
	if err != nil {
		return errors.New("The destination registry repository is invalid.")
	}
	if err := remote.Write(tag, image, options...); err != nil {
		return describeRegistryError(err)
	}
	found, err = readRegistryImage(config, options)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("The registry did not return the uploaded image digest. Retry to resume the content-addressed upload.")
	}
	return nil
}

func imageReference(config imageModel) string {
	return "registry.cloudflare.com/" + config.AccountID.ValueString() + "/" + config.Repository.ValueString() + "@" + config.Digest.ValueString()
}

func describeRegistryError(err error) error {
	var registryError *transport.Error
	if errors.As(err, &registryError) {
		return fmt.Errorf("The container registry returned HTTP %d. Check Containers registry access before retrying.", registryError.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.New(describeCloudflareError(err))
	}
	return errors.New("The container registry request failed. Check connectivity and registry access before retrying.")
}
