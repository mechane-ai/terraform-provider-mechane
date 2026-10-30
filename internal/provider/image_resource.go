package provider

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type containerImageResource struct {
	client *cloudflare.Client
}

type imageModel struct {
	AccountID               types.String `tfsdk:"account_id"`
	Repository              types.String `tfsdk:"repository"`
	SourceURL               types.String `tfsdk:"source_url"`
	SourceSHA256            types.String `tfsdk:"source_sha256"`
	Digest                  types.String `tfsdk:"digest"`
	OperationTimeoutSeconds types.Int64  `tfsdk:"operation_timeout_seconds"`
	ID                      types.String `tfsdk:"id"`
	Image                   types.String `tfsdk:"image"`
}

func newContainerImageResource() resource.Resource {
	return &containerImageResource{}
}

func (r *containerImageResource) Metadata(_ context.Context, request resource.MetadataRequest, response *resource.MetadataResponse) {
	response.TypeName = request.ProviderTypeName + "_cloudflare_container_image"
}

func (r *containerImageResource) Schema(_ context.Context, _ resource.SchemaRequest, response *resource.SchemaResponse) {
	immutable := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	response.Schema = schema.Schema{
		Description: "Copies a verified OCI release archive into the account's Cloudflare registry without Docker. Removal forgets ownership and retains the immutable image for running applications and rollback.",
		Attributes: map[string]schema.Attribute{
			"account_id":                schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"repository":                schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"source_url":                schema.StringAttribute{Required: true, Description: "Public HTTPS URL for the uncompressed OCI layout tar archive."},
			"source_sha256":             schema.StringAttribute{Required: true, Description: "SHA-256 of the complete release archive, in lowercase hexadecimal."},
			"digest":                    schema.StringAttribute{Required: true, PlanModifiers: immutable, Description: "sha256 digest of the Linux amd64 image manifest inside the archive."},
			"operation_timeout_seconds": schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(2400)},
			"id":                        schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"image":                     schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *containerImageResource) Configure(_ context.Context, request resource.ConfigureRequest, response *resource.ConfigureResponse) {
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

func (r *containerImageResource) ValidateConfig(ctx context.Context, request resource.ValidateConfigRequest, response *resource.ValidateConfigResponse) {
	var config imageModel
	response.Diagnostics.Append(request.Config.Get(ctx, &config)...)
	if response.Diagnostics.HasError() {
		return
	}
	if !config.SourceURL.IsUnknown() {
		parsed, err := url.Parse(config.SourceURL.ValueString())
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			response.Diagnostics.AddAttributeError(path.Root("source_url"), "Invalid release archive URL", "Use a public HTTPS URL without credentials, query parameters, or a fragment.")
		}
	}
	for _, field := range []struct {
		name    string
		value   types.String
		pattern string
	}{
		{"account_id", config.AccountID, `^[a-f0-9]{32}$`},
		{"repository", config.Repository, `^[a-z0-9]+(?:[._-][a-z0-9]+)*$`},
		{"source_sha256", config.SourceSHA256, `^[a-f0-9]{64}$`},
		{"digest", config.Digest, `^sha256:[a-f0-9]{64}$`},
	} {
		if !field.value.IsUnknown() && !regexp.MustCompile(field.pattern).MatchString(field.value.ValueString()) {
			response.Diagnostics.AddAttributeError(path.Root(field.name), "Invalid image attribute", fmt.Sprintf("%s has an unsupported format.", field.name))
		}
	}
	if !config.OperationTimeoutSeconds.IsNull() && !config.OperationTimeoutSeconds.IsUnknown() && (config.OperationTimeoutSeconds.ValueInt64() < 60 || config.OperationTimeoutSeconds.ValueInt64() > 7200) {
		response.Diagnostics.AddAttributeError(path.Root("operation_timeout_seconds"), "Invalid operation deadline", "Set the operation deadline between 60 and 7200 seconds.")
	}
}

func (r *containerImageResource) Create(ctx context.Context, request resource.CreateRequest, response *resource.CreateResponse) {
	var plan imageModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(plan.OperationTimeoutSeconds.ValueInt64())*time.Second)
	defer cancel()
	if err := r.copyImage(ctx, plan); err != nil {
		response.Diagnostics.AddError("Copy container image", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.AccountID.ValueString() + "/" + plan.Repository.ValueString() + "@" + plan.Digest.ValueString())
	plan.Image = types.StringValue("registry.cloudflare.com/" + plan.ID.ValueString())
	response.Diagnostics.Append(response.State.Set(ctx, &plan)...)
}

func (r *containerImageResource) Read(ctx context.Context, request resource.ReadRequest, response *resource.ReadResponse) {
	var state imageModel
	response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	if response.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	found, err := r.readImage(ctx, state)
	if err != nil {
		response.Diagnostics.AddError("Read container image", err.Error())
		return
	}
	if !found {
		response.State.RemoveResource(ctx)
		return
	}
	state.Image = types.StringValue("registry.cloudflare.com/" + state.ID.ValueString())
	response.Diagnostics.Append(response.State.Set(ctx, &state)...)
}

func (r *containerImageResource) Update(ctx context.Context, request resource.UpdateRequest, response *resource.UpdateResponse) {
	var plan imageModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if !response.Diagnostics.HasError() {
		plan.Image = types.StringValue("registry.cloudflare.com/" + plan.ID.ValueString())
		response.Diagnostics.Append(response.State.Set(ctx, &plan)...)
	}
}

func (r *containerImageResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
	// Images can still be referenced by sleeping machines or earlier application versions.
}

func (r *containerImageResource) ImportState(ctx context.Context, request resource.ImportStateRequest, response *resource.ImportStateResponse) {
	parts := strings.SplitN(request.ID, "/", 2)
	if len(parts) != 2 {
		response.Diagnostics.AddError("Invalid image import ID", "Use <account_id>/<repository>@sha256:<digest>.")
		return
	}
	image := strings.SplitN(parts[1], "@", 2)
	if len(image) != 2 || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(image[1]) {
		response.Diagnostics.AddError("Invalid image import ID", "Use <account_id>/<repository>@sha256:<digest>.")
		return
	}
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("id"), request.ID)...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("account_id"), parts[0])...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("repository"), image[0])...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("digest"), image[1])...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("operation_timeout_seconds"), int64(2400))...)
}
