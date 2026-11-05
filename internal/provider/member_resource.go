package provider

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

const (
	statusPending = "pending"
	statusJoined  = "joined"
)

var noUpper = regexp.MustCompile(`^[^A-Z]*$`)

type memberResource struct{ c *client.Client }

type memberModel struct {
	ID       types.String `tfsdk:"id"`
	Email    types.String `tfsdk:"email"`
	Role     types.String `tfsdk:"role"`
	UserID   types.String `tfsdk:"user_id"`
	Status   types.String `tfsdk:"status"`
	InviteID types.String `tfsdk:"invite_id"`
}

var _ resource.ResourceWithModifyPlan = &memberResource{}

func newMemberResource() resource.Resource { return &memberResource{} }

func (r *memberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_member"
}

func (r *memberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A member of the organization, invited by email.\n\n" +
			"Creating one mails a one-use invite; the member is `pending` until the person joins with it, then `joined` " +
			"with a `user_id`. Terraform notices on the next plan. An invite that expires or is revoked before anyone " +
			"joins, and a member who leaves or is removed, drop out of the state, so the next apply invites them again.\n\n" +
			"While pending, changing `email` or `role` revokes the invite and mails a new one. Once joined, `role` " +
			"changes the member's role and `email` the address PushWard shows for them, an unverified label.\n\n" +
			"An org admin key grants `member` and `viewer` only. Owners, admins and billing members are managed in the " +
			"PushWard console.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The user id once joined, the invite id while pending.",
				Computed:            true,
			},
			"email": schema.StringAttribute{
				MarkdownDescription: "Where the invite is mailed, and the member's label once they join. Lowercase, as PushWard stores it; `lower()` helps.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 254),
					stringvalidator.RegexMatches(noUpper, "must be lowercase: PushWard stores email addresses in lowercase (use lower())"),
				},
			},
			"role": schema.StringAttribute{
				MarkdownDescription: "`member` or `viewer`.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.OneOf("member", "viewer")},
			},
			"user_id": schema.StringAttribute{
				MarkdownDescription: "The member's user id; null until they join.",
				Computed:            true,
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "`pending` until the person joins, then `joined`.",
				Computed:            true,
			},
			"invite_id": schema.StringAttribute{
				MarkdownDescription: "The invite that was mailed; null after an import.",
				Computed:            true,
			},
		},
	}
}

func (r *memberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

// ImportState takes a user id.
func (r *memberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), req.ID)...)
}

