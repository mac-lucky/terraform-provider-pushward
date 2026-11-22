package provider

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

type routeOrderResource struct{ c *client.Client }

type routeOrderModel struct {
	ID       types.String `tfsdk:"id"`
	RouteIDs types.List   `tfsdk:"route_ids"`
	OrgID    types.String `tfsdk:"org_id"`
}

func newRouteOrderResource() resource.Resource { return &routeOrderResource{} }

func (r *routeOrderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_route_order"
}

func (r *routeOrderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The order of the organization's routing rules; the first rule that matches decides. " +
			"Declare at most one. `route_ids` must name every rule of the organization exactly once, " +
			"rules made outside Terraform included. Destroying it leaves the order as it is.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The organization id.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"route_ids": schema.ListAttribute{
				MarkdownDescription: "Every rule's id, first match first.",
				ElementType:         types.StringType,
				Required:            true,
				Validators:          []validator.List{listvalidator.UniqueValues(), listvalidator.SizeAtMost(100), listvalidator.ValueStringsAre(lowerID)},
			},
			"org_id": orgIDAttribute(),
		},
	}
}

func (r *routeOrderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

// ImportState takes any id: the order belongs to the key's organization.
func (r *routeOrderResource) ImportState(ctx context.Context, _ resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.Set(ctx, &routeOrderModel{
		ID:       types.StringValue(r.c.OrgID),
		RouteIDs: types.ListNull(types.StringType),
		OrgID:    types.StringValue(r.c.OrgID),
	})...)
}

func routeIDs(routes []client.Route) types.List {
	vals := make([]attr.Value, 0, len(routes))
	for _, rt := range routes {
		vals = append(vals, types.StringValue(rt.ID))
	}
	return types.ListValueMust(types.StringType, vals)
}

// put saves plan's route_ids, followed by kept, as the order.
func (r *routeOrderResource) put(ctx context.Context, plan *routeOrderModel, kept []string, diags *diag.Diagnostics) {
	ids := []string{}
	diags.Append(plan.RouteIDs.ElementsAs(ctx, &ids, false)...)
	if diags.HasError() {
		return
	}
	ids = append(ids, kept...)
	var out struct {
		Items []client.Route `json:"items"`
	}
	if err := r.c.Do(ctx, http.MethodPut, r.c.OrgPath("routes", "order"), map[string]any{"route_ids": ids}, &out); err != nil {
		diags.AddError("Reordering the routing rules failed", r.orderMismatch(ctx, ids, err))
		return
	}
	plan.ID = types.StringValue(r.c.OrgID)
	plan.OrgID = types.StringValue(r.c.OrgID)
	plan.RouteIDs = routeIDs(out.Items)
}

func (r *routeOrderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan routeOrderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, &plan, nil, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *routeOrderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state routeOrderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	var out struct {
		Items []client.Route `json:"items"`
	}
	if err := r.c.Do(ctx, http.MethodGet, r.c.OrgPath("routes"), nil, &out); err != nil {
		resp.Diagnostics.AddError("Reading the routing rules failed", err.Error())
		return
	}
	state.ID = types.StringValue(r.c.OrgID)
	state.RouteIDs = routeIDs(out.Items)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *routeOrderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state routeOrderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	kept := r.kept(ctx, &plan, &state, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	planned := plan.RouteIDs
	ids := make([]string, 0, len(kept))
	names := make([]string, 0, len(kept))
	for _, rt := range kept {
		ids = append(ids, rt.ID)
		names = append(names, rt.ID+" ("+rt.SlugPattern+")")
	}
	r.put(ctx, &plan, ids, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	// The kept rules are past the end of the list only until their destroy.
	plan.RouteIDs = planned
	if len(kept) > 0 {
		resp.Diagnostics.AddWarning("Routing rules kept at the end of the order",
			"route_ids no longer names "+strings.Join(names, ", ")+"; they were put last for this apply, because a rule must "+
				"stay listed until it is destroyed. If this apply does not destroy them, add them back to route_ids.")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// kept is the rules the state lists and the plan no longer does that still
// exist. Terraform updates the order before it destroys a rule taken out of
// it, and the server refuses an order that leaves out a rule it has.
func (r *routeOrderResource) kept(ctx context.Context, plan, state *routeOrderModel, diags *diag.Diagnostics) []client.Route {
	var planned, prior []string
	diags.Append(plan.RouteIDs.ElementsAs(ctx, &planned, false)...)
	diags.Append(state.RouteIDs.ElementsAs(ctx, &prior, true)...)
	prior = slices.DeleteFunc(prior, func(id string) bool { return slices.Contains(planned, id) })
	if diags.HasError() || len(prior) == 0 {
		return nil
	}
	var out struct {
		Items []client.Route `json:"items"`
	}
	if err := r.c.Do(ctx, http.MethodGet, r.c.OrgPath("routes"), nil, &out); err != nil {
		diags.AddError("Reading the routing rules failed", err.Error())
		return nil
	}
	var kept []client.Route
	for _, id := range prior {
		if i := slices.IndexFunc(out.Items, func(rt client.Route) bool { return rt.ID == id }); i >= 0 {
			kept = append(kept, out.Items[i])
		}
	}
	return kept
}

// Delete only forgets the order: there is no order to remove, so it sends
// nothing, to any organization.
func (r *routeOrderResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}

// orderMismatch explains a 409 org.routes_changed. The server's advice is to
// reload and retry, but from Terraform the cause is a route_ids list that does
// not name every rule, so retrying never helps: name what is off instead.
func (r *routeOrderResource) orderMismatch(ctx context.Context, ids []string, err error) string {
	var e *client.Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict || e.Code != "org.routes_changed" {
		return err.Error()
	}
	var out struct {
		Items []client.Route `json:"items"`
	}
	if r.c.Do(ctx, http.MethodGet, r.c.OrgPath("routes"), nil, &out) != nil {
		return err.Error()
	}
	var missing, unknown []string
	for _, rt := range out.Items {
		if !slices.Contains(ids, rt.ID) {
			missing = append(missing, rt.ID+" ("+rt.SlugPattern+")")
		}
	}
	for _, id := range ids {
		if !slices.ContainsFunc(out.Items, func(rt client.Route) bool { return rt.ID == id }) {
			unknown = append(unknown, id)
		}
	}
	msg := "route_ids must name every routing rule of the organization exactly once, including rules made outside Terraform."
	if len(missing) > 0 {
		msg += " Missing: " + strings.Join(missing, ", ") + "."
	}
	if len(unknown) > 0 {
		msg += " Not a rule of the organization: " + strings.Join(unknown, ", ") + "."
	}
	return msg + " Server said: " + err.Error()
}
