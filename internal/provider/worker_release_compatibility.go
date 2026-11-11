package provider

import (
	"context"
	"errors"

	"github.com/cloudflare/cloudflare-go/v7/option"
)

func preserveWorkerFormats(previous, desired workerFormats) error {
	for category, entries := range previous.Migrations {
		candidates, exists := desired.Migrations[category]
		if !exists {
			return errors.New("The release removes an applied migration category. Preserve applied migration checksums and append new steps instead.")
		}
		allowed := make(map[string]bool, len(candidates))
		for _, candidate := range candidates {
			allowed[candidate] = true
		}
		for _, entry := range entries {
			if !allowed[entry] {
				return errors.New("The release removes or changes an applied migration. Preserve its exact checksum and append a new step instead.")
			}
		}
	}
	return nil
}

func rejectWorkerDowngrade(previous, desired string) error {
	oldVersion, err := parseWorkerReleaseVersion(previous)
	if err != nil {
		return err
	}
	newVersion, err := parseWorkerReleaseVersion(desired)
	if err != nil {
		return err
	}
	if newVersion.LessThan(oldVersion) {
		return errors.New("Worker release downgrades are not supported. Publish compatible recovery code as a newer release.")
	}
	return nil
}

func (r *workerReleaseResource) preflightWorkerCompatibility(ctx context.Context, model workerReleaseModel, desired workerReleaseManifest) (string, error) {
	deployment, err := r.getWorkerDeployment(ctx, model)
	if err != nil {
		return "", errors.New(describeCloudflareError(err))
	}
	if deployment.ID == "" && len(deployment.Versions) == 0 {
		var history workerEnvelope[[]workerVersion]
		err := r.client.Get(ctx, workerVersionPath(model), nil, &history, option.WithQuery("per_page", "1"))
		if err != nil {
			return "", errors.New(describeCloudflareError(err))
		}
		if !history.Success || len(history.Result) != 0 {
			return "", errors.New("A Worker without an active deployment must also have no version history to qualify as a fresh installation. Restore an identifiable live release before applying.")
		}
		return "", nil
	}
	active := false
	for _, traffic := range deployment.Versions {
		if traffic.Percentage <= 0 {
			continue
		}
		active = true
		live, err := r.getWorkerVersion(ctx, model, traffic.VersionID)
		if err != nil {
			return "", errors.New(describeCloudflareError(err))
		}
		if err = checkWorkerCompatibility(ctx, desired, live); err != nil {
			return "", err
		}
	}
	if !active {
		return "", errors.New("The Worker has deployment metadata without an active version. Restore an identifiable live release before applying.")
	}
	return deployment.ID, nil
}

func checkWorkerCompatibility(ctx context.Context, desired workerReleaseManifest, live workerVersion) error {
	configuration, err := live.configuration()
	if err != nil {
		return err
	}
	marker, marked, err := parseWorkerReleaseMarker(live.Annotations["workers/message"])
	if err != nil {
		return err
	}
	if !marked {
		return errors.New("The active Worker has no verified release marker. Deploy a new owned compute service instead of adopting an unmarked installation.")
	}
	fingerprint, err := configuration.fingerprint()
	if err != nil || fingerprint != marker.Fingerprint {
		return errors.New("The live Worker's release marker disagrees with its actual bytes or configuration.")
	}
	manifest, err := downloadWorkerManifest(ctx, desired.Repository, configuration)
	if err != nil {
		return err
	}
	if err = rejectWorkerDowngrade(manifest.Version, desired.Version); err != nil {
		return err
	}
	return preserveWorkerFormats(manifest.Formats, desired.Formats)
}
