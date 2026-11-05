package provider

import (
	"context"
	"net/http"
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
	Secret        types.String    `tfsdk:"secret"`
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
			Validators:          []validator.Set{setvalidator.SizeAtMost(50)},
		}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A sending key (`hlk_...`) owned by the organization. What it sends reaches the members the " +
			"organization's routing rules allow, and its emails the organization's verified recipients.\n\n" +
			"`secret` is returned once, when the key is created, and kept in the state; it is null after an import. " +
			"`target` is fixed at creation, so changing it replaces the key. To rotate the secret, replace the key, " +
			"for example with `lifecycle { replace_triggered_by = [...] }`.",
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
					"emails":  level("emails", "`none` or `send`. Not for a key with a `target`.", "none", "send"),
				},
			},
			"activity_slugs": slugs("Restrict the key to these activity slugs or patterns (trailing `*`)."),
			"widget_slugs":   slugs("Restrict the key to these widget slugs or patterns (trailing `*`)."),
			"expires_at": schema.StringAttribute{
				MarkdownDescription: "When the key stops working, an RFC 3339 time in the future. Unset means never.",
				Optional:            true,
			},
			"target": schema.SingleNestedAttribute{
				MarkdownDescription: "Limit the key to these groups and tags: what it sends must target inside them, and it sees " +
					"only activities and widgets targeted inside them. Unset reaches the whole organization. " +
					"Changing it replaces the key.",
				Optional:      true,
				PlanModifiers: []planmodifier.Object{objectplanmodifier.RequiresReplace()},
				Attributes: map[string]schema.Attribute{
					"group_ids": ids("Group ids, at most 50."),
					"tag_ids":   ids("Tag ids, at most 50."),
				},
			},
			"secret": schema.StringAttribute{
				MarkdownDescription: "The key's secret (`hlk_...`). Only known when Terraform created the key.",
				Computed:            true,
				Sensitive:           true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
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
	if resp.Diagnostics.HasError() {
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
	if resp.Diagnostics.HasError() {
		return
	}
	body := plan.body(ctx, &resp.Diagnostics)
	// Sent only when it changed: an expiry already in the past is refused.
	if !plan.ExpiresAt.Equal(state.ExpiresAt) {
		body["expires_at"] = expiry(plan.ExpiresAt, &resp.Diagnostics)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	var k client.Key
	if err := r.c.Do(ctx, http.MethodPatch, r.c.OrgPath("keys", state.ID.ValueString()), body, &k); err != nil {
		resp.Diagnostics.AddError("Updating the key failed", err.Error())
		return
	}
	plan.set(&k)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state keyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.c.Do(ctx, http.MethodDelete, r.c.OrgPath("keys", state.ID.ValueString()), nil, nil)
	if err != nil && !client.IsGone(err, client.CodeKeyNotFound) {
		resp.Diagnostics.AddError("Revoking the key failed", err.Error())
	}
}
