package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"golang.org/x/net/idna"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

type domainResource struct{ c *client.Client }

type domainModel struct {
	ID         types.String `tfsdk:"id"`
	Domain     types.String `tfsdk:"domain"`
	Status     types.String `tfsdk:"status"`
	TXTName    types.String `tfsdk:"txt_name"`
	TXTValue   types.String `tfsdk:"txt_value"`
	VerifiedAt types.String `tfsdk:"verified_at"`
	OrgID      types.String `tfsdk:"org_id"`
}

func newDomainResource() resource.Resource { return &domainResource{} }

func (r *domainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_domain"
}

func (r *domainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An email domain the organization claims, for single sign-on. Enterprise plan only.\n\n" +
			"It starts `pending`. Publish a TXT record named `txt_name` with the value `txt_value`, then verify it with " +
			"`pushward_org_domain_verification`. A domain is verified by one organization at most, the first to verify it. " +
			"PushWard looks the record up again every day and sets the domain back to `pending` after seven missed checks in a row, " +
			"so keep the record; `status` then shows it, and replacing the verification (`terraform apply -replace`) verifies " +
			"the domain again. Destroying it removes the domain; while single sign-on is on, PushWard refuses that for the " +
			"organization's last verified domain.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"domain": schema.StringAttribute{
				MarkdownDescription: "The domain, such as `example.com`. Not an IP address and not a public suffix such as `com` or `co.uk`. " +
					"PushWard stores it lowercase, without a trailing dot, an internationalized domain in punycode (`xn--...`); " +
					"the state keeps it as written here. Changing it to another domain replaces the domain; another spelling of " +
					"the same one changes nothing in PushWard.",
				Required: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIf(otherDomain,
					"Another domain replaces it.", "Another domain replaces it.")},
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "`pending` or `verified`.",
				Computed:            true,
			},
			"txt_name": schema.StringAttribute{
				MarkdownDescription: "The DNS name of the TXT record that verifies the domain.",
				Computed:            true,
				// Fixed for a domain id, so a spelling update does not re-plan the DNS record.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"txt_value": schema.StringAttribute{
				MarkdownDescription: "The TXT record's value.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"verified_at": schema.StringAttribute{
				MarkdownDescription: "When it was verified, RFC 3339; null while pending.",
				Computed:            true,
			},
			"org_id": orgIDAttribute(),
		},
	}
}

// canonicalDomain is s as PushWard stores it: trimmed, lowercase, without a
// trailing dot, an IDN in punycode. A non-hostname comes back trimmed and
// lowercase, for PushWard to refuse with its own message.
func canonicalDomain(s string) string {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if ascii, err := idna.Registration.ToASCII(d); err == nil {
		return ascii
	}
	return d
}

// otherDomain replaces the domain for another domain, not for another
// spelling of the same one.
func otherDomain(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	resp.RequiresReplace = canonicalDomain(req.StateValue.ValueString()) != canonicalDomain(req.PlanValue.ValueString())
}

func (r *domainResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

// findDomain lists the organization's domains and returns the first match,
// or nil.
func findDomain(ctx context.Context, c *client.Client, match func(*client.Domain) bool) (*client.Domain, error) {
	var out struct {
		Domains []client.Domain `json:"domains"`
	}
	if err := c.Do(ctx, http.MethodGet, c.OrgPath("domains"), nil, &out); err != nil {
		return nil, err
	}
	for i := range out.Domains {
		if match(&out.Domains[i]) {
			return &out.Domains[i], nil
		}
	}
	return nil, nil
}

func domainWithID(id string) func(*client.Domain) bool {
	return func(d *client.Domain) bool { return d.ID == id }
}

func domainNamed(name string) func(*client.Domain) bool {
	return func(d *client.Domain) bool { return d.Domain == name }
}

// ImportState takes the domain's id, or the domain itself.
func (r *domainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !strings.Contains(req.ID, ".") {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
		return
	}
	name := canonicalDomain(req.ID)
	d, err := findDomain(ctx, r.c, domainNamed(name))
	if err != nil {
		resp.Diagnostics.AddError("Listing the domains failed", err.Error())
		return
	}
	if d == nil {
		resp.Diagnostics.AddError("The organization has no such domain", "The organization has no domain "+name+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), d.ID)...)
}

// set copies d into m. The domain stays as m has it while that is a spelling
// of d's.
func (m *domainModel) set(d *client.Domain) {
	m.ID = types.StringValue(d.ID)
	if canonicalDomain(m.Domain.ValueString()) != d.Domain {
		m.Domain = types.StringValue(d.Domain)
	}
	m.Status = types.StringValue(d.Status)
	m.TXTName = types.StringValue(d.TXTName)
	m.TXTValue = types.StringValue(d.TXTValue)
	m.VerifiedAt = timePointer(d.VerifiedAt)
}

func (r *domainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan domainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.OrgID = types.StringValue(r.c.OrgID)
	name := canonicalDomain(plan.Domain.ValueString())
	exists := func() {
		resp.Diagnostics.AddError("The organization already has this domain",
			"The organization already has the domain "+name+". Import it with `terraform import <address> "+name+"`.")
	}
	existing, err := findDomain(ctx, r.c, domainNamed(name))
	if err != nil {
		resp.Diagnostics.AddError("Listing the domains failed", err.Error())
		return
	}
	if existing != nil {
		exists()
		return
	}
	var d client.Domain
	err = r.c.Do(ctx, http.MethodPost, r.c.OrgPath("domains"), map[string]any{"domain": name}, &d)
	// PushWard dedupes no domain create by Idempotency-Key, so a repeat after
	// an attempt that may have made the domain meets 409 org.domain_exists for
	// it. On the first attempt the 409 is someone else's: a console add since
	// the list, or another resource with the same domain.
	// shortcut: a repeat that raced another create of the same domain adopts
	// that one; telling them apart needs the server to dedupe domain creates.
	var apiErr *client.Error
	if errors.As(err, &apiErr) && apiErr.Code == client.CodeDomainExists {
		if !apiErr.Repeated {
			exists()
			return
		}
		if found, ferr := findDomain(ctx, r.c, domainNamed(name)); ferr == nil && found != nil {
			d, err = *found, nil
		}
	}
	if err != nil {
		resp.Diagnostics.AddError("Adding the domain failed", err.Error())
		return
	}
	plan.set(&d)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *domainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	d, err := findDomain(ctx, r.c, domainWithID(state.ID.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Reading the domain failed", err.Error())
		return
	}
	if d == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	state.set(d)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update only takes another spelling of the same domain into the state:
// another domain replaces it, and the rest is computed.
func (r *domainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state domainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	state.Domain = plan.Domain
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *domainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	err := r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("domains", state.ID.ValueString()), nil, nil)
	if err != nil && !client.IsGone(err, client.CodeDomainNotFound) {
		resp.Diagnostics.AddError("Removing the domain failed", err.Error())
	}
}
