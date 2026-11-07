package provider

import (
	"context"
	"net/http"
	"reflect"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

type keyResource struct{ c *client.Client }

type keyModel struct {
	ID            types.String    `tfsdk:"id"`
	Name          types.String    `tfsdk:"name"`
	Permissions   *keyPermissions `tfsdk:"permissions"`
	ActivitySlugs types.Set       `tfsdk:"activity_slugs"`
	WidgetSlugs   types.Set       `tfsdk:"widget_slugs"`
	ExpiresAt     types.String    `tfsdk:"expires_at"`
	Target        *keyTarget      `tfsdk:"target"`
	Rotation      types.Map       `tfsdk:"rotation_triggers"`
	Secret        types.String    `tfsdk:"secret"`
	OrgID         types.String    `tfsdk:"org_id"`
}

type keyPermissions struct {
	Activities    types.String `tfsdk:"activities"`
	Notifications types.String `tfsdk:"notifications"`
	Widgets       types.String `tfsdk:"widgets"`
	Emails        types.String `tfsdk:"emails"`
}

type keyTarget struct {
	GroupIDs types.Set `tfsdk:"group_ids"`
	TagIDs   types.Set `tfsdk:"tag_ids"`
}

// The levels a new key gets when permissions is left out: what the server
// gives a key created without permissions.
var defaultKeyLevels = map[string]string{
	"activities":    "manage",
	"notifications": "schedule",
	"widgets":       "none",
	"emails":        "none",
}

var (
	_ resource.ResourceWithValidateConfig = &keyResource{}
	_ resource.ResourceWithModifyPlan     = &keyResource{}
)

func newKeyResource() resource.Resource { return &keyResource{} }

func (r *keyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_key"
}

func level(name, doc string, levels ...string) schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: doc + " Defaults to `" + defaultKeyLevels[name] + "`.",
		Optional:            true,
		Computed:            true,
		Default:             stringdefault.StaticString(defaultKeyLevels[name]),
		Validators:          []validator.String{stringvalidator.OneOf(levels...)},
	}
}

func (r *keyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	levelTypes := map[string]attr.Type{}
	levelValues := map[string]attr.Value{}
	for k, v := range defaultKeyLevels {
		levelTypes[k] = types.StringType
		levelValues[k] = types.StringValue(v)
	}
	slugs := func(doc string) schema.SetAttribute {
		return schema.SetAttribute{
			MarkdownDescription: doc + " Empty means unrestricted.",
			ElementType:         types.StringType,
			Optional:            true,
			Computed:            true,
			Default:             setdefault.StaticValue(emptySet()),
		}
	}
	ids := func(doc string) schema.SetAttribute {
		return schema.SetAttribute{
			MarkdownDescription: doc,
			ElementType:         types.StringType,
			Optional:            true,
			Computed:            true,
			Default:             setdefault.StaticValue(emptySet()),
			Validators:          []validator.Set{setvalidator.SizeAtMost(50), setvalidator.ValueStringsAre(lowerID)},
		}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A sending key (`hlk_...`) owned by the organization. What it sends reaches the members the " +
			"organization's routing rules allow, and its emails the organization's verified recipients.\n\n" +
			"`secret` is returned once, when the key is created, and kept in the state; it is null after an import. " +
			"`target` is fixed at creation, so changing it replaces the key.\n\n" +
			"To rotate the secret in place, change `rotation_triggers`: the key keeps its id and gets a new secret, and " +
			"the old secret stops working at once. After an import, setting `rotation_triggers` rolls the secret and puts " +
			"the new one in the state. Replacing the key, for example with `lifecycle { replace_triggered_by = [...] }`, " +
			"also gives a new secret, on a new key id.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:   true,
				Validators: []validator.String{stringvalidator.UTF8LengthBetween(1, 256)},
			},
			"permissions": schema.SingleNestedAttribute{
				MarkdownDescription: "What the key may do. Organization keys have no device, sharing or account access.",
				Optional:            true,
				Computed:            true,
				Default:             objectdefault.StaticValue(types.ObjectValueMust(levelTypes, levelValues)),
				Attributes: map[string]schema.Attribute{
					"activities": level("activities",
						"`none`, `read` (list and get), `update` (also PATCH) or `manage` (also create and delete).",
						"none", "read", "update", "manage"),
					"notifications": level("notifications",
						"`none`, `send` (send and read answers) or `schedule` (also scheduled notifications).",
						"none", "send", "schedule"),
					"widgets": level("widgets", "`none`, `read` or `write`.", "none", "read", "write"),
					"emails":  level("emails", "`none` or `send`. Not for a key with a `target`, which PushWard refuses.", "none", "send"),
				},
			},
			"activity_slugs": slugs("Restrict the key to these activity slugs or patterns (trailing `*`)."),
			"widget_slugs":   slugs("Restrict the key to these widget slugs or patterns (trailing `*`)."),
			"expires_at": schema.StringAttribute{
				MarkdownDescription: "When the key stops working, an RFC 3339 time in the future. Unset means never. " +
					"An import reads it as PushWard returns it, so when the configuration spells the same time another way " +
					"(another UTC offset), the first apply after an import rewrites it in place and changes nothing else.",
				Optional:   true,
				Validators: []validator.String{rfc3339},
			},
			"target": schema.SingleNestedAttribute{
				MarkdownDescription: "Limit the key to these groups and tags: what it sends must target inside them, and it sees " +
					"only activities and widgets targeted inside them. Unset reaches the whole organization. " +
					"It must name at least one group or tag. Changing it replaces the key.",
				Optional:      true,
				PlanModifiers: []planmodifier.Object{objectplanmodifier.RequiresReplace()},
				Attributes: map[string]schema.Attribute{
					"group_ids": ids("Group ids, at most 50."),
					"tag_ids":   ids("Tag ids, at most 50."),
				},
			},
			"rotation_triggers": schema.MapAttribute{
				MarkdownDescription: "Arbitrary values; a change rolls the secret in place (same key id, a new secret, the old one " +
					"stops working at once). Creating the key and removing the map do not roll it.",
				ElementType: types.StringType,
				Optional:    true,
			},
			"secret": schema.StringAttribute{
				MarkdownDescription: "The key's secret (`hlk_...`). Only known when Terraform created or rolled the key.",
				Computed:            true,
				Sensitive:           true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"org_id": orgIDAttribute(),
		},
	}
}

