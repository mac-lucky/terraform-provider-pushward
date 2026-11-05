// Package provider is the PushWard Terraform provider.
package provider

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

var _ provider.Provider = &pushwardProvider{}

type pushwardProvider struct {
	version string
}

type providerModel struct {
	APIKey types.String `tfsdk:"api_key"`
	APIURL types.String `tfsdk:"api_url"`
}

// New returns the provider factory for version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &pushwardProvider{version: version}
	}
}

func (p *pushwardProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "pushward"
	resp.Version = p.version
}

func (p *pushwardProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a PushWard organization: members, groups, device tags, routing rules and sending keys. " +
			"It authenticates with an org admin key (`hlo_...`), which an owner or admin creates in the " +
			"[PushWard console](https://console.pushward.app). The organization is the key's own.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				MarkdownDescription: "Org admin key (`hlo_...`). A `manage` key acts as an admin. A `read` key acts as a viewer: " +
					"it reads the organization, members, devices, groups, tags and routing rules, but not sending keys or open " +
					"invites, so a plan with a `pushward_org_key` or a pending `pushward_org_member` needs a `manage` key. " +
					"Defaults to the `PUSHWARD_ORG_API_TOKEN` environment variable.",
				Optional:  true,
				Sensitive: true,
			},
			"api_url": schema.StringAttribute{
				MarkdownDescription: "API base URL. Plain `http://` to anything but this machine gets a warning, since it sends the key unencrypted. " +
					"Defaults to `PUSHWARD_API_URL`, then `" + client.DefaultURL + "`.",
				Optional: true,
			},
		},
	}
}

func (p *pushwardProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.APIKey.IsUnknown() || cfg.APIURL.IsUnknown() {
		resp.Diagnostics.AddError("Provider configuration is not known yet",
			"api_key and api_url must be known when Terraform plans; they cannot come from a resource that is not created yet.")
		return
	}
	key := os.Getenv("PUSHWARD_ORG_API_TOKEN")
	if !cfg.APIKey.IsNull() {
		key = cfg.APIKey.ValueString()
	}
	baseURL := client.DefaultURL
	if v := os.Getenv("PUSHWARD_API_URL"); v != "" {
		baseURL = v
	}
	if !cfg.APIURL.IsNull() {
		baseURL = cfg.APIURL.ValueString()
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		resp.Diagnostics.AddAttributeError(path.Root("api_url"), "Invalid API URL",
			"api_url (or PUSHWARD_API_URL) must be an http or https URL such as "+client.DefaultURL+", got "+strconv.Quote(baseURL)+".")
		return
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		resp.Diagnostics.AddAttributeWarning(path.Root("api_url"), "API URL is not https",
			"The org admin key is sent unencrypted to "+u.Host+". Use https unless the server is on this machine.")
	}
	if key == "" {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Missing org admin key",
			"Set api_key or PUSHWARD_ORG_API_TOKEN to an org admin key (hlo_...). An owner or admin creates one in the PushWard console at https://console.pushward.app.")
		return
	}
	if !strings.HasPrefix(key, "hlo_") {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Not an org admin key",
			"This provider needs an org admin key, which starts with hlo_. An owner or admin creates one in the PushWard console at https://console.pushward.app. "+
				"hlk_ keys only send activities and notifications.")
		return
	}

	c := client.New(baseURL, key, p.version)
	var orgs struct {
		Items []client.Org `json:"items"`
	}
	if err := c.Do(ctx, http.MethodGet, "/orgs", nil, &orgs); err != nil {
		resp.Diagnostics.AddError("Could not read the key's organization", err.Error())
		return
	}
	if len(orgs.Items) != 1 {
		resp.Diagnostics.AddError("Expected exactly one organization",
			"GET /orgs returned "+strconv.Itoa(len(orgs.Items))+" organizations for this key; an org admin key belongs to exactly one.")
		return
	}
	c.OrgID = orgs.Items[0].ID
	resp.DataSourceData = c
	resp.ResourceData = c
}

func (p *pushwardProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newMemberResource,
		newGroupResource,
		newTagResource,
		newRouteResource,
		newRouteOrderResource,
		newKeyResource,
	}
}

func (p *pushwardProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newOrgDataSource,
		newMembersDataSource,
		newDevicesDataSource,
	}
}
