package provider

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type machineApplicationResource struct {
	client *cloudflare.Client
}

type machineApplicationModel struct {
	AccountID        types.String `tfsdk:"account_id"`
	Name             types.String `tfsdk:"name"`
	NamespaceID      types.String `tfsdk:"durable_object_namespace_id"`
	LogsEnabled      types.Bool   `tfsdk:"logs_enabled"`
	DeleteProtection types.Bool   `tfsdk:"delete_protection"`
	ID               types.String `tfsdk:"id"`
}

func newMachineApplicationResource() resource.Resource { return &machineApplicationResource{} }

func (r *machineApplicationResource) Metadata(_ context.Context, request resource.MetadataRequest, response *resource.MetadataResponse) {
	response.TypeName = request.ProviderTypeName + "_cloudflare_machine_application"
}

func (r *machineApplicationResource) Schema(_ context.Context, _ resource.SchemaRequest, response *resource.SchemaResponse) {
	immutable := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	response.Schema = schema.Schema{
		Description: "Durable Object-scheduled compute. The Worker owns image selection, instance size and each machine lifecycle; its capacity ledger owns admission.",
		Attributes: map[string]schema.Attribute{
			"account_id":                  schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"name":                        schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"durable_object_namespace_id": schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"logs_enabled":                schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true)},
			"delete_protection":           schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "Apply false before deletion. Deleting compute never deletes its Durable Object ledger or portable workspace bucket."},
			"id":                          schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *machineApplicationResource) Configure(_ context.Context, request resource.ConfigureRequest, response *resource.ConfigureResponse) {
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

func (r *machineApplicationResource) ValidateConfig(ctx context.Context, request resource.ValidateConfigRequest, response *resource.ValidateConfigResponse) {
	var config machineApplicationModel
	response.Diagnostics.Append(request.Config.Get(ctx, &config)...)
	if response.Diagnostics.HasError() {
		return
	}
	for name, value := range map[string]types.String{"account_id": config.AccountID, "durable_object_namespace_id": config.NamespaceID} {
		if !value.IsUnknown() && !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(value.ValueString()) {
			response.Diagnostics.AddAttributeError(path.Root(name), "Invalid Cloudflare identifier", "Use the 32-character lowercase hexadecimal identifier.")
		}
	}
}

func (r *machineApplicationResource) Create(ctx context.Context, request resource.CreateRequest, response *resource.CreateResponse) {
	var plan machineApplicationModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	existing, err := (&containerApplicationResource{client: r.client}).findApplication(ctx, applicationModel{AccountID: plan.AccountID, Name: plan.Name})
	if err != nil {
		response.Diagnostics.AddError("Find machine application", err.Error())
		return
	}
	if existing != "" {
		response.Diagnostics.AddError("Machine application already exists", fmt.Sprintf("Import %s/%s; an uncertain create is never repeated by name.", plan.AccountID.ValueString(), existing))
		return
	}
	type body = containers.ApplicationNewParamsBodyCcContainersCreateDurableObjectApplicationRequest
	type objects = containers.ApplicationNewParamsBodyCcContainersCreateDurableObjectApplicationRequestDurableObjects
	type observability = containers.ApplicationNewParamsBodyCcContainersCreateDurableObjectApplicationRequestObservability
	type logs = containers.ApplicationNewParamsBodyCcContainersCreateDurableObjectApplicationRequestObservabilityLogs
	created, err := r.client.Containers.Applications.New(ctx, containers.ApplicationNewParams{
		AccountID: cloudflare.F(plan.AccountID.ValueString()),
		Body: body{
			Name:             cloudflare.F(plan.Name.ValueString()),
			SchedulingPolicy: cloudflare.F(containers.ApplicationNewParamsBodyCcContainersCreateDurableObjectApplicationRequestSchedulingPolicyDurableObject),
			DurableObjects:   cloudflare.F[containers.ApplicationNewParamsBodyCcContainersCreateDurableObjectApplicationRequestDurableObjectsUnion](objects{NamespaceID: cloudflare.F(plan.NamespaceID.ValueString())}),
			Observability:    cloudflare.F(observability{Logs: cloudflare.F(logs{Enabled: cloudflare.F(plan.LogsEnabled.ValueBool())})}),
		},
	})
	if err != nil {
		response.Diagnostics.AddError("Create machine application", describeCloudflareError(err))
		return
	}
	if created.ID == "" {
		response.Diagnostics.AddError("Create machine application", "Cloudflare returned no application ID. Inspect the application by name before retrying.")
		return
	}
	plan.ID = types.StringValue(created.ID)
	response.Diagnostics.Append(response.State.Set(ctx, &plan)...)
}

func (r *machineApplicationResource) read(ctx context.Context, model machineApplicationModel) (*containers.ApplicationGetResponseCcDurableObjectApplication, error) {
	result, err := r.client.Containers.Applications.Get(ctx, model.ID.ValueString(), containers.ApplicationGetParams{AccountID: cloudflare.F(model.AccountID.ValueString())})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New(describeCloudflareError(err))
	}
	if result.SchedulingPolicy != "durable_object" {
		return nil, errors.New("This resource owns only Durable Object-scheduled compute; it cannot adopt or delete a scheduler-managed application.")
	}
	// The SDK's undiscriminated union selects its first member even for durable_object.
	// Validate the wire discriminator, then decode into its public native response type.
	var app containers.ApplicationGetResponseCcDurableObjectApplication
	if err := json.Unmarshal([]byte(result.JSON.RawJSON()), &app); err != nil {
		return nil, errors.New("Cloudflare returned an unreadable machine application.")
	}
	if !model.NamespaceID.IsNull() && app.DurableObjects.NamespaceID != model.NamespaceID.ValueString() {
		return nil, errors.New("Machine application namespace changed. Inspect ownership before another operation.")
	}
	if !model.Name.IsNull() && app.Name != model.Name.ValueString() {
		return nil, errors.New("Machine application name changed. Inspect ownership before another operation.")
	}
	return &app, nil
}

