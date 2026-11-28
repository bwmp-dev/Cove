package provider

import (
	"context"
	"cove/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"os"
)

type coveProvider struct{}
type settings struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
	Team     types.String `tfsdk:"team"`
}

func New() provider.Provider { return &coveProvider{} }
func (*coveProvider) Metadata(_ context.Context, _ provider.MetadataRequest, response *provider.MetadataResponse) {
	response.TypeName = "cove"
	response.Version = "dev"
}
func (*coveProvider) Schema(_ context.Context, _ provider.SchemaRequest, response *provider.SchemaResponse) {
	response.Schema = schema.Schema{Description: "Manage Cove configuration through the versioned API. Deployment is explicit; this provider does not start or stop applications.", Attributes: map[string]schema.Attribute{
		"endpoint": schema.StringAttribute{Optional: true, Description: "HTTPS panel URL including its optional base path; defaults to COVE_ENDPOINT."},
		"token":    schema.StringAttribute{Optional: true, Sensitive: true, Description: "Scoped API token; defaults to COVE_API_TOKEN."},
		"team":     schema.StringAttribute{Optional: true, Description: "Selected team ID; defaults to COVE_TEAM."},
	}}
}
func (*coveProvider) Configure(ctx context.Context, request provider.ConfigureRequest, response *provider.ConfigureResponse) {
	var config settings
	response.Diagnostics.Append(request.Config.Get(ctx, &config)...)
	if response.Diagnostics.HasError() {
		return
	}
	if config.Endpoint.IsUnknown() || config.Token.IsUnknown() || config.Team.IsUnknown() {
		response.Diagnostics.AddError("Unknown provider setting", "Endpoint, token and team must be known before planning.")
		return
	}
	endpoint, token, team := config.Endpoint.ValueString(), config.Token.ValueString(), config.Team.ValueString()
	if endpoint == "" {
		endpoint = os.Getenv("COVE_ENDPOINT")
	}
	if token == "" {
		token = os.Getenv("COVE_API_TOKEN")
	}
	if team == "" {
		team = os.Getenv("COVE_TEAM")
	}
	api, err := client.New(endpoint, token, team)
	if err != nil {
		response.Diagnostics.AddError("Invalid Cove configuration", err.Error())
		return
	}
	response.ResourceData = api
}
func (*coveProvider) Resources(context.Context) []func() resource.Resource {
	result := []func() resource.Resource{}
	for _, kind := range []string{"project", "environment", "workload", "variable", "routes"} {
		result = append(result, func() resource.Resource { return &managedResource{kind: kind} })
	}
	return result
}
func (*coveProvider) DataSources(context.Context) []func() datasource.DataSource { return nil }
