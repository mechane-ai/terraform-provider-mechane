package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/containers"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type containerApplicationResource struct {
	client *cloudflare.Client
}

type applicationModel struct {
	AccountID                types.String `tfsdk:"account_id"`
	Name                     types.String `tfsdk:"name"`
	DurableObjectNamespaceID types.String `tfsdk:"durable_object_namespace_id"`
	Image                    types.String `tfsdk:"image"`
	InstanceType             types.String `tfsdk:"instance_type"`
	MaxInstances             types.Int64  `tfsdk:"max_instances"`
	RolloutActiveGracePeriod types.Int64  `tfsdk:"rollout_active_grace_period"`
	LogsEnabled              types.Bool   `tfsdk:"logs_enabled"`
	DeleteProtection         types.Bool   `tfsdk:"delete_protection"`
	OperationTimeoutSeconds  types.Int64  `tfsdk:"operation_timeout_seconds"`
	ID                       types.String `tfsdk:"id"`
	Version                  types.Int64  `tfsdk:"version"`
	ActiveRolloutID          types.String `tfsdk:"active_rollout_id"`
	SchedulingPolicy         types.String `tfsdk:"scheduling_policy"`
}

func newContainerApplicationResource() resource.Resource {
	return &containerApplicationResource{}
}

func (r *containerApplicationResource) Metadata(_ context.Context, request resource.MetadataRequest, response *resource.MetadataResponse) {
	response.TypeName = request.ProviderTypeName + "_cloudflare_container_application"
}

func (r *containerApplicationResource) Schema(_ context.Context, _ resource.SchemaRequest, response *resource.SchemaResponse) {
	immutable := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	response.Schema = schema.Schema{
		Description: "A default-scheduled Cloudflare Containers application associated with an existing Durable Object namespace. Worker migrations and sandbox data remain outside this resource.",
		Attributes: map[string]schema.Attribute{
			"account_id":                  schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"name":                        schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"durable_object_namespace_id": schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"image":                       schema.StringAttribute{Required: true, Description: "Container image pinned by sha256 digest."},
			"instance_type":               schema.StringAttribute{Required: true},
			"max_instances":               schema.Int64Attribute{Required: true},
			"rollout_active_grace_period": schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(60)},
			"logs_enabled":                schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true)},
			"delete_protection":           schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "Apply false before destroying. Protection is retained in state even if the resource is removed from configuration."},
			"operation_timeout_seconds":   schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(1200)},
			"id":                          schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"version":                     schema.Int64Attribute{Computed: true},
			"active_rollout_id":           schema.StringAttribute{Computed: true},
			"scheduling_policy":           schema.StringAttribute{Computed: true},
		},
	}
}

func (r *containerApplicationResource) Configure(_ context.Context, request resource.ConfigureRequest, response *resource.ConfigureResponse) {
	if request.ProviderData == nil {
		return
	}
	client, ok := request.ProviderData.(*cloudflare.Client)
	if !ok {
		response.Diagnostics.AddError("Invalid provider client", "The provider did not supply a Cloudflare client.")
		return
	}
	r.client = client
}

