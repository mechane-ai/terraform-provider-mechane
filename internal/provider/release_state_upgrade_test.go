package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestReleaseStateUpgradeRetainsRemoteIdentities(t *testing.T) {
	for _, kind := range []string{"worker", "image"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var current resource.SchemaResponse
			var upgrader resource.StateUpgrader
			if kind == "worker" {
				instance := &workerReleaseResource{}
				instance.Schema(ctx, resource.SchemaRequest{}, &current)
				upgrader = instance.UpgradeState(ctx)[0]
			} else {
				instance := &containerImageResource{}
				instance.Schema(ctx, resource.SchemaRequest{}, &current)
				upgrader = instance.UpgradeState(ctx)[0]
			}
			currentType := current.Schema.Type().TerraformType(ctx).(tftypes.Object)
			previous := map[string]any{
				"id": "retained-resource", "account_id": "retained-account", "operation_timeout_seconds": 300,
			}
			if kind == "worker" {
				for name, value := range map[string]any{
					"worker_name": "retained-worker", "bundle_file": "worker.js", "bundle_sha256": nil,
					"release_manifest_file": "installation.json", "release_manifest_sha256": nil,
					"compatibility_date": "2026-10-28", "compatibility_flags": []string{"nodejs_compat"},
					"bindings": nil, "exports": map[string]any{"SandboxObject": map[string]string{"type": "durable-object", "storage": "sqlite", "state": "created"}},
					"durable_object_bindings": []map[string]any{{"name": "SANDBOXES", "type": "durable_object_namespace", "class_name": "SandboxObject", "text": nil, "bucket_name": nil, "jurisdiction": nil, "store_id": nil, "secret_name": nil}},
					"container_classes":       []string{"SandboxObject"}, "cpu_ms": 300000,
					"release_version": "0.27.1", "source_revision": nil, "traffic_percentage": 100,
				} {
					previous[name] = value
				}
				previous["durable_object_namespaces"] = map[string]string{"SandboxObject": "retained-namespace"}
				previous["version_id"] = "retained-version"
				previous["deployment_id"] = "retained-deployment"
			} else {
				previous["source_url"] = "https://example.com/previous.oci.tar"
				previous["image"] = "registry.cloudflare.com/retained-image"
				previous["repository"] = "retained-repository"
				previous["source_sha256"] = nil
				previous["digest"] = nil
			}
			content, err := json.Marshal(previous)
			if err != nil {
				t.Fatal(err)
			}
			response := resource.UpgradeStateResponse{}
			upgrader.StateUpgrader(ctx, resource.UpgradeStateRequest{RawState: &tfprotov6.RawState{JSON: content}}, &response)
			if response.Diagnostics.HasError() || response.DynamicValue == nil {
				t.Fatal(response.Diagnostics)
			}
			upgraded, err := response.DynamicValue.Unmarshal(currentType)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]tftypes.Value
			if err := upgraded.As(&values); err != nil {
				t.Fatal(err)
			}
			for name, value := range previous {
				if name != "source_url" {
					expected, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					previousValue, err := tftypes.ValueFromJSON(expected, currentType.AttributeTypes[name])
					if err != nil {
						t.Fatal(err)
					}
					if !values[name].Equal(previousValue) {
						t.Fatalf("upgrade changed %s", name)
					}
				}
			}
			if kind == "worker" && !values["qualification_sha256"].IsNull() {
				t.Fatal("upgrade invented qualification")
			}
			if kind == "image" && !values["source_file"].IsNull() {
				t.Fatal("upgrade invented a qualified archive path")
			}
		})
	}
}
