package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

type orgSettingsResource struct{ c *client.Client }

type orgSettingsModel struct {
	ID    types.String `tfsdk:"id"`
	Name  types.String `tfsdk:"name"`
	OrgID types.String `tfsdk:"org_id"`
}

func newOrgSettingsResource() resource.Resource { return &orgSettingsResource{} }

func (r *orgSettingsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_settings"
}

func (r *orgSettingsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The settings of the provider key's organization; today its name. Declare at most one. " +
			"Creating it adopts the organization as it is and sets the name; destroying it sends nothing and leaves the " +
			"name as it is. Needs a `manage` key.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The organization id.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "The organization's display name, 1-50 characters.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.UTF8LengthBetween(1, 50), trimmed},
			},
			"org_id": orgIDAttribute(),
		},
	}
}

func (r *orgSettingsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

// ImportState takes any id: the settings belong to the key's organization.
func (r *orgSettingsResource) ImportState(ctx context.Context, _ resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.Set(ctx, &orgSettingsModel{
		ID:    types.StringValue(r.c.OrgID),
		Name:  types.StringNull(),
		OrgID: types.StringValue(r.c.OrgID),
	})...)
}

func (r *orgSettingsResource) patch(ctx context.Context, plan *orgSettingsModel, diags *diag.Diagnostics) {
	var o client.Org
	if err := r.c.Do(ctx, http.MethodPatch, r.c.OrgPath(), map[string]any{"name": plan.Name.ValueString()}, &o); err != nil {
		diags.AddError("Renaming the organization failed", err.Error())
		return
	}
	plan.ID = types.StringValue(r.c.OrgID)
	plan.Name = types.StringValue(o.Name)
}

func (r *orgSettingsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan orgSettingsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.OrgID = types.StringValue(r.c.OrgID)
	r.patch(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *orgSettingsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state orgSettingsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	var o client.Org
	if err := r.c.Do(ctx, http.MethodGet, r.c.OrgPath(), nil, &o); err != nil {
		resp.Diagnostics.AddError("Reading the organization failed", err.Error())
		return
	}
	state.ID = types.StringValue(r.c.OrgID)
	state.Name = types.StringValue(o.Name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *orgSettingsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state orgSettingsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	plan.OrgID = state.OrgID
	r.patch(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

// Delete only forgets the settings: the organization keeps its name, and
// nothing is sent, to any organization.
func (r *orgSettingsResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}
