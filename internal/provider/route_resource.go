package provider

import (
	"context"
	"net/http"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

var slugPattern = regexp.MustCompile(`^(\*|[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}\*?)$`)

type routeResource struct{ c *client.Client }

type routeModel struct {
	ID            types.String `tfsdk:"id"`
	SlugPattern   types.String `tfsdk:"slug_pattern"`
	AppliesTo     types.String `tfsdk:"applies_to"`
	AllowGroupIDs types.Set    `tfsdk:"allow_group_ids"`
	AllowTagIDs   types.Set    `tfsdk:"allow_tag_ids"`
	Note          types.String `tfsdk:"note"`
	OrgID         types.String `tfsdk:"org_id"`
}

func newRouteResource() resource.Resource { return &routeResource{} }

func (r *routeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_route"
}

func (r *routeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A routing rule: who receives the organization's activities, notifications or widgets whose slug matches. " +
			"Rules are tried in order and the first match decides; with no match everyone receives it. " +
			"A new rule goes after the existing ones, and `pushward_org_route_order` sets the order.\n\n" +
			"Every change takes effect at once: the server re-applies the rules to ongoing Live Activities itself, " +
			"so there is no separate apply step.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"slug_pattern": schema.StringAttribute{
				MarkdownDescription: "`*`, a slug, or a slug prefix ending in `*`. Activities and widgets match their slug, " +
					"notifications their `activity_slug`, else their source.",
				Required:   true,
				Validators: []validator.String{stringvalidator.RegexMatches(slugPattern, "must be *, a slug, or a slug prefix ending in * (letters, digits, - and _)")},
			},
			"applies_to": schema.StringAttribute{
				MarkdownDescription: "`activities`, `notifications`, `both` (activities and notifications) or `widgets`.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.OneOf("activities", "notifications", "both", "widgets")},
			},
			"allow_group_ids": schema.SetAttribute{
				MarkdownDescription: "Members of these groups receive what the rule matches. At most 50.",
				ElementType:         types.StringType,
				Optional:            true,
				Computed:            true,
				Default:             setdefault.StaticValue(emptySet()),
				Validators:          []validator.Set{setvalidator.SizeAtMost(50), setvalidator.ValueStringsAre(lowerID)},
			},
			"allow_tag_ids": schema.SetAttribute{
				MarkdownDescription: "Devices with these tags receive what the rule matches. At most 50. " +
					"With both lists empty the rule reaches nobody.",
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Default:     setdefault.StaticValue(emptySet()),
				Validators:  []validator.Set{setvalidator.SizeAtMost(50), setvalidator.ValueStringsAre(lowerID)},
			},
			"note": schema.StringAttribute{
				MarkdownDescription: "Up to 200 characters.",
				Optional:            true,
				Validators:          []validator.String{stringvalidator.UTF8LengthBetween(1, 200), trimmed},
			},
			"org_id": orgIDAttribute(),
		},
	}
}

func (r *routeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

func (r *routeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (m *routeModel) set(rt *client.Route) {
	m.ID = types.StringValue(rt.ID)
	m.SlugPattern = types.StringValue(rt.SlugPattern)
	m.AppliesTo = types.StringValue(rt.AppliesTo)
	m.AllowGroupIDs = stringSet(rt.AllowGroupIDs)
	m.AllowTagIDs = stringSet(rt.AllowTagIDs)
	m.Note = types.StringPointerValue(rt.Note)
}

func (m *routeModel) body(ctx context.Context, diags *diag.Diagnostics) map[string]any {
	return map[string]any{
		"slug_pattern":    m.SlugPattern.ValueString(),
		"applies_to":      m.AppliesTo.ValueString(),
		"allow_group_ids": setStrings(ctx, m.AllowGroupIDs, diags),
		"allow_tag_ids":   setStrings(ctx, m.AllowTagIDs, diags),
	}
}

func (r *routeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan routeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	body := plan.body(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	body["note"] = plan.Note.ValueStringPointer()
	plan.OrgID = types.StringValue(r.c.OrgID)
	var rt client.Route
	if err := r.c.Do(ctx, http.MethodPost, r.c.OrgPath("routes"), body, &rt); err != nil {
		resp.Diagnostics.AddError("Creating the routing rule failed", err.Error())
		return
	}
	plan.set(&rt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *routeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state routeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	var rt client.Route
	err := r.c.Do(ctx, http.MethodGet, r.c.OrgPath("routes", state.ID.ValueString()), nil, &rt)
	if client.IsGone(err, client.CodeRouteNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the routing rule failed", err.Error())
		return
	}
	state.set(&rt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *routeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state routeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	body := plan.body(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	plan.OrgID = state.OrgID
	body["note"] = clearable(plan.Note)
	var rt client.Route
	if err := r.c.Do(ctx, http.MethodPatch, r.c.OrgPath("routes", state.ID.ValueString()), body, &rt); err != nil {
		resp.Diagnostics.AddError("Updating the routing rule failed", err.Error())
		return
	}
	plan.set(&rt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *routeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state routeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	err := r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("routes", state.ID.ValueString()), nil, nil)
	if err != nil && !client.IsGone(err, client.CodeRouteNotFound) {
		resp.Diagnostics.AddError("Deleting the routing rule failed", err.Error())
	}
}