// ModifyPlan keeps the computed attributes of a joined member, which an
// update changes in place. A pending one with a new email or role gets a
// new invite, and may have joined since the refresh, so they are unknown.
func (r *memberResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state memberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if state.Status.ValueString() == statusPending && (!plan.Email.Equal(state.Email) || !plan.Role.Equal(state.Role)) {
		return
	}
	plan.ID, plan.UserID, plan.Status, plan.InviteID = state.ID, state.UserID, state.Status, state.InviteID
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (m *memberModel) setEmail(email *string) {
	v := ""
	if email != nil {
		v = *email
	}
	m.Email = types.StringValue(v)
}

func (m *memberModel) setMember(mem *client.Member) {
	m.ID = types.StringValue(mem.UserID)
	m.UserID = types.StringValue(mem.UserID)
	m.Status = types.StringValue(statusJoined)
	m.Role = types.StringValue(mem.Role)
	m.setEmail(mem.Email)
}

// resolve brings m up to date: the invite while nobody has joined with it,
// then the member. gone means the invite ended unused, or the member left.
func (r *memberResource) resolve(ctx context.Context, m *memberModel) (gone bool, err error) {
	if m.UserID.ValueString() == "" {
		var inv client.Invite
		err := r.c.Do(ctx, http.MethodGet, r.c.OrgPath("invites", m.InviteID.ValueString()), nil, &inv)
		if client.IsGone(err, client.CodeInviteNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if inv.RedeemedBy == nil || *inv.RedeemedBy == "" {
			if inv.RevokedAt != nil || !time.Now().Before(inv.ExpiresAt) {
				return true, nil
			}
			m.ID = types.StringValue(inv.ID)
			m.InviteID = types.StringValue(inv.ID)
			m.UserID = types.StringNull()
			m.Status = types.StringValue(statusPending)
			m.Role = types.StringValue(inv.Role)
			if inv.Email != nil {
				m.setEmail(inv.Email)
			}
			return false, nil
		}
		m.UserID = types.StringValue(*inv.RedeemedBy)
	}
	var mem client.Member
	err = r.c.Do(ctx, http.MethodGet, r.c.OrgPath("members", m.UserID.ValueString()), nil, &mem)
	if client.IsGone(err, client.CodeMemberNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	m.setMember(&mem)
	return false, nil
}

// invite mails a new invite for plan and records it in plan.
func (r *memberResource) invite(ctx context.Context, plan *memberModel) error {
	var inv client.Invite
	body := map[string]any{"email": plan.Email.ValueString(), "role": plan.Role.ValueString()}
	if err := r.c.Do(ctx, http.MethodPost, r.c.OrgPath("invites"), body, &inv); err != nil {
		return err
	}
	plan.ID = types.StringValue(inv.ID)
	plan.InviteID = types.StringValue(inv.ID)
	plan.UserID = types.StringNull()
	plan.Status = types.StringValue(statusPending)
	return nil
}

func inviteFailed(diags *diag.Diagnostics, email string, err error) {
	if client.HasCode(err, client.CodeMemberEmailTaken) {
		diags.AddError("A member or open invite already has this email",
			"The organization already has a member or an open invite with "+email+". "+
				"Import the member with `terraform import <address> <user_id>` (the user id is in the console or the "+
				"pushward_org_members data source), or revoke the open invite in the PushWard console.\n\n"+err.Error())
		return
	}
	diags.AddError("Inviting the member failed", err.Error())
}

func (r *memberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan memberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.invite(ctx, &plan); err != nil {
		inviteFailed(&resp.Diagnostics, plan.Email.ValueString(), err)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *memberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state memberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	gone, err := r.resolve(ctx, &state)
	if err != nil {
		resp.Diagnostics.AddError("Reading the member failed", err.Error())
		return
	}
	if gone {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *memberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state memberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	wasJoined := state.Status.ValueString() == statusJoined
	cur := state
	gone, err := r.resolve(ctx, &cur)
	if err != nil {
		resp.Diagnostics.AddError("Reading the member failed", err.Error())
		return
	}
	switch {
	case gone && wasJoined:
		resp.Diagnostics.AddError("The member left the organization",
			"The member left or was removed since the plan. Run terraform apply again to invite them again.")
		return
	case gone:
		if err := r.invite(ctx, &plan); err != nil {
			inviteFailed(&resp.Diagnostics, plan.Email.ValueString(), err)
			return
		}
	case cur.Status.ValueString() == statusJoined:
		body := map[string]any{}
		if !plan.Role.Equal(cur.Role) {
			body["role"] = plan.Role.ValueString()
		}
		if !plan.Email.Equal(cur.Email) {
			body["email"] = plan.Email.ValueString()
		}
		if len(body) > 0 {
			var mem client.Member
			if err := r.c.Do(ctx, http.MethodPatch, r.c.OrgPath("members", cur.UserID.ValueString()), body, &mem); err != nil {
				resp.Diagnostics.AddError("Updating the member failed", err.Error())
				return
			}
			cur.setMember(&mem)
		}
		plan.ID, plan.UserID, plan.Status, plan.InviteID = cur.ID, cur.UserID, cur.Status, cur.InviteID
	default:
		err := r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("invites", cur.InviteID.ValueString()), nil, nil)
		if err != nil && !client.IsGone(err, client.CodeInviteNotFound) {
			resp.Diagnostics.AddError("Revoking the old invite failed", err.Error())
			return
		}
		if err := r.invite(ctx, &plan); err != nil {
			// The old invite is revoked: the next refresh drops the member
			// and the next apply invites them.
			inviteFailed(&resp.Diagnostics, plan.Email.ValueString(), err)
			return
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *memberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state memberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Re-read first: someone who joined since the refresh is removed, not
	// left behind with a revoked invite.
	gone, err := r.resolve(ctx, &state)
	if err != nil {
		resp.Diagnostics.AddError("Reading the member failed", err.Error())
		return
	}
	if gone {
		return
	}
	if state.Status.ValueString() == statusJoined {
		err = r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("members", state.UserID.ValueString()), nil, nil)
		if err != nil && !client.IsGone(err, client.CodeMemberNotFound) {
			resp.Diagnostics.AddError("Removing the member failed", err.Error())
		}
		return
	}
	err = r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("invites", state.InviteID.ValueString()), nil, nil)
	if err != nil && !client.IsGone(err, client.CodeInviteNotFound) {
		resp.Diagnostics.AddError("Revoking the invite failed", err.Error())
	}
}
