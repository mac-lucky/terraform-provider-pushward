package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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

type groupResource struct{ c *client.Client }

type groupModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	MemberIDs   types.Set    `tfsdk:"member_ids"`
	OrgID       types.String `tfsdk:"org_id"`
}

func newGroupResource() resource.Resource { return &groupResource{} }

func (r *groupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_group"
}

func (r *groupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A group of organization members. Routing rules and sending keys reach members by group.\n\n" +
			"`member_ids` takes user ids. A member who was invited by email has none until they join, so build the list with " +
			"`compact()` over `pushward_org_member.*.user_id`: they are added on the first apply after they join.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Unique in the organization: 1-40 lowercase letters, digits, `-` and `_`.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.RegexMatches(routingName, routingNameRule)},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Up to 200 characters.",
				Optional:            true,
				Validators:          []validator.String{stringvalidator.UTF8LengthBetween(1, 200), trimmed},
			},
			"member_ids": schema.SetAttribute{
				MarkdownDescription: "User ids of every member of the group. The set replaces the current members; empty by default.",
				ElementType:         types.StringType,
				Optional:            true,
				Computed:            true,
				Default:             setdefault.StaticValue(emptySet()),
				Validators:          []validator.Set{setvalidator.ValueStringsAre(lowerID)},
			},
			"org_id": orgIDAttribute(),
		},
	}
}

func (r *groupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

func (r *groupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (m *groupModel) set(g *client.Group) {
	m.ID = types.StringValue(g.ID)
	m.Name = types.StringValue(g.Name)
	m.Description = types.StringPointerValue(g.Description)
	m.MemberIDs = stringSet(g.MemberIDs)
}

func (r *groupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	members := setStrings(ctx, plan.MemberIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.OrgID = types.StringValue(r.c.OrgID)
	var g client.Group
	body := map[string]any{"name": plan.Name.ValueString(), "description": plan.Description.ValueStringPointer()}
	if err := r.c.Do(ctx, http.MethodPost, r.c.OrgPath("groups"), body, &g); err != nil {
		createFailed(&resp.Diagnostics, "group", plan.Name.ValueString(), err)
		return
	}
	plan.set(&g)
	// Saved before the members are set, so a failure leaves the group tainted
	// in state instead of orphaned.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if len(members) == 0 {
		return
	}
	if err := r.c.Do(ctx, http.MethodPut, r.c.OrgPath("groups", g.ID, "members"), map[string]any{"user_ids": members}, &g); err != nil {
		resp.Diagnostics.AddError("Setting the group's members failed", err.Error())
		return
	}
	plan.set(&g)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	var g client.Group
	err := r.c.Do(ctx, http.MethodGet, r.c.OrgPath("groups", state.ID.ValueString()), nil, &g)
	if client.IsGone(err, client.CodeGroupNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the group failed", err.Error())
		return
	}
	state.set(&g)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *groupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	members := setStrings(ctx, plan.MemberIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	plan.OrgID = state.OrgID
	id := state.ID.ValueString()
	var g client.Group
	if !plan.Name.Equal(state.Name) || !plan.Description.Equal(state.Description) {
		body := map[string]any{"name": plan.Name.ValueString(), "description": clearable(plan.Description)}
		if err := r.c.Do(ctx, http.MethodPatch, r.c.OrgPath("groups", id), body, &g); err != nil {
			resp.Diagnostics.AddError("Updating the group failed", err.Error())
			return
		}
	}
	if !plan.MemberIDs.Equal(state.MemberIDs) {
		if err := r.c.Do(ctx, http.MethodPut, r.c.OrgPath("groups", id, "members"), map[string]any{"user_ids": members}, &g); err != nil {
			resp.Diagnostics.AddError("Setting the group's members failed", err.Error())
			return
		}
	}
	if g.ID != "" {
		plan.set(&g)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	err := r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("groups", state.ID.ValueString()), nil, nil)
	if err != nil && !client.IsGone(err, client.CodeGroupNotFound) {
		resp.Diagnostics.AddError("Deleting the group failed", err.Error())
	}
}
