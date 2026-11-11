package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type workerReleaseFixture struct {
	Model    workerReleaseModel
	Manifest workerReleaseManifest
	Root     string
}

func writeReleaseFixtureFile(t *testing.T, root, path string, content []byte) qualifiedFile {
	t.Helper()
	fullPath := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, content, 0600); err != nil {
		t.Fatal(err)
	}
	return qualifiedFile{Path: path, SHA256: workerDigest(content), Bytes: int64(len(content))}
}

func writeReleaseFixtureJSON(t *testing.T, root, path string, value any) qualifiedFile {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writeReleaseFixtureFile(t, root, path, content)
}

func newWorkerReleaseFixture(t *testing.T) workerReleaseFixture {
	t.Helper()
	root := t.TempDir()
	main := writeReleaseFixtureFile(t, root, "worker.js", []byte("export default {fetch() { return new Response('application') }};"))
	wasm := writeReleaseFixtureFile(t, root, "sqlite.wasm", []byte{0, 97, 115, 109, 1, 0, 0, 0})
	console := writeReleaseFixtureFile(t, root, "console/index.html", []byte("<!doctype html><title>Console</title>"))
	manifest := map[string]any{
		"format_version": 2, "is_preview": false, "version": "0.28.0", "repository": "mechane-ai/mechane", "public_commit": strings.Repeat("a", 40),
		"worker": map[string]any{
			"path": main.Path, "sha256": main.SHA256, "bytes": main.Bytes,
			"compatibility_date": "2026-11-09", "compatibility_flags": []string{"nodejs_compat"}, "container_classes": []string{"SandboxObject"},
			"triggers": workerTriggers{Crons: []string{"* * * * *"}},
			"bindings": []map[string]string{{"name": "SANDBOXES", "class_name": "SandboxObject"}, {"name": "MECHS", "class_name": "MechObject"}},
			"exports":  map[string]workerExport{"SandboxObject": {Type: "durable-object", Storage: "sqlite", State: "created"}, "MechObject": {Type: "durable-object", Storage: "sqlite", State: "created"}},
			"modules":  []workerArtifact{{Path: main.Path, SHA256: main.SHA256, Bytes: main.Bytes, ContentType: "application/javascript+module"}, {Path: wasm.Path, SHA256: wasm.SHA256, Bytes: wasm.Bytes, ContentType: "application/wasm"}},
			"assets":   workerAssetManifest{Directory: "console", Binding: "ASSETS", Files: []workerArtifact{{Path: "index.html", SHA256: console.SHA256, Bytes: console.Bytes, ContentType: "text/html"}}, Config: workerAssetConfig{HTMLHandling: "none", NotFoundHandling: "none", RunWorkerFirst: true}},
		},
		"formats": workerFormats{Wire: map[string][]string{"sandbox": {"historical-wire", "current-wire"}}, Migrations: map[string][]string{"control": {"historical-migration", "current-migration"}}},
	}
	installation := writeReleaseFixtureJSON(t, root, "installation.json", manifest)
	inventory := writeReleaseFixtureJSON(t, root, "artifacts.json", map[string]any{
		"format_version": 1, "version": "0.28.0", "repository": "mechane-ai/mechane", "public_commit": strings.Repeat("a", 40),
		"assets": []map[string]any{{"name": installation.Path, "sha256": installation.SHA256, "bytes": installation.Bytes}},
	})
	input := writeReleaseFixtureFile(t, root, "input.txt", []byte("pinned qualification input"))
	inputs := map[string][]qualifiedFile{"toolchain": {input}, "fixtures": {input}, "policy": {input}, "configuration": {input}}
	inputsDigest, err := qualificationInputsDigest(inputs)
	if err != nil {
		t.Fatal(err)
	}
	checks := []map[string]any{}
	for _, name := range []string{"complete", "artifact-scenarios", "shipped-formats"} {
		evidence := writeReleaseFixtureJSON(t, root, name+".json", workerCheckEvidence{FormatVersion: 1, Name: name, Outcome: "passed", PublicCommit: strings.Repeat("a", 40), Artifacts: inventory, Installation: installation, InputsSHA256: inputsDigest})
		checks = append(checks, map[string]any{"name": name, "outcome": "passed", "evidence": evidence})
	}
	qualification := writeReleaseFixtureJSON(t, root, "qualification.json", map[string]any{
		"format_version": 1, "version": "0.28.0", "public_commit": strings.Repeat("a", 40), "artifacts": inventory, "installation": installation,
		"inputs": inputs,
		"checks": checks,
	})
	bindings, diagnostics := types.SetValueFrom(context.Background(), workerBindingObjectType(), []workerBindingModel{})
	if diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	model := workerReleaseModel{AccountID: types.StringValue("account"), WorkerName: types.StringValue("retained-worker"),
		ReleaseManifestFile: types.StringValue(filepath.Join(root, installation.Path)), ReleaseManifestSHA256: types.StringValue(installation.SHA256),
		QualificationFile: types.StringValue(filepath.Join(root, qualification.Path)), QualificationSHA256: types.StringValue(qualification.SHA256),
		Bindings: bindings, CPUMilliseconds: types.Int64Value(300000), OperationTimeoutSeconds: types.Int64Value(30),
	}
	decoded, err := readWorkerManifest(model)
	if err != nil {
		t.Fatal(err)
	}
	return workerReleaseFixture{Model: model, Manifest: decoded, Root: root}
}