// ValidateConfig refuses at plan time the targets PushWard refuses: one that
// names nothing, and one on a key that sends emails.
func (r *keyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var target types.Object
	var emails types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("target"), &target)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("permissions").AtName("emails"), &emails)...)
	if resp.Diagnostics.HasError() || target.IsNull() || target.IsUnknown() {
		return
	}
	if emails.ValueString() == "send" {
		resp.Diagnostics.AddAttributeError(path.Root("permissions").AtName("emails"), "Emails need a key without a target",
			"A key limited to groups and tags cannot send emails: the organization's email recipients are not in its groups and tags. "+
				"Set emails = \"none\", or leave target out.")
	}
	empty := func(name string) bool {
		s, ok := target.Attributes()[name].(types.Set)
		return !ok || s.IsNull() || (!s.IsUnknown() && len(s.Elements()) == 0)
	}
	if empty("group_ids") && empty("tag_ids") {
		resp.Diagnostics.AddAttributeError(path.Root("target"), "Empty target",
			"target must name at least one group or tag. Leave target out to let the key reach the whole organization.")
	}
}

// rolls reports whether an update rolls the secret: rotation_triggers is set
// and changed. Removing it does not roll.
func rolls(plan, state types.Map) bool { return !plan.IsNull() && !plan.Equal(state) }

// ModifyPlan shows the secret as unknown when the update rolls it.
func (r *keyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state types.Map
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("rotation_triggers"), &plan)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("rotation_triggers"), &state)...)
	if !resp.Diagnostics.HasError() && rolls(plan, state) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("secret"), types.StringUnknown())...)
	}
}

