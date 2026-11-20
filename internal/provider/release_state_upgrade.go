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
		upgradeReleaseState(request, response, previousType, currentType, []string{"source_url"}, []string{"source_file"}, nil)
	}}}
}

func previousWorkerBindingsType(currentSet tftypes.Set) tftypes.Set {
	currentElem := currentSet.ElementType.(tftypes.Object)
	previousElem := currentElem
	previousElem.AttributeTypes = maps.Clone(currentElem.AttributeTypes)
	delete(previousElem.AttributeTypes, "id")
	return tftypes.Set{ElementType: previousElem}
}

func upgradeWorkerBindingSet(value tftypes.Value, targetSet tftypes.Set) (tftypes.Value, error) {
	if value.IsNull() || !value.IsKnown() {
		return tftypes.NewValue(targetSet, nil), nil
	}
	var items []tftypes.Value
	if err := value.As(&items); err != nil {
		return tftypes.Value{}, err
	}
	targetElem := targetSet.ElementType.(tftypes.Object)
	upgraded := make([]tftypes.Value, 0, len(items))
	for _, item := range items {
		var fields map[string]tftypes.Value
		if err := item.As(&fields); err != nil {
			return tftypes.Value{}, err
		}
		fields["id"] = tftypes.NewValue(tftypes.String, nil)
		upgraded = append(upgraded, tftypes.NewValue(targetElem, fields))
	}
	return tftypes.NewValue(targetSet, upgraded), nil
}

func (r *workerReleaseResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	var current resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &current)
	currentType := current.Schema.Type().TerraformType(ctx).(tftypes.Object)
	v1Type := currentType
	v1Type.AttributeTypes = maps.Clone(currentType.AttributeTypes)
	v1Type.AttributeTypes["bindings"] = previousWorkerBindingsType(currentType.AttributeTypes["bindings"].(tftypes.Set))
	v1Type.AttributeTypes["durable_object_bindings"] = previousWorkerBindingsType(currentType.AttributeTypes["durable_object_bindings"].(tftypes.Set))
	v0Type := v1Type
	v0Type.AttributeTypes = maps.Clone(v1Type.AttributeTypes)
	delete(v0Type.AttributeTypes, "qualification_file")
	delete(v0Type.AttributeTypes, "qualification_sha256")
	bindingFields := []string{"bindings", "durable_object_bindings"}
	return map[int64]resource.StateUpgrader{
		0: {StateUpgrader: func(ctx context.Context, request resource.UpgradeStateRequest, response *resource.UpgradeStateResponse) {
			upgradeReleaseState(request, response, v0Type, currentType, nil, []string{"qualification_file", "qualification_sha256"}, bindingFields)
		}},
		1: {StateUpgrader: func(ctx context.Context, request resource.UpgradeStateRequest, response *resource.UpgradeStateResponse) {
			upgradeReleaseState(request, response, v1Type, currentType, nil, nil, bindingFields)
		}},
	}
}

func upgradeReleaseState(request resource.UpgradeStateRequest, response *resource.UpgradeStateResponse, previousType, currentType tftypes.Object, removed, added, upgradedBindingSets []string) {
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
	for _, name := range upgradedBindingSets {
		upgraded, err := upgradeWorkerBindingSet(values[name], currentType.AttributeTypes[name].(tftypes.Set))
		if err != nil {
			response.Diagnostics.AddError("Upgrade release state", "The previous Worker binding attributes could not be preserved.")
			return
		}
		values[name] = upgraded
	}
	dynamic, err := tfprotov6.NewDynamicValue(currentType, tftypes.NewValue(currentType, values))
	if err != nil {
		response.Diagnostics.AddError("Upgrade release state", "The preserved resource attributes could not be encoded.")
		return
	}
	response.DynamicValue = &dynamic
}
