package provider

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/containers"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type imageTestTransport func(*http.Request) (*http.Response, error)

func (f imageTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type imageFixture struct {
	Resource     *containerImageResource
	Model        imageModel
	Preparations atomic.Int32
}

func newImageFixture(t *testing.T, existing bool, prepare func(int32, string) containers.ImagePrepareResponse) *imageFixture {
	t.Helper()
	configuration, err := empty.Image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	configuration.OS = "linux"
	configuration.Architecture = "amd64"
	image, err := mutate.ConfigFile(empty.Image, configuration)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	archive := writeImageArchiveFixture(t, image)
	content, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &imageFixture{Model: imageModel{
		AccountID: types.StringValue(strings.Repeat("a", 32)), Repository: types.StringValue("guest"),
		SourceFile: types.StringValue(archive), SourceSHA256: types.StringValue(workerDigest(content)),
		Digest: types.StringValue(digest.String()), OperationTimeoutSeconds: types.Int64Value(120),
	}}
	fixture.Model.ID = types.StringValue(fixture.Model.AccountID.ValueString() + "/guest@" + digest.String())
	fixture.Model.Image = types.StringValue(imageReference(fixture.Model))
	registryHandler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/v2/") {
			registryHandler.ServeHTTP(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch request.URL.Path {
		case "/accounts/" + fixture.Model.AccountID.ValueString() + "/containers/registries/registry.cloudflare.com/credentials":
			result = map[string]string{"registry_host": "registry.cloudflare.com", "username": "fixture", "password": "fixture"}
		case "/accounts/" + fixture.Model.AccountID.ValueString() + "/containers/image-preparations":
			var body struct {
				Image string `json:"image"`
			}
			if request.Method != http.MethodPost || json.NewDecoder(request.Body).Decode(&body) != nil || body.Image != imageReference(fixture.Model) {
				t.Error("preparation did not select the exact immutable image")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			prepared := prepare(fixture.Preparations.Add(1), body.Image)
			if prepared.Status == containers.ImagePrepareResponseStatusPending {
				w.WriteHeader(http.StatusAccepted)
			}
			result = prepared
		default:
			t.Errorf("unexpected image fixture endpoint %s", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	previous := remote.DefaultTransport
	remote.DefaultTransport = imageTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "registry.cloudflare.com" {
			request = request.Clone(request.Context())
			request.URL.Scheme = endpoint.Scheme
			request.URL.Host = endpoint.Host
			request.Host = endpoint.Host
		}
		return previous.RoundTrip(request)
	})
	t.Cleanup(func() { remote.DefaultTransport = previous })
	fixture.Resource = &containerImageResource{client: cloudflare.NewClient(option.WithBaseURL(server.URL+"/"), option.WithAPIToken("fixture"), option.WithMaxRetries(0))}
	if existing {
		tag, err := name.NewTag("registry.cloudflare.com/" + fixture.Model.AccountID.ValueString() + "/guest:existing")
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(tag, image); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func writeImageArchiveFixture(t *testing.T, image v1.Image) string {
	t.Helper()
	directory := t.TempDir()
	oci, err := layout.Write(directory, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := oci.AppendImage(image); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "image.tar"))
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name, err = filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = writer.Write(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func writeImageFixture(t *testing.T, fixture *imageFixture, ctx context.Context, update bool) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	var schemaResponse resource.SchemaResponse
	fixture.Resource.Schema(ctx, resource.SchemaRequest{}, &schemaResponse)
	plan := tfsdk.Plan{Schema: schemaResponse.Schema}
	if diagnostics := plan.Set(ctx, &fixture.Model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	state := tfsdk.State{Schema: schemaResponse.Schema}
	if update {
		prior := fixture.Model
		prior.OperationTimeoutSeconds = types.Int64Value(60)
		if diagnostics := state.Set(ctx, &prior); diagnostics.HasError() {
			t.Fatal(diagnostics)
		}
		response := resource.UpdateResponse{State: state}
		fixture.Resource.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &response)
		return response.State, response.Diagnostics
	}
	response := resource.CreateResponse{State: state}
	fixture.Resource.Create(ctx, resource.CreateRequest{Plan: plan}, &response)
	return response.State, response.Diagnostics
}

func readyImage(_ int32, image string) containers.ImagePrepareResponse {
	return containers.ImagePrepareResponse{Image: image, Status: containers.ImagePrepareResponseStatusReady, ArtifactDigest: "sha256:" + strings.Repeat("b", 64)}
}

func TestImageWritesPrepareUploadedAndExistingImages(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		existing bool
		update   bool
	}{{"upload", false, false}, {"existing manifest", true, false}, {"update existing manifest", true, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newImageFixture(t, scenario.existing, readyImage)
			state, diagnostics := writeImageFixture(t, fixture, context.Background(), scenario.update)
			if diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			var installed imageModel
			if diagnostics := state.Get(context.Background(), &installed); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			if installed.Image.ValueString() != imageReference(fixture.Model) || fixture.Preparations.Load() == 0 {
				t.Fatal("an image was accepted without preparing its exact runtime artifact")
			}
		})
	}
}

func TestImagePreparationWaitsForReady(t *testing.T) {
	fixture := newImageFixture(t, false, func(call int32, image string) containers.ImagePrepareResponse {
		if call == 1 {
			return containers.ImagePrepareResponse{Image: image, Status: containers.ImagePrepareResponseStatusPending}
		}
		return readyImage(call, image)
	})
	state, diagnostics := writeImageFixture(t, fixture, context.Background(), false)
	if diagnostics.HasError() || state.Raw.IsNull() || fixture.Preparations.Load() != 2 {
		t.Fatal("image installation accepted pending preparation or did not observe readiness", diagnostics)
	}
}

func TestImageWritesRejectFailedOrMismatchedPreparation(t *testing.T) {
	for _, scenario := range []string{"terminal error", "wrong image", "missing artifact", "unknown status"} {
		for _, update := range []bool{false, true} {
			operation := "create"
			if update {
				operation = "update"
			}
			t.Run(scenario+"/"+operation, func(t *testing.T) {
				fixture := newImageFixture(t, true, func(call int32, image string) containers.ImagePrepareResponse {
					result := readyImage(call, image)
					switch scenario {
					case "terminal error":
						result.Status = containers.ImagePrepareResponseStatusError
						result.Reason = "runtime conversion failed"
					case "wrong image":
						result.Image += "-other"
					case "missing artifact":
						result.ArtifactDigest = ""
					case "unknown status":
						result.Status = "unrecognized"
					}
					return result
				})
				state, diagnostics := writeImageFixture(t, fixture, context.Background(), update)
				if !diagnostics.HasError() || fixture.Preparations.Load() == 0 {
					t.Fatal("unready runtime preparation was accepted")
				}
				if update {
					var retained imageModel
					if diagnostics := state.Get(context.Background(), &retained); diagnostics.HasError() {
						t.Fatal(diagnostics)
					}
					if retained.OperationTimeoutSeconds.ValueInt64() != 60 || retained.ID != fixture.Model.ID {
						t.Fatal("failed preparation replaced the existing image state")
					}
				} else if !state.Raw.IsNull() {
					t.Fatal("failed image creation accepted resource state")
				}
			})
		}
	}
}

func TestImagePreparationHonorsTheOperationDeadline(t *testing.T) {
	fixture := newImageFixture(t, true, func(_ int32, image string) containers.ImagePrepareResponse {
		return containers.ImagePrepareResponse{Image: image, Status: containers.ImagePrepareResponseStatusPending}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, diagnostics := writeImageFixture(t, fixture, ctx, false)
	if !diagnostics.HasError() || !state.Raw.IsNull() || fixture.Preparations.Load() == 0 || ctx.Err() != context.DeadlineExceeded {
		t.Fatal("pending preparation escaped its operation deadline or accepted ready state", diagnostics)
	}
}

func TestImageRefreshDoesNotStartPreparation(t *testing.T) {
	fixture := newImageFixture(t, true, readyImage)
	var schemaResponse resource.SchemaResponse
	fixture.Resource.Schema(context.Background(), resource.SchemaRequest{}, &schemaResponse)
	state := tfsdk.State{Schema: schemaResponse.Schema}
	if diagnostics := state.Set(context.Background(), &fixture.Model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	response := resource.ReadResponse{State: state}
	fixture.Resource.Read(context.Background(), resource.ReadRequest{State: state}, &response)
	if response.Diagnostics.HasError() || response.State.Raw.IsNull() || fixture.Preparations.Load() != 0 {
		t.Fatal("refresh changed preparation or lost the existing registry image", response.Diagnostics)
	}
}
