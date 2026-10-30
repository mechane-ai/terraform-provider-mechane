package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/durable_objects"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type workerModule struct {
	Name          string `json:"name"`
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"`
}

type workerLimits struct {
	CPUMilliseconds int64 `json:"cpu_ms"`
}

type workerVersion struct {
	ID                 string                  `json:"id,omitempty"`
	MainModule         string                  `json:"main_module"`
	CompatibilityDate  string                  `json:"compatibility_date"`
	CompatibilityFlags []string                `json:"compatibility_flags"`
	Bindings           []workerBinding         `json:"bindings"`
	Exports            map[string]workerExport `json:"exports"`
	Containers         []workerContainer       `json:"containers"`
	Modules            []workerModule          `json:"modules"`
	Limits             workerLimits            `json:"limits"`
	Annotations        map[string]string       `json:"annotations"`
}

type workerTraffic struct {
	VersionID  string  `json:"version_id"`
	Percentage float64 `json:"percentage"`
}

type workerDeployment struct {
	ID       string          `json:"id"`
	Versions []workerTraffic `json:"versions"`
}

type workerEnvelope[T any] struct {
	Success    bool `json:"success"`
	Result     T    `json:"result"`
	ResultInfo struct {
		TotalCount int `json:"total_count"`
	} `json:"result_info"`
}

func workerVersionPath(model workerReleaseModel) string {
	return fmt.Sprintf("accounts/%s/workers/workers/%s/versions", url.PathEscape(model.AccountID.ValueString()), url.PathEscape(model.WorkerName.ValueString()))
}

func workerDeploymentPath(model workerReleaseModel) string {
	return fmt.Sprintf("accounts/%s/workers/scripts/%s/deployments", url.PathEscape(model.AccountID.ValueString()), url.PathEscape(model.WorkerName.ValueString()))
}

func (r *workerReleaseResource) getWorkerVersion(ctx context.Context, model workerReleaseModel, id string) (workerVersion, error) {
	var envelope workerEnvelope[workerVersion]
	err := r.client.Get(ctx, workerVersionPath(model)+"/"+url.PathEscape(id), nil, &envelope, option.WithQuery("include", "modules"))
	if err != nil {
		return workerVersion{}, err
	}
	if !envelope.Success || envelope.Result.ID == "" {
		return workerVersion{}, errors.New("Cloudflare did not return the requested Worker version.")
	}
	return envelope.Result, nil
}

func (r *workerReleaseResource) getWorkerDeployment(ctx context.Context, model workerReleaseModel) (workerDeployment, error) {
	var envelope workerEnvelope[struct {
		Deployments []workerDeployment `json:"deployments"`
	}]
	err := r.client.Get(ctx, workerDeploymentPath(model), nil, &envelope, option.WithQuery("per_page", "1"))
	if err != nil {
		return workerDeployment{}, err
	}
	if !envelope.Success {
		return workerDeployment{}, errors.New("Cloudflare did not return the Worker deployment.")
	}
	if len(envelope.Result.Deployments) == 0 {
		return workerDeployment{}, nil
	}
	return envelope.Result.Deployments[0], nil
}

func (model *workerReleaseModel) readWorkerDeployment(deployment workerDeployment) {
	model.DeploymentID = types.StringValue(deployment.ID)
	percentage := float64(0)
	for _, version := range deployment.Versions {
		if version.VersionID == model.VersionID.ValueString() {
			percentage += version.Percentage
		}
	}
	model.TrafficPercentage = types.Float64Value(percentage)
}