func (r *containerApplicationResource) ValidateConfig(ctx context.Context, request resource.ValidateConfigRequest, response *resource.ValidateConfigResponse) {
	var config applicationModel
	response.Diagnostics.Append(request.Config.Get(ctx, &config)...)
	if response.Diagnostics.HasError() {
		return
	}
	for _, field := range []struct {
		name  string
		value types.String
	}{
		{"account_id", config.AccountID},
		{"durable_object_namespace_id", config.DurableObjectNamespaceID},
	} {
		if !field.value.IsUnknown() && !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(field.value.ValueString()) {
			response.Diagnostics.AddAttributeError(path.Root(field.name), "Invalid Cloudflare identifier", "Use the 32-character lowercase hexadecimal Cloudflare identifier.")
		}
	}
	if !config.Image.IsUnknown() && !regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`).MatchString(config.Image.ValueString()) {
		response.Diagnostics.AddAttributeError(path.Root("image"), "Unpinned container image", "Use an image reference ending in @sha256: followed by its 64 lowercase hexadecimal digest characters.")
	}
	if !config.InstanceType.IsUnknown() {
		switch config.InstanceType.ValueString() {
		case "lite", "basic", "standard-1", "standard-2", "standard-3", "standard-4":
		default:
			response.Diagnostics.AddAttributeError(path.Root("instance_type"), "Unsupported container size", "Choose lite, basic, or standard-1 through standard-4.")
		}
	}
	if !config.MaxInstances.IsUnknown() && config.MaxInstances.ValueInt64() < 1 {
		response.Diagnostics.AddAttributeError(path.Root("max_instances"), "Invalid capacity", "Set max_instances to at least 1. Sleeping machines do not consume a running instance.")
	}
	if !config.RolloutActiveGracePeriod.IsNull() && !config.RolloutActiveGracePeriod.IsUnknown() && (config.RolloutActiveGracePeriod.ValueInt64() < 0 || config.RolloutActiveGracePeriod.ValueInt64() > 604800) {
		response.Diagnostics.AddAttributeError(path.Root("rollout_active_grace_period"), "Invalid rollout grace period", "Set the rollout grace period between 0 and 604800 seconds.")
	}
	if !config.OperationTimeoutSeconds.IsNull() && !config.OperationTimeoutSeconds.IsUnknown() && (config.OperationTimeoutSeconds.ValueInt64() < 60 || config.OperationTimeoutSeconds.ValueInt64() > 7200) {
		response.Diagnostics.AddAttributeError(path.Root("operation_timeout_seconds"), "Invalid operation deadline", "Set the operation deadline between 60 and 7200 seconds.")
	}
}

func (r *containerApplicationResource) Create(ctx context.Context, request resource.CreateRequest, response *resource.CreateResponse) {
	var plan applicationModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(plan.OperationTimeoutSeconds.ValueInt64())*time.Second)
	defer cancel()

	existingID, err := r.findApplication(ctx, plan)
	if err != nil {
		response.Diagnostics.AddError("Find container application", err.Error())
		return
	}
	if existingID != "" {
		response.Diagnostics.AddError("Container application already exists", fmt.Sprintf("Import %s/%s before applying. The provider does not adopt an existing application by name.", plan.AccountID.ValueString(), existingID))
		return
	}
	applicationID, err := r.createApplication(ctx, plan)
	if err != nil {
		response.Diagnostics.AddError("Create container application", err.Error())
		return
	}
	plan.ID = types.StringValue(applicationID)
	plan.Version = types.Int64Value(0)
	plan.ActiveRolloutID = types.StringValue("")
	plan.SchedulingPolicy = types.StringValue("default")
	response.Diagnostics.Append(response.State.Set(ctx, &plan)...)

	application, err := r.waitForApplication(ctx, plan, true)
	if err != nil {
		response.Diagnostics.AddError("Wait for container application", err.Error())
		return
	}
	plan.readApplication(application)
	response.Diagnostics.Append(response.State.Set(ctx, &plan)...)
}

func (r *containerApplicationResource) Read(ctx context.Context, request resource.ReadRequest, response *resource.ReadResponse) {
	var state applicationModel
	response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	if response.Diagnostics.HasError() {
		return
	}
	application, found, err := r.readApplication(ctx, state.AccountID.ValueString(), state.ID.ValueString())
	if err != nil {
		response.Diagnostics.AddError("Read container application", err.Error())
		return
	}
	if !found {
		response.State.RemoveResource(ctx)
		return
	}
	if application.activeRolloutID != "" {
		ctx, cancel := context.WithTimeout(ctx, time.Duration(state.OperationTimeoutSeconds.ValueInt64())*time.Second)
		defer cancel()
		application, err = r.waitForApplication(ctx, state, false)
		if err != nil {
			response.Diagnostics.AddError("Resume container rollout", err.Error())
			return
		}
	}
	state.readApplication(application)
	response.Diagnostics.Append(response.State.Set(ctx, &state)...)
}

func (r *containerApplicationResource) Update(ctx context.Context, request resource.UpdateRequest, response *resource.UpdateResponse) {
	var plan applicationModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(plan.OperationTimeoutSeconds.ValueInt64())*time.Second)
	defer cancel()
	application, err := r.waitForApplication(ctx, plan, false)
	if err == nil {
		err = r.updateApplication(ctx, plan, application)
	}
	if err != nil {
		response.Diagnostics.AddError("Update container application", err.Error())
		return
	}
	application, err = r.waitForApplication(ctx, plan, true)
	if err != nil {
		response.Diagnostics.AddError("Wait for container rollout", err.Error())
		return
	}
	plan.readApplication(application)
	response.Diagnostics.Append(response.State.Set(ctx, &plan)...)
}

func (r *containerApplicationResource) Delete(ctx context.Context, request resource.DeleteRequest, response *resource.DeleteResponse) {
	var state applicationModel
	response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	if response.Diagnostics.HasError() {
		return
	}
	if state.DeleteProtection.IsNull() || state.DeleteProtection.IsUnknown() || state.DeleteProtection.ValueBool() {
		response.Diagnostics.AddError("Container application is protected", "Restore the resource configuration, set delete_protection to false, and apply that change before destroying. Deletion stops sandbox machines; it never deletes the Worker, Durable Object namespace, or R2 data.")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(state.OperationTimeoutSeconds.ValueInt64())*time.Second)
	defer cancel()
	application, found, err := r.readApplication(ctx, state.AccountID.ValueString(), state.ID.ValueString())
	if err != nil {
		response.Diagnostics.AddError("Verify application before deletion", err.Error())
		return
	}
	if !found {
		return
	}
	if application.namespaceID != state.DurableObjectNamespaceID.ValueString() || application.name != state.Name.ValueString() {
		response.Diagnostics.AddError("Application ownership changed", "Refresh the application and inspect its name and Durable Object namespace before deleting.")
		return
	}
	_, err = r.client.Containers.Applications.Delete(ctx, state.ID.ValueString(), containers.ApplicationDeleteParams{AccountID: cloudflare.F(state.AccountID.ValueString())})
	if err != nil && !isNotFound(err) {
		response.Diagnostics.AddError("Delete container application", describeCloudflareError(err))
		return
	}
	for {
		_, err := r.client.Containers.Applications.Get(ctx, state.ID.ValueString(), containers.ApplicationGetParams{AccountID: cloudflare.F(state.AccountID.ValueString())})
		if isNotFound(err) {
			return
		}
		if err != nil {
			response.Diagnostics.AddError("Confirm application deletion", describeCloudflareError(err))
			return
		}
		select {
		case <-ctx.Done():
			response.Diagnostics.AddError("Confirm application deletion", describeCloudflareError(ctx.Err()))
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *containerApplicationResource) ImportState(ctx context.Context, request resource.ImportStateRequest, response *resource.ImportStateResponse) {
	parts := strings.Split(request.ID, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		response.Diagnostics.AddError("Invalid application import ID", "Use <account_id>/<application_id>.")
		return
	}
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("account_id"), parts[0])...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("id"), parts[1])...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("delete_protection"), true)...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("operation_timeout_seconds"), int64(1200))...)
}