func (r *keyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

func (r *keyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// set copies k into m. An expiry that is the same instant as the one in m
// keeps m's spelling of it.
func (m *keyModel) set(k *client.Key) {
	m.ID = types.StringValue(k.ID)
	m.Name = types.StringValue(k.Name)
	m.Permissions = &keyPermissions{
		Activities:    types.StringValue(k.Permissions.Activities),
		Notifications: types.StringValue(k.Permissions.Notifications),
		Widgets:       types.StringValue(k.Permissions.Widgets),
		Emails:        types.StringValue(k.Permissions.Emails),
	}
	m.ActivitySlugs = stringSet(k.ActivitySlugs)
	m.WidgetSlugs = stringSet(k.WidgetSlugs)
	if k.ExpiresAt == nil {
		m.ExpiresAt = types.StringNull()
	} else if t, err := time.Parse(time.RFC3339, m.ExpiresAt.ValueString()); err != nil || !t.Equal(*k.ExpiresAt) {
		m.ExpiresAt = types.StringValue(k.ExpiresAt.Format(time.RFC3339))
	}
	m.Target = nil
	if k.Target != nil {
		m.Target = &keyTarget{GroupIDs: stringSet(k.Target.GroupIDs), TagIDs: stringSet(k.Target.TagIDs)}
	}
	if k.Secret != nil {
		m.Secret = types.StringValue(*k.Secret)
	}
}

// body is the create or PATCH body for m: name, levels and slug lists.
func (m *keyModel) body(ctx context.Context, diags *diag.Diagnostics) map[string]any {
	body := map[string]any{
		"name":           m.Name.ValueString(),
		"activity_slugs": setStrings(ctx, m.ActivitySlugs, diags),
		"widget_slugs":   setStrings(ctx, m.WidgetSlugs, diags),
	}
	if p := m.Permissions; p != nil {
		body["permissions"] = map[string]string{
			"activities":    p.Activities.ValueString(),
			"notifications": p.Notifications.ValueString(),
			"widgets":       p.Widgets.ValueString(),
			"emails":        p.Emails.ValueString(),
		}
	}
	return body
}

func expiry(s types.String, diags *diag.Diagnostics) any {
	if s.IsNull() {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("expires_at"), "Invalid expires_at", "expires_at must be an RFC 3339 time, such as 2027-01-31T00:00:00Z.")
		return nil
	}
	return t
}

func (r *keyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan keyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.OrgID = types.StringValue(r.c.OrgID)
	body := plan.body(ctx, &resp.Diagnostics)
	if exp := expiry(plan.ExpiresAt, &resp.Diagnostics); exp != nil {
		body["expires_at"] = exp
	}
	if plan.Target != nil {
		body["target"] = map[string]any{
			"group_ids": setStrings(ctx, plan.Target.GroupIDs, &resp.Diagnostics),
			"tag_ids":   setStrings(ctx, plan.Target.TagIDs, &resp.Diagnostics),
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}
	var k client.Key
	if err := r.c.Do(ctx, http.MethodPost, r.c.OrgPath("keys"), body, &k); err != nil {
		resp.Diagnostics.AddError("Creating the key failed", err.Error())
		return
	}
	plan.set(&k)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state keyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	var k client.Key
	err := r.c.Do(ctx, http.MethodGet, r.c.OrgPath("keys", state.ID.ValueString()), nil, &k)
	if client.IsGone(err, client.CodeKeyNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the key failed", err.Error())
		return
	}
	state.set(&k)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *keyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state keyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	plan.OrgID = state.OrgID
	body := plan.body(ctx, &resp.Diagnostics)
	// Nothing but rotation_triggers changed when the body is the state's.
	patch := !reflect.DeepEqual(body, state.body(ctx, &resp.Diagnostics)) || !plan.ExpiresAt.Equal(state.ExpiresAt)
	// Sent only when it changed: an expiry already in the past is refused.
	if !plan.ExpiresAt.Equal(state.ExpiresAt) {
		body["expires_at"] = expiry(plan.ExpiresAt, &resp.Diagnostics)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	var k client.Key
	if patch {
		if err := r.c.Do(ctx, http.MethodPatch, r.c.OrgPath("keys", id), body, &k); err != nil {
			resp.Diagnostics.AddError("Updating the key failed", err.Error())
			return
		}
		plan.set(&k)
	}
	// Last, so that nothing can fail between the roll and saving its secret.
	// A failed roll keeps the old triggers in the state, so the next apply
	// rolls again.
	if rolls(plan.Rotation, state.Rotation) {
		if err := r.c.Do(ctx, http.MethodPost, r.c.OrgPath("keys", id, "roll"), nil, &k); err != nil {
			resp.Diagnostics.AddError("Rolling the key's secret failed", err.Error())
			return
		}
		plan.set(&k)
	}
	// Planned unknown for a trigger only known at apply that ended up unchanged.
	if plan.Secret.IsUnknown() {
		plan.Secret = state.Secret
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state keyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	err := r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("keys", state.ID.ValueString()), nil, nil)
	if err != nil && !client.IsGone(err, client.CodeKeyNotFound) {
		resp.Diagnostics.AddError("Revoking the key failed", err.Error())
	}
}
