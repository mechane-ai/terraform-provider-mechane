package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type mechaneProvider struct {
	version string
}

type providerModel struct {
	CloudflareAPIToken types.String `tfsdk:"cloudflare_api_token"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &mechaneProvider{version: version}
	}
}

func (p *mechaneProvider) Metadata(_ context.Context, _ provider.MetadataRequest, response *provider.MetadataResponse) {
	response.TypeName = "mechane"
	response.Version = p.version
}

func (p *mechaneProvider) Schema(_ context.Context, _ provider.SchemaRequest, response *provider.SchemaResponse) {
	response.Schema = schema.Schema{
		Description: "Worker releases and Cloudflare Containers lifecycle for Mechane installations.",
		Attributes: map[string]schema.Attribute{
			"cloudflare_api_token": schema.StringAttribute{
				Optional: true, Sensitive: true,
				Description: "Cloudflare API token. Prefer the CLOUDFLARE_API_TOKEN environment variable.",
			},
		},
	}
}

func (p *mechaneProvider) Configure(ctx context.Context, request provider.ConfigureRequest, response *provider.ConfigureResponse) {
	var config providerModel
	response.Diagnostics.Append(request.Config.Get(ctx, &config)...)
	if response.Diagnostics.HasError() {
		return
	}
	if config.CloudflareAPIToken.IsUnknown() {
		response.Diagnostics.AddError("Unknown Cloudflare credential", "Configure the provider with a known Cloudflare API token.")
		return
	}

	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if !config.CloudflareAPIToken.IsNull() {
		token = config.CloudflareAPIToken.ValueString()
	}
	if token == "" {
		response.Diagnostics.AddError("Missing Cloudflare credential", "Set CLOUDFLARE_API_TOKEN or cloudflare_api_token.")
		return
	}

	client := cloudflare.NewClient(
		option.WithAPIToken(token),
		option.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
		// A repeated POST can create another application or rollout after a lost response.
		option.WithMaxRetries(0),
	)
	response.ResourceData = client
	response.DataSourceData = client
}

func (p *mechaneProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{newContainerApplicationResource, newContainerImageResource, newWorkerReleaseResource}
}

func (p *mechaneProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

func describeCloudflareError(err error) string {
	var apiError *cloudflare.Error
	if errors.As(err, &apiError) {
		codes := make([]int64, 0, len(apiError.Errors))
		for _, failure := range apiError.Errors {
			codes = append(codes, failure.Code)
		}
		return fmt.Sprintf("Cloudflare returned HTTP %d with error codes %v. Check those codes and the account's Cloudflare diagnostics before retrying.", apiError.StatusCode, codes)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "The operation deadline expired. Refresh state and retry to resume the existing operation."
	}
	if errors.Is(err, context.Canceled) {
		return "The operation was canceled. Refresh state before retrying."
	}
	// SDK errors can contain request headers or response bodies.
	return "The Cloudflare request failed. Check connectivity and account access before retrying."
}

func isNotFound(err error) bool {
	var apiError *cloudflare.Error
	return errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound
}
