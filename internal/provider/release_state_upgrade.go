package provider

import (
	"context"
	"maps"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func (r *containerImageResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	var current resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &current)
	currentType := current.Schema.Type().TerraformType(ctx).(tftypes.Object)
	previousType := currentType
	previousType.AttributeTypes = maps.Clone(currentType.AttributeTypes)
	delete(previousType.AttributeTypes, "source_file")
	previousType.AttributeTypes["source_url"] = tftypes.String
	return map[int64]resource.StateUpgrader{0: {StateUpgrader: func(ctx context.Context, request resource.UpgradeStateRequest, response *resource.UpgradeStateResponse) {
		upgradeReleaseState(request, response, previousType, currentType, []string{"source_url"}, []string{"source_file"})
	}}}
}

func (r *workerReleaseResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	var current resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &current)
	currentType := current.Schema.Type().TerraformType(ctx).(tftypes.Object)
	previousType := currentType
	previousType.AttributeTypes = maps.Clone(currentType.AttributeTypes)
	delete(previousType.AttributeTypes, "qualification_file")
	delete(previousType.AttributeTypes, "qualification_sha256")
	return map[int64]resource.StateUpgrader{0: {StateUpgrader: func(ctx context.Context, request resource.UpgradeStateRequest, response *resource.UpgradeStateResponse) {
		upgradeReleaseState(request, response, previousType, currentType, nil, []string{"qualification_file", "qualification_sha256"})
	}}}
}

func upgradeReleaseState(request resource.UpgradeStateRequest, response *resource.UpgradeStateResponse, previousType, currentType tftypes.Object, removed, added []string) {
	if request.RawState == nil {
		response.Diagnostics.AddError("Upgrade release state", "The previous resource state was not supplied.")
		return
	}
	previous, err := request.RawState.Unmarshal(previousType)
	if err != nil {
		response.Diagnostics.AddError("Upgrade release state", "The previous resource state could not be decoded. No remote resources were changed.")
		return
	}
	var values map[string]tftypes.Value
	if err := previous.As(&values); err != nil {
		response.Diagnostics.AddError("Upgrade release state", "The previous resource attributes could not be preserved.")
		return
	}
	for _, name := range removed {
		delete(values, name)
	}
	for _, name := range added {
		values[name] = tftypes.NewValue(currentType.AttributeTypes[name], nil)
	}
	dynamic, err := tfprotov6.NewDynamicValue(currentType, tftypes.NewValue(currentType, values))
	if err != nil {
		response.Diagnostics.AddError("Upgrade release state", "The preserved resource attributes could not be encoded.")
		return
	}
	response.DynamicValue = &dynamic
}
