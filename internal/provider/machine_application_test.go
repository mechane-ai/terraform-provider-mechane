package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestMachineApplicationOwnership(t *testing.T) {
	for _, test := range []struct {
		name, policy, namespace string
		allowed                 bool
	}{
		{"owned", "durable_object", "namespace", true},
		{"foreign namespace", "durable_object", "someone-else", false},
		{"other scheduler", "default", "namespace", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{
					"id": "application", "name": "machines", "scheduling_policy": test.policy,
					"account_id": "account", "created_at": "2026-11-13T00:00:00Z", "updated_at": "2026-11-13T00:00:00Z",
					"durable_objects": map[string]any{"namespace_id": test.namespace},
				}})
			}))
			defer server.Close()
			owner := machineApplicationResource{client: cloudflare.NewClient(option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0))}
			_, err := owner.read(context.Background(), machineApplicationModel{
				AccountID: types.StringValue("account"), ID: types.StringValue("application"),
				Name: types.StringValue("machines"), NamespaceID: types.StringValue("namespace"),
			})
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v, error=%v", test.allowed, err)
			}
		})
	}
}

func TestMachineApplicationCreatesOnlyDurableObjectCompute(t *testing.T) {
	var created map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			w.Write([]byte(`{"success":true,"result":[]}`))
			return
		}
		if request.Method != http.MethodPost {
			t.Errorf("unexpected method %s", request.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&created); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"success":true,"result":{"id":"application"}}`))
	}))
	defer server.Close()
	owner := machineApplicationResource{client: cloudflare.NewClient(option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0))}
	var schema resource.SchemaResponse
	owner.Schema(context.Background(), resource.SchemaRequest{}, &schema)
	plan := tfsdk.Plan{Schema: schema.Schema}
	diagnostics := plan.Set(context.Background(), &machineApplicationModel{
		AccountID: types.StringValue("account"), ID: types.StringUnknown(),
		Name: types.StringValue("machines"), NamespaceID: types.StringValue("namespace"),
		DeleteProtection: types.BoolValue(true), LogsEnabled: types.BoolValue(true),
	})
	if diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	response := resource.CreateResponse{State: tfsdk.State{Schema: schema.Schema}}
	owner.Create(context.Background(), resource.CreateRequest{Plan: plan}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	if created["scheduling_policy"] != "durable_object" || created["name"] != "machines" {
		t.Fatalf("wrong compute scheduling request: %v", created)
	}
	objects, ok := created["durable_objects"].(map[string]any)
	if !ok || objects["namespace_id"] != "namespace" {
		t.Fatalf("wrong compute namespace: %v", created)
	}
	for _, field := range []string{"configuration", "instances", "max_instances"} {
		if _, present := created[field]; present {
			t.Errorf("scheduler-owned field %s escaped into Durable Object compute", field)
		}
	}
}

func TestMachineApplicationDeletionRequiresProtectionAndConfirmedAbsence(t *testing.T) {
	for _, protected := range []bool{true, false} {
		t.Run(map[bool]string{true: "protected", false: "retired"}[protected], func(t *testing.T) {
			reads, deletes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodDelete {
					deletes++
					w.Write([]byte(`{"success":true,"result":{}}`))
					return
				}
				reads++
				if deletes > 0 {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"absent"}]}`))
					return
				}
				w.Write([]byte(`{"success":true,"result":{"id":"application","account_id":"account","created_at":"2026-11-13T00:00:00Z","updated_at":"2026-11-13T00:00:00Z","name":"machines","scheduling_policy":"durable_object","durable_objects":{"namespace_id":"namespace"}}}`))
			}))
			defer server.Close()
			owner := machineApplicationResource{client: cloudflare.NewClient(option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0))}
			var schema resource.SchemaResponse
			owner.Schema(context.Background(), resource.SchemaRequest{}, &schema)
			state := tfsdk.State{Schema: schema.Schema}
			diagnostics := state.Set(context.Background(), &machineApplicationModel{
				AccountID: types.StringValue("account"), ID: types.StringValue("application"),
				Name: types.StringValue("machines"), NamespaceID: types.StringValue("namespace"),
				DeleteProtection: types.BoolValue(protected), LogsEnabled: types.BoolValue(true),
			})
			if diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			var response resource.DeleteResponse
			owner.Delete(context.Background(), resource.DeleteRequest{State: state}, &response)
			if protected {
				if !response.Diagnostics.HasError() || reads != 0 || deletes != 0 {
					t.Fatal("protected application reached provider deletion")
				}
			} else if response.Diagnostics.HasError() || deletes != 1 || reads != 2 {
				t.Fatalf("deletion lacked exact ownership/absence reads: reads=%d deletes=%d diagnostics=%v", reads, deletes, response.Diagnostics)
			}
		})
	}
}
