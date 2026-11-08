package provider

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

type ssoResource struct{ c *client.Client }

type ssoModel struct {
	ID              types.String  `tfsdk:"id"`
	Protocol        types.String  `tfsdk:"protocol"`
	Enabled         types.Bool    `tfsdk:"enabled"`
	RequireSSO      types.Bool    `tfsdk:"require_sso"`
	OIDC            *ssoOIDCModel `tfsdk:"oidc"`
	SAML            *ssoSAMLModel `tfsdk:"saml"`
	RedirectURI     types.String  `tfsdk:"redirect_uri"`
	SPEntityID      types.String  `tfsdk:"sp_entity_id"`
	ACSURL          types.String  `tfsdk:"acs_url"`
	SPMetadataURL   types.String  `tfsdk:"sp_metadata_url"`
	IdPEntityID     types.String  `tfsdk:"idp_entity_id"`
	IdPFingerprints types.List    `tfsdk:"idp_certificate_fingerprints"`
	OrgID           types.String  `tfsdk:"org_id"`
}

type ssoOIDCModel struct {
	Issuer         types.String `tfsdk:"issuer"`
	ClientID       types.String `tfsdk:"client_id"`
	ClientSecret   types.String `tfsdk:"client_secret"`
	ClientSecretWO types.String `tfsdk:"client_secret_wo"`
	WOVersion      types.Int64  `tfsdk:"client_secret_wo_version"`
}

type ssoSAMLModel struct {
	MetadataXML types.String `tfsdk:"idp_metadata_xml"`
	MetadataURL types.String `tfsdk:"idp_metadata_url"`
}

var (
	_ resource.ResourceWithValidateConfig = &ssoResource{}
	_ resource.ResourceWithModifyPlan     = &ssoResource{}
)

func newSSOResource() resource.Resource { return &ssoResource{} }

func (r *ssoResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_sso_connection"
}

func computedString(doc string, stable bool) schema.StringAttribute {
	a := schema.StringAttribute{MarkdownDescription: doc, Computed: true}
	if stable {
		a.PlanModifiers = []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	}
	return a
}

