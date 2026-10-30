package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/containers"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type containerApplication struct {
	id, name, namespaceID, image, instanceType, activeRolloutID, health string
	version, maxInstances, rolloutGracePeriod                           int64
	logsEnabled                                                         bool
}

func (m *applicationModel) readApplication(application containerApplication) {
	m.ID = types.StringValue(application.id)
	m.Name = types.StringValue(application.name)
	m.DurableObjectNamespaceID = types.StringValue(application.namespaceID)
	m.Image = types.StringValue(application.image)
	m.InstanceType = types.StringValue(application.instanceType)
	m.MaxInstances = types.Int64Value(application.maxInstances)
	m.RolloutActiveGracePeriod = types.Int64Value(application.rolloutGracePeriod)
	m.LogsEnabled = types.BoolValue(application.logsEnabled)
	m.Version = types.Int64Value(application.version)
	m.ActiveRolloutID = types.StringValue(application.activeRolloutID)
	m.SchedulingPolicy = types.StringValue("default")
}

func (r *containerApplicationResource) findApplication(ctx context.Context, plan applicationModel) (string, error) {
	pager := r.client.Containers.Applications.ListAutoPaging(ctx, containers.ApplicationListParams{AccountID: cloudflare.F(plan.AccountID.ValueString())})
	var applicationID string
	for pager.Next() {
		application := pager.Current()
		if application.Name != plan.Name.ValueString() {
			continue
		}
		if applicationID != "" {
			return "", errors.New("More than one application has the configured name. Resolve the duplicate applications before importing.")
		}
		applicationID = application.ID
	}
	if err := pager.Err(); err != nil {
		return "", errors.New(describeCloudflareError(err))
	}
	return applicationID, nil
}

func (r *containerApplicationResource) createApplication(ctx context.Context, plan applicationModel) (string, error) {
	type configuration = containers.ApplicationNewParamsBodyCcContainersCreateScheduledApplicationRequestConfiguration
	type observability = containers.ApplicationNewParamsBodyCcContainersCreateScheduledApplicationRequestObservability
	type logs = containers.ApplicationNewParamsBodyCcContainersCreateScheduledApplicationRequestObservabilityLogs
	type durableObjects = containers.ApplicationNewParamsBodyCcContainersCreateScheduledApplicationRequestDurableObjects
	created, err := r.client.Containers.Applications.New(ctx, containers.ApplicationNewParams{
		AccountID: cloudflare.F(plan.AccountID.ValueString()),
		Body: containers.ApplicationNewParamsBodyCcContainersCreateScheduledApplicationRequest{
			Name:             cloudflare.F(plan.Name.ValueString()),
			SchedulingPolicy: cloudflare.F(containers.ApplicationNewParamsBodyCcContainersCreateScheduledApplicationRequestSchedulingPolicyDefault),
			// The Durable Object starts instances on demand; capacity is a ceiling.
			Instances:      cloudflare.F(int64(0)),
			MaxInstances:   cloudflare.F(plan.MaxInstances.ValueInt64()),
			DurableObjects: cloudflare.F[containers.ApplicationNewParamsBodyCcContainersCreateScheduledApplicationRequestDurableObjectsUnion](durableObjects{NamespaceID: cloudflare.F(plan.DurableObjectNamespaceID.ValueString())}),
			Configuration: cloudflare.F(configuration{
				Image:        cloudflare.F(plan.Image.ValueString()),
				InstanceType: cloudflare.F(containers.ApplicationNewParamsBodyCcContainersCreateScheduledApplicationRequestConfigurationInstanceType(plan.InstanceType.ValueString())),
			}),
			Observability:            cloudflare.F(observability{Logs: cloudflare.F(logs{Enabled: cloudflare.F(plan.LogsEnabled.ValueBool())})}),
			RolloutActiveGracePeriod: cloudflare.F(plan.RolloutActiveGracePeriod.ValueInt64()),
		},
	})
	if err != nil {
		// A response can be lost after the application was created. Recover its ID only
		// when its name, namespace, and complete managed configuration match this request.
		applicationID, lookupErr := r.findApplication(ctx, plan)
		if lookupErr == nil && applicationID != "" {
			application, found, readErr := r.readApplication(ctx, plan.AccountID.ValueString(), applicationID)
			if readErr == nil && found && matchesApplication(plan, application) {
				return applicationID, nil
			}
		}
		return "", errors.New(describeCloudflareError(err))
	}
	if created.ID == "" {
		return "", errors.New("Cloudflare did not return an application ID. Inspect the application list before retrying.")
	}
	return created.ID, nil
}

