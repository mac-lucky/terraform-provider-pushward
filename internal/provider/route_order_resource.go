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
				Validators:          []validator.List{listvalidator.UniqueValues(), listvalidator.SizeAtMost(100)},
			},
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
	})...)
}

func routeIDs(routes []client.Route) types.List {
	vals := make([]attr.Value, 0, len(routes))
	for _, rt := range routes {
		vals = append(vals, types.StringValue(rt.ID))
	}
	return types.ListValueMust(types.StringType, vals)
}

func (r *routeOrderResource) put(ctx context.Context, plan *routeOrderModel, diags *diag.Diagnostics) {
	ids := []string{}
	diags.Append(plan.RouteIDs.ElementsAs(ctx, &ids, false)...)
	if diags.HasError() {
		return
	}
	var out struct {
		Items []client.Route `json:"items"`
	}
	if err := r.c.Do(ctx, http.MethodPut, r.c.OrgPath("routes", "order"), map[string]any{"route_ids": ids}, &out); err != nil {
		diags.AddError("Reordering the routing rules failed", r.orderMismatch(ctx, ids, err))
		return
	}
	plan.ID = types.StringValue(r.c.OrgID)
	plan.RouteIDs = routeIDs(out.Items)
}

func (r *routeOrderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan routeOrderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *routeOrderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state routeOrderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
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
	var plan routeOrderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

// Delete only forgets the order: there is no order to remove.
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