func (r *ssoResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	sibling := func(name string) path.Expression { return path.MatchRelative().AtParent().AtName(name) }
	resp.Schema = schema.Schema{
		MarkdownDescription: "The organization's single sign-on connection, OpenID Connect or SAML 2.0. Enterprise plan only; " +
			"declare at most one.\n\n" +
			"Only an owner sets it up or changes its identity provider (the protocol, the OIDC issuer or client_id, the SAML " +
			"entity ID, SSO URL or signing certificates), and an org admin key acts as an admin, so PushWard refuses those " +
			"changes from Terraform with a 403. An owner sets it up in the [PushWard console](https://console.pushward.app); " +
			"then `terraform import` brings it in, and Terraform manages `enabled` and `require_sso`, and can roll the OIDC " +
			"client secret or save the same SAML metadata again. Creating the resource while the connection already exists " +
			"with the same identity provider adopts it the same way.\n\n" +
			"PushWard never returns the OIDC client secret. Terraform sends it when it changes, and when the connection is " +
			"created or its provider changes, and does not notice a secret changed in the console. `client_secret_wo` " +
			"(Terraform 1.11 or newer) keeps it out of the plan and the state, and is the form to prefer. The SAML metadata is " +
			"sent the same way: a change that leaves it alone keeps the stored identity provider and asks it nothing, so " +
			"`enabled` and `require_sso` can be turned off while the metadata URL is down. With neither metadata attribute " +
			"(`saml = {}`) none is ever sent, and Terraform manages an existing SAML connection's `enabled` and `require_sso` " +
			"only: also one set up from an uploaded document, or whose metadata URL already lists the next certificate.\n\n" +
			"Turning it on needs a verified domain, so in the apply that verifies one, make it `depends_on` the " +
			"`pushward_org_domain_verification`. That verification stays in the state, also when the domain lapses back to " +
			"`pending`, so later changes do not wait for it; one Terraform has to create again (its domain was removed or " +
			"replaced) holds them up until it is through or times out. To change the connection first, say to turn " +
			"`require_sso` off, take the `depends_on` out for that apply: `-target` brings the dependencies along. While no " +
			"domain is verified, PushWard refuses `enabled = true`.\n\n" +
			"Destroying it removes the connection; the console sessions it signed in stay valid until they expire.",
		Attributes: map[string]schema.Attribute{
			"id": computedString("The organization id.", true),
			"protocol": schema.StringAttribute{
				MarkdownDescription: "`oidc` or `saml`, with the block of the same name. Switching it takes an owner.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.OneOf("oidc", "saml")},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether people sign in with it. Turning it on needs a verified domain.",
				Required:            true,
			},
			"require_sso": schema.BoolAttribute{
				MarkdownDescription: "Console sessions of members other than owners must have signed in with it. Owners are " +
					"exempt, so an identity provider outage cannot lock the organization out. Defaults to `false`.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
			},
			"oidc": schema.SingleNestedAttribute{
				MarkdownDescription: "The OpenID Connect identity provider, with protocol `oidc`.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"issuer": schema.StringAttribute{
						MarkdownDescription: "The issuer, an https URL; PushWard reads its discovery document when it is set or changed. " +
							"Changing it takes an owner.",
						Required: true,
					},
					"client_id": schema.StringAttribute{
						MarkdownDescription: "Changing it takes an owner.",
						Required:            true,
					},
					"client_secret": schema.StringAttribute{
						MarkdownDescription: "The client secret, kept in the state as configured. Left out, PushWard keeps the stored one.",
						Optional:            true,
						Sensitive:           true,
						Validators:          []validator.String{stringvalidator.ConflictsWith(sibling("client_secret_wo"))},
					},
					"client_secret_wo": schema.StringAttribute{
						MarkdownDescription: "The client secret, write-only: never in the plan or the state. Sent when the connection " +
							"is created and when `client_secret_wo_version` changes. Needs Terraform 1.11 or newer.",
						Optional:   true,
						Sensitive:  true,
						WriteOnly:  true,
						Validators: []validator.String{stringvalidator.AlsoRequires(sibling("client_secret_wo_version"))},
					},
					"client_secret_wo_version": schema.Int64Attribute{
						MarkdownDescription: "Change it to send `client_secret_wo` again, such as after rolling the secret at the identity provider.",
						Optional:            true,
						Validators:          []validator.Int64{int64validator.AlsoRequires(sibling("client_secret_wo"))},
					},
				},
			},
			"saml": schema.SingleNestedAttribute{
				MarkdownDescription: "The SAML 2.0 identity provider's metadata, with protocol `saml`: `idp_metadata_xml` or " +
					"`idp_metadata_url`, or neither to keep the identity provider PushWard has stored. Metadata with another " +
					"entity ID, SSO URL or other signing certificates takes an owner.",
				Optional: true,
				Attributes: map[string]schema.Attribute{
					"idp_metadata_xml": schema.StringAttribute{
						MarkdownDescription: "The metadata document, one EntityDescriptor (`file()` helps). PushWard does not return it.",
						Optional:            true,
						Validators:          []validator.String{stringvalidator.ConflictsWith(sibling("idp_metadata_url"))},
					},
					"idp_metadata_url": schema.StringAttribute{
						MarkdownDescription: "An https URL PushWard reads the metadata from, once per change. Left out, and " +
							"without `idp_metadata_xml`, it shows the URL the stored metadata was read from, if any.",
						Optional:      true,
						Computed:      true,
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
						Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
					},
				},
			},
			"redirect_uri":    computedString("The redirect URI to register at an OIDC identity provider.", true),
			"sp_entity_id":    computedString("The entity ID (audience) to register at a SAML identity provider.", true),
			"acs_url":         computedString("The assertion consumer service URL (HTTP-POST) to register at a SAML identity provider.", true),
			"sp_metadata_url": computedString("The URL of PushWard's service provider metadata, which it serves once the connection is SAML.", true),
			"idp_entity_id":   computedString("The SAML identity provider's entity ID, from its metadata.", false),
			"idp_certificate_fingerprints": schema.ListAttribute{
				MarkdownDescription: "The SHA-256 fingerprints of the SAML identity provider's signing certificates, colon-separated upper case hex.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"org_id": orgIDAttribute(),
		},
	}
}