func (r *containerApplicationResource) readApplication(ctx context.Context, accountID, applicationID string) (containerApplication, bool, error) {
	response, err := r.client.Containers.Applications.Get(ctx, applicationID, containers.ApplicationGetParams{AccountID: cloudflare.F(accountID)})
	if isNotFound(err) {
		return containerApplication{}, false, nil
	}
	if err != nil {
		return containerApplication{}, false, errors.New(describeCloudflareError(err))
	}
	application, ok := response.AsUnion().(containers.ApplicationGetResponseCcScheduledApplication)
	if !ok || application.SchedulingPolicy != "default" {
		return containerApplication{}, false, errors.New("This resource manages only default-scheduled applications. It will not convert or delete a Durable Object-scheduled application.")
	}
	instanceType, err := readInstanceType(application.Configuration)
	if err != nil {
		return containerApplication{}, false, err
	}
	logsEnabled := application.Observability.Logs.Enabled
	if application.JSON.Observability.IsMissing() {
		logsEnabled = application.Configuration.Observability.Logs.Enabled
	}
	return containerApplication{
		id: application.ID, name: application.Name,
		namespaceID: application.DurableObjects.NamespaceID,
		image:       application.Configuration.Image, instanceType: instanceType,
		maxInstances: application.MaxInstances, rolloutGracePeriod: application.RolloutActiveGracePeriod,
		logsEnabled: logsEnabled, version: application.Version,
		activeRolloutID: application.ActiveRolloutID, health: string(application.Health.Summary),
	}, true, nil
}

func (r *containerApplicationResource) updateApplication(ctx context.Context, plan applicationModel, current containerApplication) error {
	if current.name != plan.Name.ValueString() || current.namespaceID != plan.DurableObjectNamespaceID.ValueString() {
		return errors.New("The application identity or Durable Object association changed outside OpenTofu. Refresh and inspect the replacement plan before applying.")
	}
	if current.maxInstances != plan.MaxInstances.ValueInt64() || current.rolloutGracePeriod != plan.RolloutActiveGracePeriod.ValueInt64() || current.logsEnabled != plan.LogsEnabled.ValueBool() {
		_, err := r.client.Containers.Applications.Edit(ctx, current.id, containers.ApplicationEditParams{
			AccountID:                cloudflare.F(plan.AccountID.ValueString()),
			MaxInstances:             cloudflare.F(plan.MaxInstances.ValueInt64()),
			RolloutActiveGracePeriod: cloudflare.F(plan.RolloutActiveGracePeriod.ValueInt64()),
			Observability: cloudflare.F(containers.ApplicationEditParamsObservability{
				Logs: cloudflare.F(containers.ApplicationEditParamsObservabilityLogs{Enabled: cloudflare.F(plan.LogsEnabled.ValueBool())}),
			}),
		})
		if err != nil {
			return errors.New(describeCloudflareError(err))
		}
	}
	if current.image == plan.Image.ValueString() && current.instanceType == plan.InstanceType.ValueString() {
		return nil
	}
	_, err := r.client.Containers.Applications.Rollouts.New(ctx, current.id, containers.ApplicationRolloutNewParams{
		AccountID:      cloudflare.F(plan.AccountID.ValueString()),
		Description:    cloudflare.F("Mechane installation update"),
		Strategy:       cloudflare.F(containers.ApplicationRolloutNewParamsStrategyRolling),
		Kind:           cloudflare.F(containers.ApplicationRolloutNewParamsKindFullAuto),
		StepPercentage: cloudflare.F(containers.ApplicationRolloutNewParamsStepPercentage100),
		TargetConfiguration: cloudflare.F(containers.ApplicationRolloutNewParamsTargetConfiguration{
			Image:        cloudflare.F(plan.Image.ValueString()),
			InstanceType: cloudflare.F(containers.ApplicationRolloutNewParamsTargetConfigurationInstanceType(plan.InstanceType.ValueString())),
		}),
	})
	if err != nil {
		return errors.New(describeCloudflareError(err))
	}
	return nil
}

