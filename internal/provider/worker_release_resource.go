package provider

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type workerReleaseResource struct {
	client *cloudflare.Client
}

type workerReleaseModel struct {
	AccountID               types.String  `tfsdk:"account_id"`
	WorkerName              types.String  `tfsdk:"worker_name"`
	BundleFile              types.String  `tfsdk:"bundle_file"`
	BundleSHA256            types.String  `tfsdk:"bundle_sha256"`
	ReleaseManifestFile     types.String  `tfsdk:"release_manifest_file"`
	ReleaseManifestSHA256   types.String  `tfsdk:"release_manifest_sha256"`
	AdoptionBaseline        types.String  `tfsdk:"adoption_baseline"`
	CompatibilityDate       types.String  `tfsdk:"compatibility_date"`
	CompatibilityFlags      types.Set     `tfsdk:"compatibility_flags"`
	Bindings                types.Set     `tfsdk:"bindings"`
	Exports                 types.Map     `tfsdk:"exports"`
	ContainerClasses        types.Set     `tfsdk:"container_classes"`
	CPUMilliseconds         types.Int64   `tfsdk:"cpu_ms"`
	ReleaseVersion          types.String  `tfsdk:"release_version"`
	SourceRevision          types.String  `tfsdk:"source_revision"`
	TrafficPercentage       types.Float64 `tfsdk:"traffic_percentage"`
	OperationTimeoutSeconds types.Int64   `tfsdk:"operation_timeout_seconds"`
	ID                      types.String  `tfsdk:"id"`
	VersionID               types.String  `tfsdk:"version_id"`
	DeploymentID            types.String  `tfsdk:"deployment_id"`
	DurableObjectNamespaces types.Map     `tfsdk:"durable_object_namespaces"`
}

func newWorkerReleaseResource() resource.Resource {
	return &workerReleaseResource{}
}

func (r *workerReleaseResource) Metadata(_ context.Context, request resource.MetadataRequest, response *resource.MetadataResponse) {
	response.TypeName = request.ProviderTypeName + "_cloudflare_worker_release"
}

func (r *workerReleaseResource) Schema(_ context.Context, _ resource.SchemaRequest, response *resource.SchemaResponse) {
	immutable := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	emptySet := setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{}))
	response.Schema = schema.Schema{
		Description: "An immutable Worker bundle, its reference-only bindings, and exclusive activation. The separate Worker resource owns identity and deletion; removing this release retains versions, Durable Objects, and their data.",
		Attributes: map[string]schema.Attribute{
			"account_id":              schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"worker_name":             schema.StringAttribute{Required: true, PlanModifiers: immutable},
			"bundle_file":             schema.StringAttribute{Required: true},
			"bundle_sha256":           schema.StringAttribute{Required: true},
			"release_manifest_file":   schema.StringAttribute{Required: true, Description: "Local installation.json from the immutable published release."},
			"release_manifest_sha256": schema.StringAttribute{Required: true, Description: "SHA-256 of installation.json, recorded on every uploaded Worker version."},
			"adoption_baseline":       schema.StringAttribute{Optional: true, Description: "One-time JSON baseline for an unmarked legacy Worker. It binds worker_version_id, bundle_sha256, release_version, and complete formats maps. Remove it after adoption."},
			"compatibility_date":      schema.StringAttribute{Required: true},
			"compatibility_flags":     schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, Default: emptySet},
			"bindings":                schema.SetNestedAttribute{Required: true, NestedObject: schema.NestedAttributeObject{Attributes: workerBindingAttributes()}},
			"exports": schema.MapNestedAttribute{Required: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
				"type":    schema.StringAttribute{Required: true},
				"storage": schema.StringAttribute{Required: true},
				"state":   schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("created")},
			}}},
			"container_classes":         schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, Default: emptySet},
			"cpu_ms":                    schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(300000)},
			"release_version":           schema.StringAttribute{Required: true},
			"source_revision":           schema.StringAttribute{Required: true},
			"traffic_percentage":        schema.Float64Attribute{Optional: true, Computed: true, Default: float64default.StaticFloat64(100), Description: "Exclusive activation is required. Refresh reports drift when another deployment changes traffic."},
			"operation_timeout_seconds": schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(300)},
			"id":                        schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"version_id":                schema.StringAttribute{Computed: true},
			"deployment_id":             schema.StringAttribute{Computed: true},
			"durable_object_namespaces": schema.MapAttribute{Computed: true, ElementType: types.StringType, Description: "Provisioned namespace IDs keyed by bound Durable Object class."},
		},
	}
}

func workerBindingAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"name":         schema.StringAttribute{Required: true},
		"type":         schema.StringAttribute{Required: true},
		"text":         schema.StringAttribute{Optional: true},
		"class_name":   schema.StringAttribute{Optional: true},
		"bucket_name":  schema.StringAttribute{Optional: true},
		"jurisdiction": schema.StringAttribute{Optional: true},
		"store_id":     schema.StringAttribute{Optional: true},
		"secret_name":  schema.StringAttribute{Optional: true},
	}
}

func (r *workerReleaseResource) Configure(_ context.Context, request resource.ConfigureRequest, response *resource.ConfigureResponse) {
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

func (r *workerReleaseResource) ValidateConfig(ctx context.Context, request resource.ValidateConfigRequest, response *resource.ValidateConfigResponse) {
	var config workerReleaseModel
	response.Diagnostics.Append(request.Config.Get(ctx, &config)...)
	if response.Diagnostics.HasError() {
		return
	}
	for name, value := range map[string]types.String{"bundle_sha256": config.BundleSHA256, "release_manifest_sha256": config.ReleaseManifestSHA256, "source_revision": config.SourceRevision} {
		pattern := `^[a-f0-9]{64}$`
		if name == "source_revision" {
			pattern = `^[a-f0-9]{40}$`
		}
		if !value.IsUnknown() && !regexp.MustCompile(pattern).MatchString(value.ValueString()) {
			response.Diagnostics.AddAttributeError(path.Root(name), "Invalid release digest", "Use the complete lowercase hexadecimal digest from the release manifest.")
		}
	}
	if !config.ReleaseVersion.IsUnknown() && !config.ReleaseVersion.IsNull() {
		if _, err := parseWorkerReleaseVersion(config.ReleaseVersion.ValueString()); err != nil {
			response.Diagnostics.AddAttributeError(path.Root("release_version"), "Invalid release version", err.Error())
		}
	}
	if !config.AdoptionBaseline.IsUnknown() && !config.AdoptionBaseline.IsNull() {
		if _, err := decodeWorkerAdoptionBaseline(config.AdoptionBaseline.ValueString()); err != nil {
			response.Diagnostics.AddAttributeError(path.Root("adoption_baseline"), "Invalid adoption baseline", err.Error())
		}
	}
	if !config.TrafficPercentage.IsNull() && !config.TrafficPercentage.IsUnknown() && config.TrafficPercentage.ValueFloat64() != 100 {
		response.Diagnostics.AddAttributeError(path.Root("traffic_percentage"), "Exclusive deployment required", "A Mechane release must receive 100 percent of traffic.")
	}
	if !config.OperationTimeoutSeconds.IsNull() && !config.OperationTimeoutSeconds.IsUnknown() && (config.OperationTimeoutSeconds.ValueInt64() < 30 || config.OperationTimeoutSeconds.ValueInt64() > 1800) {
		response.Diagnostics.AddAttributeError(path.Root("operation_timeout_seconds"), "Invalid operation deadline", "Set the deadline between 30 and 1800 seconds.")
	}
	if !config.CPUMilliseconds.IsNull() && !config.CPUMilliseconds.IsUnknown() && (config.CPUMilliseconds.ValueInt64() < 1 || config.CPUMilliseconds.ValueInt64() > 300000) {
		response.Diagnostics.AddAttributeError(path.Root("cpu_ms"), "Invalid CPU limit", "Set the CPU limit between 1 and 300000 milliseconds.")
	}
	if !config.Bindings.IsUnknown() {
		var bindings []workerBindingModel
		response.Diagnostics.Append(config.Bindings.ElementsAs(ctx, &bindings, true)...)
		for _, binding := range bindings {
			if binding.Type.IsUnknown() {
				continue
			}
			switch binding.Type.ValueString() {
			case "plain_text", "durable_object_namespace", "r2_bucket", "secrets_store_secret":
			default:
				response.Diagnostics.AddAttributeError(path.Root("bindings"), "Unsupported Worker binding", "Use supported resource bindings or Secrets Store references. Credential values and secret_text bindings are not accepted.")
			}
		}
	}
	if !config.Exports.IsUnknown() {
		var exports map[string]workerExportModel
		response.Diagnostics.Append(config.Exports.ElementsAs(ctx, &exports, true)...)
		for _, export := range exports {
			invalidType := !export.Type.IsUnknown() && export.Type.ValueString() != "durable-object"
			invalidStorage := !export.Storage.IsUnknown() && export.Storage.ValueString() != "sqlite"
			invalidState := !export.State.IsUnknown() && !export.State.IsNull() && export.State.ValueString() != "created"
			if invalidType || invalidStorage || invalidState {
				response.Diagnostics.AddAttributeError(path.Root("exports"), "Destructive export change refused", "Release exports must name live SQLite Durable Object classes. Deletion, renaming, and transfer require a separate explicit migration.")
			}
		}
	}
	if request.Config.Raw.IsFullyKnown() {
		_, err := workerConfiguration(ctx, config)
		if err != nil {
			response.Diagnostics.AddError("Invalid Worker release", err.Error())
		}
	}
}

func (r *workerReleaseResource) Create(ctx context.Context, request resource.CreateRequest, response *resource.CreateResponse) {
	var plan workerReleaseModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	result, err := r.applyRelease(ctx, plan)
	if result.VersionID.ValueString() != "" {
		response.Diagnostics.Append(response.State.Set(ctx, &result)...)
	}
	if err != nil {
		response.Diagnostics.AddError("Apply Worker release", err.Error())
	}
}

func (r *workerReleaseResource) Update(ctx context.Context, request resource.UpdateRequest, response *resource.UpdateResponse) {
	var plan workerReleaseModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	result, err := r.applyRelease(ctx, plan)
	if result.VersionID.ValueString() != "" {
		response.Diagnostics.Append(response.State.Set(ctx, &result)...)
	}
	if err != nil {
		response.Diagnostics.AddError("Apply Worker release", err.Error())
	}
}

func (r *workerReleaseResource) Read(ctx context.Context, request resource.ReadRequest, response *resource.ReadResponse) {
	var state workerReleaseModel
	response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	if response.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	version, err := r.getWorkerVersion(ctx, state, state.VersionID.ValueString())
	if isNotFound(err) {
		response.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		response.Diagnostics.AddError("Read Worker release", describeCloudflareError(err))
		return
	}
	if err = state.readWorkerVersion(ctx, version); err != nil {
		response.Diagnostics.AddError("Read Worker release", err.Error())
		return
	}
	deployment, err := r.getWorkerDeployment(ctx, state)
	if err != nil {
		response.Diagnostics.AddError("Read Worker activation", describeCloudflareError(err))
		return
	}
	state.readWorkerDeployment(deployment)
	response.Diagnostics.Append(response.State.Set(ctx, &state)...)
}

func (r *workerReleaseResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// The Worker identity resource owns deletion; retained versions remain usable for rollback.
}

func (r *workerReleaseResource) ImportState(ctx context.Context, request resource.ImportStateRequest, response *resource.ImportStateResponse) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	parts := strings.Split(request.ID, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		response.Diagnostics.AddError("Invalid Worker release import", "Import account_id/worker_name or account_id/worker_name/version_id.")
		return
	}
	state := workerReleaseModel{
		AccountID: types.StringValue(parts[0]), WorkerName: types.StringValue(parts[1]),
		ID: types.StringValue(parts[0] + "/" + parts[1]), OperationTimeoutSeconds: types.Int64Value(300),
	}
	deployment, err := r.getWorkerDeployment(ctx, state)
	if err != nil {
		response.Diagnostics.AddError("Import Worker activation", describeCloudflareError(err))
		return
	}
	versionID := ""
	if len(parts) == 3 {
		versionID = parts[2]
	} else if len(deployment.Versions) == 1 && deployment.Versions[0].Percentage == 100 {
		versionID = deployment.Versions[0].VersionID
	}
	if versionID == "" {
		response.Diagnostics.AddError("Ambiguous Worker release", "Import an explicit version_id when the Worker has no exclusively active version.")
		return
	}
	version, err := r.getWorkerVersion(ctx, state, versionID)
	if err != nil {
		response.Diagnostics.AddError("Import Worker release", describeCloudflareError(err))
		return
	}
	if err = state.readWorkerVersion(ctx, version); err != nil {
		response.Diagnostics.AddError("Import Worker release", err.Error())
		return
	}
	state.readWorkerDeployment(deployment)
	response.Diagnostics.Append(response.State.Set(ctx, &state)...)
}