// ValidateConfig pairs the protocol with its block.
func (r *ssoResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var protocol types.String
	var oidc, saml types.Object
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("protocol"), &protocol)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("oidc"), &oidc)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("saml"), &saml)...)
	if resp.Diagnostics.HasError() || protocol.IsNull() || protocol.IsUnknown() {
		return
	}
	p := protocol.ValueString()
	for _, b := range []struct {
		name string
		v    types.Object
	}{{"oidc", oidc}, {"saml", saml}} {
		switch {
		case b.v.IsUnknown():
		case p == b.name && b.v.IsNull():
			resp.Diagnostics.AddAttributeError(path.Root(b.name), "Missing "+b.name, "protocol "+p+" needs the "+b.name+" settings.")
		case p != b.name && !b.v.IsNull():
			resp.Diagnostics.AddAttributeError(path.Root(b.name), "Unexpected "+b.name, b.name+" goes with protocol "+b.name+", not "+p+".")
		}
	}
}

// ModifyPlan plans no idp_metadata_url next to a metadata document, for which
// PushWard keeps none, and keeps the identity provider's entity ID and
// fingerprints for a change with the same protocol that sends no metadata:
// PushWard keeps the stored provider.
func (r *ssoResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var protocol types.String
	var planSAML types.Object
	d := &resp.Diagnostics
	d.Append(req.Plan.GetAttribute(ctx, path.Root("protocol"), &protocol)...)
	d.Append(req.Plan.GetAttribute(ctx, path.Root("saml"), &planSAML)...)
	if d.HasError() || planSAML.IsUnknown() {
		return
	}
	var saml *ssoSAMLModel
	if !planSAML.IsNull() {
		saml = &ssoSAMLModel{}
		if d.Append(planSAML.As(ctx, saml, basetypes.ObjectAsOptions{})...); d.HasError() {
			return
		}
		if !saml.MetadataXML.IsNull() {
			saml.MetadataURL = types.StringNull()
			if saml.MetadataXML.IsUnknown() {
				saml.MetadataURL = types.StringUnknown()
			}
			d.Append(resp.Plan.SetAttribute(ctx, path.Root("saml").AtName("idp_metadata_url"), saml.MetadataURL)...)
		}
	}
	if req.State.Raw.IsNull() {
		return
	}
	var state ssoModel
	d.Append(req.State.Get(ctx, &state)...)
	if d.HasError() || !protocol.Equal(state.Protocol) || (saml != nil && sendsMetadata(saml, state.SAML)) {
		return
	}
	d.Append(resp.Plan.SetAttribute(ctx, path.Root("idp_entity_id"), state.IdPEntityID)...)
	d.Append(resp.Plan.SetAttribute(ctx, path.Root("idp_certificate_fingerprints"), state.IdPFingerprints)...)
}

