package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Existing classes keep their namespace within one Worker. Planning each class
// separately preserves dependent containers while leaving new namespaces unknown.
func (r *workerReleaseResource) ModifyPlan(ctx context.Context, request resource.ModifyPlanRequest, response *resource.ModifyPlanResponse) {
	if request.Plan.Raw.IsNull() {
		return
	}
	var state, plan workerReleaseModel
	if !request.State.Raw.IsNull() {
		response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	}
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	if plan.ReleaseManifestFile.IsUnknown() || plan.ReleaseManifestFile.IsNull() || plan.ReleaseManifestSHA256.IsUnknown() || plan.ReleaseManifestSHA256.IsNull() {
		return
	}
	manifest, err := readWorkerManifest(plan)
	if err != nil {
		response.Diagnostics.AddError("Read release manifest", err.Error())
		return
	}
	if err = plan.acceptManifest(ctx, manifest); err != nil {
		response.Diagnostics.AddError("Read release manifest", err.Error())
		return
	}
	if !plan.QualificationFile.IsUnknown() && !plan.QualificationFile.IsNull() && !plan.QualificationSHA256.IsUnknown() && !plan.QualificationSHA256.IsNull() {
		if err := verifyWorkerQualification(ctx, plan, manifest); err != nil {
			response.Diagnostics.AddError("Verify release qualification", err.Error())
			return
		}
	}
	response.Diagnostics.Append(response.Plan.Set(ctx, &plan)...)
	if request.State.Raw.IsNull() || response.Diagnostics.HasError() {
		return
	}
	if plan.AccountID.IsUnknown() || plan.WorkerName.IsUnknown() || !plan.AccountID.Equal(state.AccountID) || !plan.WorkerName.Equal(state.WorkerName) || plan.DurableObjectBindings.IsUnknown() {
		return
	}
	for _, binding := range plan.DurableObjectBindings.Elements() {
		if binding.IsUnknown() || binding.IsNull() {
			return
		}
	}
	var bindings []workerBindingModel
	response.Diagnostics.Append(plan.DurableObjectBindings.ElementsAs(ctx, &bindings, false)...)
	if response.Diagnostics.HasError() {
		return
	}
	existing := state.DurableObjectNamespaces.Elements()
	namespaces := make(map[string]attr.Value)
	for _, binding := range bindings {
		if binding.Type.IsUnknown() {
			return
		}
		if binding.Type.ValueString() != "durable_object_namespace" {
			continue
		}
		if binding.ClassName.IsUnknown() || binding.ClassName.IsNull() {
			return
		}
		class := binding.ClassName.ValueString()
		namespace, found := existing[class]
		if !found {
			namespace = types.StringUnknown()
		}
		namespaces[class] = namespace
	}
	value, diagnostics := types.MapValue(types.StringType, namespaces)
	response.Diagnostics.Append(diagnostics...)
	if response.Diagnostics.HasError() {
		return
	}
	response.Diagnostics.Append(response.Plan.SetAttribute(ctx, path.Root("durable_object_namespaces"), value)...)
}