func (r *workerReleaseResource) findWorkerRelease(ctx context.Context, model workerReleaseModel, config workerReleaseConfiguration, fingerprint string) (workerVersion, bool, error) {
	const pageSize = 100
	message := workerReleaseMessage(config, fingerprint)
	for page := 1; ; page++ {
		var envelope workerEnvelope[[]workerVersion]
		err := r.client.Get(ctx, workerVersionPath(model), nil, &envelope, option.WithQuery("per_page", fmt.Sprint(pageSize)), option.WithQuery("page", fmt.Sprint(page)))
		if err != nil {
			return workerVersion{}, false, errors.New(describeCloudflareError(err))
		}
		if !envelope.Success {
			return workerVersion{}, false, errors.New("Cloudflare did not return the Worker's versions.")
		}
		for _, summary := range envelope.Result {
			if summary.Annotations["workers/message"] != message {
				continue
			}
			version, err := r.getWorkerVersion(ctx, model, summary.ID)
			if err != nil {
				return workerVersion{}, false, errors.New(describeCloudflareError(err))
			}
			actual, err := version.configuration()
			if err != nil {
				return workerVersion{}, false, err
			}
			actualFingerprint, err := actual.fingerprint()
			if err != nil {
				return workerVersion{}, false, err
			}
			if actualFingerprint != fingerprint {
				return workerVersion{}, false, errors.New("A Worker version's release marker disagrees with its actual bytes or configuration. Inspect the immutable version before retrying.")
			}
			return version, true, nil
		}
		if len(envelope.Result) < pageSize || (envelope.ResultInfo.TotalCount > 0 && page*pageSize >= envelope.ResultInfo.TotalCount) {
			return workerVersion{}, false, nil
		}
	}
}

func (r *workerReleaseResource) preserveWorkerNamespaces(ctx context.Context, model workerReleaseModel, config workerReleaseConfiguration) error {
	pager := r.client.DurableObjects.Namespaces.ListAutoPaging(ctx, durable_objects.NamespaceListParams{AccountID: cloudflare.F(model.AccountID.ValueString())})
	for pager.Next() {
		namespace := pager.Current()
		if namespace.Script != model.WorkerName.ValueString() {
			continue
		}
		if _, declared := config.Exports[namespace.Class]; !declared {
			return errors.New("The release omits an existing Durable Object class. Preserve all live classes or perform a separately reviewed migration before applying.")
		}
	}
	if err := pager.Err(); err != nil {
		return errors.New(describeCloudflareError(err))
	}
	return nil
}

func (r *workerReleaseResource) uploadWorkerRelease(ctx context.Context, model workerReleaseModel, config workerReleaseConfiguration, fingerprint string, content []byte, expectedDeploymentID string) (workerVersion, error) {
	version := workerVersion{
		MainModule: config.MainModule, CompatibilityDate: config.CompatibilityDate, CompatibilityFlags: config.CompatibilityFlags,
		Bindings: config.Bindings, Exports: config.Exports, Containers: config.Containers,
		Limits:      workerLimits{CPUMilliseconds: config.CPUMilliseconds},
		Modules:     []workerModule{{Name: config.MainModule, ContentType: "application/javascript+module", ContentBase64: base64.StdEncoding.EncodeToString(content)}},
		Annotations: map[string]string{"workers/tag": "v" + config.ReleaseVersion, "workers/message": workerReleaseMessage(config, fingerprint)},
	}
	bytes, err := json.Marshal(version)
	if err != nil {
		return workerVersion{}, errors.New("The Worker release could not be encoded.")
	}
	current, err := r.getWorkerDeployment(ctx, model)
	if err != nil {
		return workerVersion{}, errors.New(describeCloudflareError(err))
	}
	if current.ID != expectedDeploymentID {
		return workerVersion{}, errors.New("The live Worker deployment changed after compatibility preflight. Refresh state and retry before uploading or activating a release.")
	}
	var envelope workerEnvelope[workerVersion]
	err = r.client.Post(ctx, workerVersionPath(model), nil, &envelope, option.WithQuery("deploy", "true"), option.WithRequestBody("application/json", bytes))
	if err == nil && envelope.Success && envelope.Result.ID != "" {
		version, readErr := r.getWorkerVersion(ctx, model, envelope.Result.ID)
		if readErr == nil {
			return version, nil
		}
	}
	var apiError *cloudflare.Error
	if errors.As(err, &apiError) && apiError.StatusCode >= 400 && apiError.StatusCode < 500 && apiError.StatusCode != 408 {
		return workerVersion{}, errors.New(describeCloudflareError(err))
	}
	// Never repeat an uncertain POST. Its content-derived marker lets a read reconcile the completed version.
	recovery, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		version, found, readErr := r.findWorkerRelease(recovery, model, config, fingerprint)
		if readErr == nil && found {
			return version, nil
		}
		select {
		case <-recovery.Done():
			return workerVersion{}, errors.New("The Worker upload outcome could not be confirmed. No upload was retried. Refresh state and retry to reconcile a version carrying this release marker.")
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *workerReleaseResource) activateWorkerRelease(ctx context.Context, model workerReleaseModel, versionID string, expectedDeploymentID string) (workerDeployment, error) {
	current, err := r.getWorkerDeployment(ctx, model)
	if err != nil {
		return workerDeployment{}, errors.New(describeCloudflareError(err))
	}
	if len(current.Versions) == 1 && current.Versions[0].VersionID == versionID && current.Versions[0].Percentage == 100 {
		return current, nil
	}
	if current.ID != expectedDeploymentID {
		return workerDeployment{}, errors.New("The live Worker deployment changed after compatibility preflight. Refresh state and retry before uploading or activating a release.")
	}
	payload := struct {
		Strategy string          `json:"strategy"`
		Versions []workerTraffic `json:"versions"`
	}{Strategy: "percentage", Versions: []workerTraffic{{VersionID: versionID, Percentage: 100}}}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return workerDeployment{}, errors.New("The Worker activation could not be encoded.")
	}
	var envelope workerEnvelope[workerDeployment]
	postError := r.client.Post(ctx, workerDeploymentPath(model), nil, &envelope, option.WithRequestBody("application/json", bytes))
	current, err = r.getWorkerDeployment(ctx, model)
	if err == nil && len(current.Versions) == 1 && current.Versions[0].VersionID == versionID && current.Versions[0].Percentage == 100 {
		return current, nil
	}
	if postError != nil {
		return workerDeployment{}, errors.New(describeCloudflareError(postError))
	}
	return workerDeployment{}, errors.New("The requested Worker version is not exclusively active. Refresh state and retry; the existing version will be reused.")
}