func (r *machineApplicationResource) Read(ctx context.Context, request resource.ReadRequest, response *resource.ReadResponse) {
	var state machineApplicationModel
	response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	if response.Diagnostics.HasError() {
		return
	}
	app, err := r.read(ctx, state)
	if err != nil {
		response.Diagnostics.AddError("Read machine application", err.Error())
		return
	}
	if app == nil {
		response.State.RemoveResource(ctx)
		return
	}
	state.Name = types.StringValue(app.Name)
	state.NamespaceID = types.StringValue(app.DurableObjects.NamespaceID)
	state.LogsEnabled = types.BoolValue(app.Observability.Logs.Enabled)
	response.Diagnostics.Append(response.State.Set(ctx, &state)...)
}

func (r *machineApplicationResource) Update(ctx context.Context, request resource.UpdateRequest, response *resource.UpdateResponse) {
	var plan machineApplicationModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	app, err := r.read(ctx, plan)
	if err != nil {
		response.Diagnostics.AddError("Verify machine application", err.Error())
		return
	}
	if app == nil {
		response.Diagnostics.AddError("Verify machine application", "The recorded application no longer exists. Refresh the plan before recreating it.")
		return
	}
	_, err = r.client.Containers.Applications.Edit(ctx, plan.ID.ValueString(), containers.ApplicationEditParams{
		AccountID: cloudflare.F(plan.AccountID.ValueString()),
		Observability: cloudflare.F(containers.ApplicationEditParamsObservability{
			Logs: cloudflare.F(containers.ApplicationEditParamsObservabilityLogs{Enabled: cloudflare.F(plan.LogsEnabled.ValueBool())}),
		}),
	})
	if err != nil {
		response.Diagnostics.AddError("Update machine application", describeCloudflareError(err))
		return
	}
	response.Diagnostics.Append(response.State.Set(ctx, &plan)...)
}

func (r *machineApplicationResource) Delete(ctx context.Context, request resource.DeleteRequest, response *resource.DeleteResponse) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var state machineApplicationModel
	response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	if response.Diagnostics.HasError() {
		return
	}
	if state.DeleteProtection.IsNull() || state.DeleteProtection.IsUnknown() || state.DeleteProtection.ValueBool() {
		response.Diagnostics.AddError("Machine application is protected", "Drain its machines, then apply delete_protection=false before deletion.")
		return
	}
	app, err := r.read(ctx, state)
	if err != nil {
		response.Diagnostics.AddError("Verify machine application ownership", err.Error())
		return
	}
	if app == nil {
		return
	}
	_, err = r.client.Containers.Applications.Delete(ctx, state.ID.ValueString(), containers.ApplicationDeleteParams{AccountID: cloudflare.F(state.AccountID.ValueString())})
	if err != nil && !isNotFound(err) {
		response.Diagnostics.AddError("Delete machine application", describeCloudflareError(err))
		return
	}
	for {
		remaining, err := r.read(ctx, state)
		if err != nil {
			response.Diagnostics.AddError("Confirm machine application deletion", err.Error())
			return
		}
		if remaining == nil {
			return
		}
		select {
		case <-ctx.Done():
			response.Diagnostics.AddError("Confirm machine application deletion", "Cloudflare did not confirm absence before the deadline. Refresh before retrying.")
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *machineApplicationResource) ImportState(ctx context.Context, request resource.ImportStateRequest, response *resource.ImportStateResponse) {
	parts := strings.Split(request.ID, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		response.Diagnostics.AddError("Invalid machine application import ID", "Use <account_id>/<application_id>.")
		return
	}
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("account_id"), parts[0])...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("id"), parts[1])...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("delete_protection"), true)...)
}
