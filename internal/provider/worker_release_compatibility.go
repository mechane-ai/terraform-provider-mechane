package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/cloudflare/cloudflare-go/v7/option"
)

type workerAdoptionBaseline struct {
	WorkerVersionID string        `json:"worker_version_id"`
	BundleSHA256    string        `json:"bundle_sha256"`
	ReleaseVersion  string        `json:"release_version"`
	Formats         workerFormats `json:"formats"`
}

func decodeWorkerAdoptionBaseline(value string) (workerAdoptionBaseline, error) {
	var baseline workerAdoptionBaseline
	if value == "" || len(value) > maximumWorkerManifestBytes {
		return baseline, errors.New("An unmarked live Worker requires a reviewed adoption_baseline identifying its exact version, bundle digest, release version, and complete compatibility maps.")
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&baseline) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return baseline, errors.New("The adoption baseline is not valid adoption metadata.")
	}
	if baseline.WorkerVersionID == "" || !workerSHA256Pattern.MatchString(baseline.BundleSHA256) {
		return baseline, errors.New("The adoption baseline must identify the observed Worker version ID and its complete bundle SHA-256.")
	}
	if _, err := parseWorkerReleaseVersion(baseline.ReleaseVersion); err != nil {
		return baseline, err
	}
	return baseline, validateWorkerFormats(baseline.Formats)
}

func preserveWorkerFormats(previous, desired workerFormats) error {
	for _, pair := range []struct {
		previous map[string][]string
		desired  map[string][]string
	}{{previous.Wire, desired.Wire}, {previous.Migrations, desired.Migrations}} {
		for category, entries := range pair.previous {
			candidates, exists := pair.desired[category]
			if !exists {
				return errors.New("The release removes a recorded wire or migration category. Preserve every existing category and append new entries instead.")
			}
			allowed := make(map[string]bool, len(candidates))
			for _, candidate := range candidates {
				allowed[candidate] = true
			}
			for _, entry := range entries {
				if !allowed[entry] {
					return errors.New("The release removes or changes a recorded wire or migration entry. Preserve every exact existing entry and append new entries instead.")
				}
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
		if err = checkWorkerCompatibility(ctx, model, desired, live); err != nil {
			return "", err
		}
	}
	if !active {
		return "", errors.New("The Worker has deployment metadata without an active version. Restore an identifiable live release before applying.")
	}
	return deployment.ID, nil
}

func checkWorkerCompatibility(ctx context.Context, model workerReleaseModel, desired workerReleaseManifest, live workerVersion) error {
	configuration, err := live.configuration()
	if err != nil {
		return err
	}
	marker, marked, err := parseWorkerReleaseMarker(live.Annotations["workers/message"])
	if err != nil {
		return err
	}
	var previous workerFormats
	var previousVersion string
	if marked {
		fingerprint, err := configuration.fingerprint()
		if err != nil || fingerprint != marker.Fingerprint {
			return errors.New("The live Worker's native release marker disagrees with its actual bytes or configuration. Adoption cannot override this mismatch.")
		}
		manifest, err := downloadWorkerManifest(ctx, desired.Repository, configuration)
		if err != nil {
			return err
		}
		previous, previousVersion = manifest.Formats, manifest.Version
	} else {
		baseline, err := decodeWorkerAdoptionBaseline(model.AdoptionBaseline.ValueString())
		if err != nil {
			return err
		}
		if baseline.WorkerVersionID != live.ID || baseline.BundleSHA256 != configuration.BundleSHA256 {
			return errors.New("The adoption baseline does not match the currently active Worker version and actual bundle bytes. Review a new baseline before applying.")
		}
		if _, err := parseWorkerReleaseVersion(configuration.ReleaseVersion); err == nil && baseline.ReleaseVersion != configuration.ReleaseVersion {
			return errors.New("The adoption baseline release version does not match the live Worker's release tag.")
		}
		previous, previousVersion = baseline.Formats, baseline.ReleaseVersion
	}
	if err = rejectWorkerDowngrade(previousVersion, desired.Version); err != nil {
		return err
	}
	return preserveWorkerFormats(previous, desired.Formats)
}