func (r *workerReleaseResource) applyRelease(ctx context.Context, plan workerReleaseModel) (workerReleaseModel, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(plan.OperationTimeoutSeconds.ValueInt64())*time.Second)
	defer cancel()
	content, err := readWorkerBundle(plan)
	if err != nil {
		return workerReleaseModel{}, err
	}
	manifest, err := readWorkerManifest(plan)
	if err != nil {
		return workerReleaseModel{}, err
	}
	config, err := workerConfiguration(ctx, plan)
	if err != nil {
		return workerReleaseModel{}, err
	}
	fingerprint, err := config.fingerprint()
	if err != nil {
		return workerReleaseModel{}, err
	}
	expectedDeploymentID, err := r.preflightWorkerCompatibility(ctx, plan, manifest)
	if err != nil {
		return workerReleaseModel{}, err
	}
	if err = r.preserveWorkerNamespaces(ctx, plan, config); err != nil {
		return workerReleaseModel{}, err
	}
	version, found, err := r.findWorkerRelease(ctx, plan, config, fingerprint)
	if err != nil {
		return workerReleaseModel{}, err
	}
	if !found {
		version, err = r.uploadWorkerRelease(ctx, plan, config, fingerprint, content, expectedDeploymentID)
		if err != nil {
			return workerReleaseModel{}, err
		}
	}
	actual, err := version.configuration()
	if err != nil {
		return workerReleaseModel{}, err
	}
	actualFingerprint, err := actual.fingerprint()
	if err != nil || actualFingerprint != fingerprint {
		return workerReleaseModel{}, errors.New("Cloudflare's uploaded Worker bytes or configuration differ from the release. Inspect the new version before retrying.")
	}
	plan.ID = types.StringValue(plan.AccountID.ValueString() + "/" + plan.WorkerName.ValueString())
	plan.VersionID = types.StringValue(version.ID)
	plan.DeploymentID = types.StringValue("")
	if err = plan.readWorkerNamespaces(ctx, version); err != nil {
		return workerReleaseModel{}, err
	}
	deployment, err := r.activateWorkerRelease(ctx, plan, version.ID, expectedDeploymentID)
	if err != nil {
		return plan, err
	}
	plan.readWorkerDeployment(deployment)
	return plan, nil
}