type workerCloudflareFixture struct {
	Mutex                  sync.Mutex
	Requests               int
	Writes                 int
	Version                *workerVersion
	Deployment             workerDeployment
	Manifest               map[string]workerAssetEntry
	Uploaded               map[string][]byte
	MissingCompletion      bool
	UnexpectedClass        bool
	RebindSandboxNamespace bool
	MechNamespaceExists    bool
	FailVersionRead        bool
	FailDeploymentRead     bool
	LoseVersionResponse    bool
	VersionUploads         int
	DeploymentWrites       int
	StaleActivationReads   int
}

func workerFixtureClient(t *testing.T, state *workerCloudflareFixture) *workerReleaseResource {
	t.Helper()
	state.Uploaded = map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		state.Mutex.Lock()
		defer state.Mutex.Unlock()
		state.Requests++
		if request.Method == http.MethodPost {
			state.Writes++
		}
		response.Header().Set("Content-Type", "application/json")
		path := request.URL.Path
		var result any
		switch {
		case strings.HasSuffix(path, "/assets-upload-session"):
			var payload struct {
				Manifest map[string]workerAssetEntry `json:"manifest"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			state.Manifest = payload.Manifest
			hashes := []string{}
			for _, entry := range payload.Manifest {
				hashes = append(hashes, entry.Hash)
			}
			result = workerAssetSession{JWT: "upload-token", Buckets: [][]string{hashes}}
		case strings.HasSuffix(path, "/assets/upload"):
			if request.Header.Get("Authorization") != "Bearer upload-token" || request.URL.Query().Get("base64") != "true" {
				t.Error("asset upload did not use its scoped token and encoding")
			}
			reader, err := request.MultipartReader()
			if err != nil {
				t.Error(err)
				break
			}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					break
				}
				content, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
				if err != nil {
					t.Error(err)
				}
				if part.Header.Get("Content-Type") != "text/html" {
					t.Error("Console asset lost its content type")
				}
				state.Uploaded[part.FormName()] = content
			}
			jwt := "completion-token"
			if state.MissingCompletion {
				jwt = ""
			}
			result = map[string]string{"jwt": jwt}
		case strings.HasSuffix(path, "/durable_objects/namespaces"):
			class := "SandboxObject"
			if state.UnexpectedClass {
				class = "RetainedHistoryObject"
			}
			namespaces := []map[string]string{{"id": "existing-sandbox-namespace", "class": class, "script": "retained-worker"}}
			if state.MechNamespaceExists {
				namespaces = append(namespaces, map[string]string{"id": "namespace-MechObject", "class": "MechObject", "script": "retained-worker"})
			}
			result = namespaces
			if request.URL.Query().Get("page") == "2" {
				result = []map[string]string{}
			}
		case strings.HasSuffix(path, "/deployments") && request.Method == http.MethodPost:
			state.DeploymentWrites++
			if err := json.NewDecoder(request.Body).Decode(&state.Deployment); err != nil {
				t.Error(err)
			}
			state.Deployment.ID = "deployment"
			for index := range state.Version.Bindings {
				if state.Version.Bindings[index].Type == "durable_object_namespace" && state.Version.Bindings[index].NamespaceID == "" {
					state.Version.Bindings[index].NamespaceID = "namespace-" + *state.Version.Bindings[index].ClassName
				}
			}
			state.MechNamespaceExists = true
			result = state.Deployment
		case strings.HasSuffix(path, "/deployments"):
			if state.Version != nil && state.FailDeploymentRead {
				response.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(response, `{"success":false,"errors":[{"code":10000,"message":"private-error-sentinel"}]}`)
				return
			}
			deployments := []workerDeployment{}
			if state.Version != nil && state.StaleActivationReads > 0 {
				state.StaleActivationReads--
			} else if state.Deployment.ID != "" {
				deployments = append(deployments, state.Deployment)
			}
			result = map[string]any{"deployments": deployments}
		case strings.HasSuffix(path, "/versions") && request.Method == http.MethodPost:
			state.VersionUploads++
			deploy := request.URL.Query().Get("deploy") == "true"
			var version workerVersion
			if err := json.NewDecoder(request.Body).Decode(&version); err != nil {
				t.Error(err)
			}
			for _, binding := range version.Bindings {
				if binding.Type == "durable_object_namespace" && *binding.ClassName == "MechObject" && !state.MechNamespaceExists && !deploy {
					response.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(response, `{"success":false,"errors":[{"code":100406,"message":"Actor binding requires a provisioned namespace"}]}`)
					return
				}
			}
			if version.Assets == nil || version.Assets.JWT != "completion-token" {
				t.Error("version did not attach confirmed assets")
			}
			version.ID = "version"
			for index := range version.Bindings {
				if version.Bindings[index].Type == "durable_object_namespace" && *version.Bindings[index].ClassName == "SandboxObject" {
					version.Bindings[index].NamespaceID = "existing-sandbox-namespace"
					if state.RebindSandboxNamespace {
						version.Bindings[index].NamespaceID = "replacement-sandbox-namespace"
					}
				}
				if version.Bindings[index].Type == "durable_object_namespace" && *version.Bindings[index].ClassName == "MechObject" && (state.MechNamespaceExists || deploy) {
					version.Bindings[index].NamespaceID = "namespace-MechObject"
				}
			}
			state.Version = &version
			if deploy {
				state.Deployment = workerDeployment{ID: "deployment", Versions: []workerTraffic{{VersionID: version.ID, Percentage: 100}}}
				state.MechNamespaceExists = true
			}
			if state.LoseVersionResponse {
				response.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(response, `{"success":false,"errors":[{"code":10000,"message":"Response lost after acceptance"}]}`)
				return
			}
			result = version
		case strings.HasSuffix(path, "/versions/version"):
			if state.FailVersionRead {
				response.Header().Set("CF-Ray", "0123456789abcdef-SJC")
				response.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(response, `{"success":false,"errors":[{"code":10000,"message":"private-error-sentinel"}]}`)
				return
			}
			result = state.Version
		case strings.HasSuffix(path, "/versions"):
			result = []workerVersion{}
			if state.Version != nil {
				result = []workerVersion{*state.Version}
			}
		default:
			t.Errorf("unexpected fixture request %s %s", request.Method, path)
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(response).Encode(map[string]any{"success": true, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client := cloudflare.NewClient(option.WithBaseURL(server.URL+"/"), option.WithAPIToken("account-token"), option.WithMaxRetries(0))
	return &workerReleaseResource{client: client}
}

func TestWorkerReleaseUploadsQualifiedModulesAndAssets(t *testing.T) {
	fixture := newWorkerReleaseFixture(t)
	state := &workerCloudflareFixture{}
	resource := workerFixtureClient(t, state)
	result, err := resource.applyRelease(context.Background(), fixture.Model)
	if err != nil {
		t.Fatal(err)
	}
	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	if result.TrafficPercentage.ValueFloat64() != 100 || len(state.Version.Modules) != 2 || state.Version.Assets.Config.RunWorkerFirst != true {
		t.Fatal("qualified application modules and assets were not exclusively activated")
	}
	entry, found := state.Manifest["/index.html"]
	if !found || string(state.Uploaded[entry.Hash]) != "<!doctype html><title>Console</title>" {
		t.Fatal("activated Console asset bytes differ from the qualified artifact")
	}
	hasAssets := false
	for _, binding := range state.Version.Bindings {
		if binding.Name == "ASSETS" && binding.Type == "assets" {
			hasAssets = true
		}
	}
	if !hasAssets {
		t.Fatal("application has no ASSETS binding")
	}
	namespaces := result.DurableObjectNamespaces.Elements()
	if len(namespaces) != 2 {
		t.Fatal("application lost its Mech or Sandbox namespace")
	}
	if !namespaces["SandboxObject"].Equal(types.StringValue("existing-sandbox-namespace")) {
		t.Fatal("application replaced the existing Sandbox namespace")
	}
}

func TestWorkerReleaseRefusesExistingNamespaceRebinding(t *testing.T) {
	fixture := newWorkerReleaseFixture(t)
	state := &workerCloudflareFixture{RebindSandboxNamespace: true, MechNamespaceExists: true}
	resource := workerFixtureClient(t, state)
	if _, err := resource.applyRelease(context.Background(), fixture.Model); err == nil {
		t.Fatal("a changed Sandbox namespace identity was accepted")
	}
	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	if state.Deployment.ID != "" {
		t.Fatal("a changed Sandbox namespace identity was activated")
	}
}

func TestWorkerReleaseRetainsAcceptedActivationAfterReadFailure(t *testing.T) {
	for _, scenario := range []struct {
		name                string
		deploymentReadFails bool
	}{{"observed deployment", false}, {"unobserved deployment", true}} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkerReleaseFixture(t)
			fixture.Model.DurableObjectNamespaces = types.MapUnknown(types.StringType)
			state := &workerCloudflareFixture{FailVersionRead: true, FailDeploymentRead: scenario.deploymentReadFails}
			resource := workerFixtureClient(t, state)
			result, err := resource.applyRelease(context.Background(), fixture.Model)
			if err == nil || result.VersionID.ValueString() != "version" {
				t.Fatal("accepted version was lost when its follow-up read failed")
			}
			if scenario.deploymentReadFails {
				if !result.DeploymentID.IsNull() || !result.TrafficPercentage.IsNull() {
					t.Fatal("an unobserved deployment was recorded as absent or active")
				}
			} else if result.DeploymentID.ValueString() != "deployment" || result.TrafficPercentage.ValueFloat64() != 100 {
				t.Fatal("observed activation was lost when its version read failed")
			}
			if strings.Contains(err.Error(), "private-error-sentinel") {
				t.Fatal("Cloudflare response details reached the provider diagnostic")
			}
			if result.DurableObjectNamespaces.IsUnknown() {
				t.Fatal("accepted activation returned unknown namespace state")
			}
		})
	}
}

func TestWorkerReleaseReconcilesUncertainActivationWithoutRepeatingUpload(t *testing.T) {
	fixture := newWorkerReleaseFixture(t)
	state := &workerCloudflareFixture{LoseVersionResponse: true, StaleActivationReads: 2}
	resource := workerFixtureClient(t, state)
	result, err := resource.applyRelease(context.Background(), fixture.Model)
	if err != nil {
		t.Fatal(err)
	}
	if result.TrafficPercentage.ValueFloat64() != 100 {
		t.Fatal("an accepted activation was not reconciled")
	}
	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	if state.VersionUploads != 1 || state.DeploymentWrites != 0 {
		t.Fatal("an uncertain atomic activation repeated its upload or deployment")
	}
}

func TestWorkerReleaseRejectsChangedQualifiedBytesBeforeRequests(t *testing.T) {
	for _, path := range []string{"worker.js", "sqlite.wasm", "console/index.html", "input.txt", "complete.json", "qualification.json"} {
		t.Run(path, func(t *testing.T) {
			fixture := newWorkerReleaseFixture(t)
			content, err := os.ReadFile(filepath.Join(fixture.Root, path))
			if err != nil {
				t.Fatal(err)
			}
			if path == "qualification.json" {
				content = append([]byte(" "), content...)
			} else {
				content[0] ^= 1
			}
			writeReleaseFixtureFile(t, fixture.Root, path, content)
			state := &workerCloudflareFixture{}
			resource := workerFixtureClient(t, state)
			if _, err := resource.applyRelease(context.Background(), fixture.Model); err == nil {
				t.Fatal("changed qualified bytes were accepted")
			}
			state.Mutex.Lock()
			defer state.Mutex.Unlock()
			if state.Requests != 0 {
				t.Fatal("changed qualified bytes reached the Cloudflare API")
			}
		})
	}
}

func TestWorkerReleaseRetainsDeploymentWhenAssetUploadIsUnconfirmed(t *testing.T) {
	fixture := newWorkerReleaseFixture(t)
	state := &workerCloudflareFixture{MissingCompletion: true}
	resource := workerFixtureClient(t, state)
	if _, err := resource.applyRelease(context.Background(), fixture.Model); err == nil {
		t.Fatal("an unconfirmed asset set was accepted")
	}
	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	if state.Version != nil || state.Deployment.ID != "" {
		t.Fatal("unconfirmed assets changed the Worker deployment")
	}
}

func TestWorkerReleaseRejectsEvidenceForDifferentInputs(t *testing.T) {
	fixture := newWorkerReleaseFixture(t)
	content, err := os.ReadFile(filepath.Join(fixture.Root, "qualification.json"))
	if err != nil {
		t.Fatal(err)
	}
	var qualification workerQualification
	if err := json.Unmarshal(content, &qualification); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(filepath.Join(fixture.Root, "complete.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence workerCheckEvidence
	if err := json.Unmarshal(content, &evidence); err != nil {
		t.Fatal(err)
	}
	evidence.InputsSHA256 = strings.Repeat("b", 64)
	entry := writeReleaseFixtureJSON(t, fixture.Root, "complete.json", evidence)
	for index := range qualification.Checks {
		if qualification.Checks[index].Name == "complete" {
			qualification.Checks[index].Evidence = entry
		}
	}
	updated := writeReleaseFixtureJSON(t, fixture.Root, "qualification.json", qualification)
	fixture.Model.QualificationSHA256 = types.StringValue(updated.SHA256)
	state := &workerCloudflareFixture{}
	instance := workerFixtureClient(t, state)
	if _, err := instance.applyRelease(context.Background(), fixture.Model); err == nil {
		t.Fatal("check evidence for different inputs qualified a candidate")
	}
	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	if state.Requests != 0 {
		t.Fatal("stale evidence reached the Cloudflare API")
	}
}

func TestWorkerReleasePreservesExistingNamespacesAndFormats(t *testing.T) {
	fixture := newWorkerReleaseFixture(t)
	state := &workerCloudflareFixture{UnexpectedClass: true}
	resource := workerFixtureClient(t, state)
	if _, err := resource.applyRelease(context.Background(), fixture.Model); err == nil {
		t.Fatal("a release omitted an existing Durable Object namespace")
	}
	state.Mutex.Lock()
	if state.Writes != 0 {
		t.Fatal("namespace preservation failed after an API write")
	}
	state.Mutex.Unlock()
	previous := fixture.Manifest.Formats
	desired := workerFormats{Wire: map[string][]string{"sandbox": {"current-wire"}}, Migrations: previous.Migrations}
	if err := preserveWorkerFormats(previous, desired); err == nil {
		t.Fatal("a release removed a historical wire entry")
	}
}