func (r *containerApplicationResource) waitForApplication(ctx context.Context, plan applicationModel, requireTarget bool) (containerApplication, error) {
	observedRollout := false
	for {
		application, found, err := r.readApplication(ctx, plan.AccountID.ValueString(), plan.ID.ValueString())
		if err != nil {
			return containerApplication{}, err
		}
		// The application endpoint can return 404 briefly after accepting its creation.
		if found {
			rolloutActive := application.activeRolloutID != ""
			atTarget := matchesApplication(plan, application)
			// An unhealthy idle application must remain readable so an update can repair it.
			if !rolloutActive && !requireTarget {
				return application, nil
			}
			if application.health == "unhealthy" && (rolloutActive || atTarget) {
				return containerApplication{}, fmt.Errorf("Container application %s reports unhealthy target instances. Inspect its Cloudflare diagnostics before retrying.", application.id)
			}
			if !rolloutActive {
				if atTarget {
					return application, nil
				}
				if observedRollout {
					return containerApplication{}, errors.New("The rollout stopped before the requested configuration became active. Inspect the Cloudflare rollout, then refresh and retry.")
				}
			} else {
				observedRollout = true
			}
		}
		select {
		case <-ctx.Done():
			return containerApplication{}, errors.New(describeCloudflareError(ctx.Err()))
		case <-time.After(2 * time.Second):
		}
	}
}

func matchesApplication(plan applicationModel, application containerApplication) bool {
	return application.name == plan.Name.ValueString() &&
		application.namespaceID == plan.DurableObjectNamespaceID.ValueString() &&
		application.image == plan.Image.ValueString() &&
		application.instanceType == plan.InstanceType.ValueString() &&
		application.maxInstances == plan.MaxInstances.ValueInt64() &&
		application.rolloutGracePeriod == plan.RolloutActiveGracePeriod.ValueInt64() &&
		application.logsEnabled == plan.LogsEnabled.ValueBool()
}

func readInstanceType(configuration containers.ApplicationGetResponseCcScheduledApplicationConfiguration) (string, error) {
	if configuration.InstanceType != "" {
		return string(configuration.InstanceType), nil
	}
	// Older applications return hardware quantities instead of the named instance type.
	var hardware struct {
		VCPU      float64 `json:"vcpu"`
		MemoryMiB int64   `json:"memory_mib"`
		Disk      struct {
			SizeMB int64 `json:"size_mb"`
		} `json:"disk"`
	}
	if err := json.Unmarshal([]byte(configuration.JSON.RawJSON()), &hardware); err != nil {
		return "", errors.New("Cloudflare returned an unreadable container size.")
	}
	instanceTypes := []struct {
		name              string
		cpu               float64
		memoryMiB, diskMB int64
	}{
		{"lite", 0.0625, 256, 2000}, {"basic", 0.25, 1024, 4000},
		{"standard-1", 0.5, 4096, 8000}, {"standard-2", 1, 6144, 12000},
		{"standard-3", 2, 8192, 16000}, {"standard-4", 4, 12288, 20000},
	}
	for _, instanceType := range instanceTypes {
		if hardware.VCPU == instanceType.cpu && hardware.MemoryMiB == instanceType.memoryMiB && hardware.Disk.SizeMB == instanceType.diskMB {
			return instanceType.name, nil
		}
	}
	return "", errors.New("Cloudflare returned a custom container size that this resource cannot manage. Select a standard instance type before importing.")
}