func (r *ssoResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

// ImportState takes the organization id: the connection belongs to the key's
// organization.
func (r *ssoResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !strings.EqualFold(req.ID, r.c.OrgID) {
		resp.Diagnostics.AddError("Import the connection by its organization id",
			"The connection belongs to the API key's organization, "+r.c.OrgID+", not "+strconv.Quote(req.ID)+
				". Import it with `terraform import <address> "+r.c.OrgID+"`.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), r.c.OrgID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("org_id"), r.c.OrgID)...)
}

// set copies conn into m. What PushWard does not return, the client secret
// and the SAML metadata document, stays as m has it.
func (m *ssoModel) set(conn *client.SSOConnection) {
	m.Protocol = types.StringValue(conn.Protocol)
	m.Enabled = types.BoolValue(conn.Enabled)
	m.RequireSSO = types.BoolValue(conn.RequireSSO)
	m.RedirectURI = types.StringValue(conn.RedirectURI)
	m.SPEntityID, m.ACSURL, m.SPMetadataURL = types.StringNull(), types.StringNull(), types.StringNull()
	m.IdPEntityID, m.IdPFingerprints = types.StringNull(), types.ListNull(types.StringType)
	if s := conn.SAML; s != nil {
		m.SPEntityID, m.ACSURL, m.SPMetadataURL = types.StringValue(s.SPEntityID), types.StringValue(s.ACSURL), types.StringValue(s.SPMetadataURL)
	}
	oidc, saml := m.OIDC, m.SAML
	m.OIDC, m.SAML = nil, nil
	switch {
	case conn.Protocol == "oidc" && conn.OIDC != nil:
		m.OIDC = &ssoOIDCModel{
			Issuer:         types.StringValue(conn.OIDC.Issuer),
			ClientID:       types.StringValue(conn.OIDC.ClientID),
			ClientSecret:   types.StringNull(),
			ClientSecretWO: types.StringNull(),
			WOVersion:      types.Int64Null(),
		}
		if oidc != nil {
			m.OIDC.ClientSecret, m.OIDC.WOVersion = oidc.ClientSecret, oidc.WOVersion
		}
	case conn.Protocol == "saml" && conn.SAML != nil:
		m.SAML = &ssoSAMLModel{MetadataXML: types.StringNull(), MetadataURL: types.StringNull()}
		if conn.SAML.IdPMetadataURL != "" {
			m.SAML.MetadataURL = types.StringValue(conn.SAML.IdPMetadataURL)
		}
		if saml != nil {
			m.SAML.MetadataXML = saml.MetadataXML
		}
		m.IdPEntityID = types.StringValue(conn.SAML.IdPEntityID)
		prints := make([]attr.Value, 0, len(conn.SAML.IdPCertificates))
		for _, c := range conn.SAML.IdPCertificates {
			prints = append(prints, types.StringValue(c.SHA256Fingerprint))
		}
		m.IdPFingerprints = types.ListValueMust(types.StringType, prints)
	}
}

// ssoSends is what a save of plan over state sends besides the protocol and
// the switches: the client secret (nil keeps the stored one) and whether the
// SAML metadata goes too (else PushWard keeps the stored provider). Each goes
// when it changed, and with a new connection or identity provider; the
// metadata only when the plan has some. wo is client_secret_wo from the
// configuration. A create passes an empty state.
func ssoSends(plan, state *ssoModel, wo types.String) (secret *string, metadata bool) {
	fresh := !plan.Protocol.Equal(state.Protocol)
	if o := plan.OIDC; o != nil {
		s := state.OIDC
		fresh = fresh || s == nil || !o.Issuer.Equal(s.Issuer) || !o.ClientID.Equal(s.ClientID)
		switch {
		case !o.ClientSecret.IsNull() && (fresh || !o.ClientSecret.Equal(s.ClientSecret)):
			v := o.ClientSecret.ValueString()
			secret = &v
		case !wo.IsNull() && (fresh || !o.WOVersion.Equal(s.WOVersion)):
			v := wo.ValueString()
			secret = &v
		}
	}
	if p := plan.SAML; p != nil {
		s := state.SAML
		if fresh {
			s = nil
		}
		metadata = sendsMetadata(p, s)
	}
	return secret, metadata
}

// sendsMetadata reports whether a save of p over s, the state's SAML settings
// (nil for none), sends the metadata: one of the two is set, and the pair
// differs from s. A URL still unknown is one the configuration leaves out.
// Neither set keeps the stored identity provider.
func sendsMetadata(p, s *ssoSAMLModel) bool {
	set := !p.MetadataXML.IsNull() || (!p.MetadataURL.IsNull() && !p.MetadataURL.IsUnknown())
	return set && (s == nil || !p.MetadataXML.Equal(s.MetadataXML) || !p.MetadataURL.Equal(s.MetadataURL))
}

// ssoBody is the PUT body for plan.
func ssoBody(plan *ssoModel, secret *string, metadata bool) map[string]any {
	body := map[string]any{
		"protocol":    plan.Protocol.ValueString(),
		"enabled":     plan.Enabled.ValueBool(),
		"require_sso": plan.RequireSSO.ValueBool(),
	}
	if o := plan.OIDC; o != nil {
		oidc := map[string]any{"issuer": o.Issuer.ValueString(), "client_id": o.ClientID.ValueString()}
		if secret != nil {
			oidc["client_secret"] = *secret
		}
		body["oidc"] = oidc
	}
	if s := plan.SAML; s != nil {
		saml := map[string]any{}
		switch {
		case !metadata:
		case !s.MetadataXML.IsNull():
			saml["idp_metadata_xml"] = s.MetadataXML.ValueString()
		case s.MetadataURL.ValueString() != "":
			saml["idp_metadata_url"] = s.MetadataURL.ValueString()
		}
		body["saml"] = saml
	}
	return body
}

// put saves plan over state and copies the result into plan.
func (r *ssoResource) put(ctx context.Context, config tfsdk.Config, plan, state *ssoModel, diags *diag.Diagnostics) {
	var wo types.String
	if plan.OIDC != nil {
		diags.Append(config.GetAttribute(ctx, path.Root("oidc").AtName("client_secret_wo"), &wo)...)
	}
	if diags.HasError() {
		return
	}
	secret, metadata := ssoSends(plan, state, wo)
	var conn client.SSOConnection
	if err := r.c.Do(ctx, http.MethodPut, r.c.OrgPath("sso"), ssoBody(plan, secret, metadata), &conn); err != nil {
		if client.HasCode(err, client.CodePermissionDenied) {
			diags.AddError("PushWard refused the single sign-on change",
				err.Error()+"\n\nOnly an owner sets up single sign-on or changes its identity provider (the protocol, the OIDC "+
					"issuer or client_id, the SAML entity ID, SSO URL or signing certificates), and an org admin key acts as an "+
					"admin at most (a read key as a viewer, which changes nothing). Have an owner make the change in the "+
					"PushWard console at https://console.pushward.app, then import the connection with "+
					"`terraform import <address> "+r.c.OrgID+"`. Terraform then manages enabled and require_sso, and can "+
					"roll the OIDC client secret of the same provider.")
			return
		}
		diags.AddError("Saving single sign-on failed", err.Error())
		return
	}
	plan.set(&conn)
	plan.ID = types.StringValue(r.c.OrgID)
}

func (r *ssoResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan ssoModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.OrgID = types.StringValue(r.c.OrgID)
	r.put(ctx, req.Config, &plan, &ssoModel{}, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *ssoResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state ssoModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	var conn client.SSOConnection
	if err := r.c.Do(ctx, http.MethodGet, r.c.OrgPath("sso"), nil, &conn); err != nil {
		resp.Diagnostics.AddError("Reading single sign-on failed", err.Error())
		return
	}
	if !conn.Configured {
		resp.State.RemoveResource(ctx)
		return
	}
	state.set(&conn)
	state.ID = types.StringValue(r.c.OrgID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *ssoResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state ssoModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	plan.OrgID = state.OrgID
	r.put(ctx, req.Config, &plan, &state, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *ssoResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state ssoModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	err := r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("sso"), nil, nil)
	if err != nil && !client.IsGone(err, client.CodeSSONotFound) {
		resp.Diagnostics.AddError("Removing single sign-on failed", err.Error())
	}
}
