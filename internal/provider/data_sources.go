package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

func timeString(t time.Time) types.String { return types.StringValue(t.Format(time.RFC3339)) }

func timePointer(t *time.Time) types.String {
	if t == nil {
		return types.StringNull()
	}
	return timeString(*t)
}

// --- pushward_org ---

type orgDataSource struct{ c *client.Client }

type orgModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Slug        types.String `tfsdk:"slug"`
	Role        types.String `tfsdk:"role"`
	Plan        types.String `tfsdk:"plan"`
	Status      types.String `tfsdk:"status"`
	MemberCount types.Int64  `tfsdk:"member_count"`
	MemberLimit types.Int64  `tfsdk:"member_limit"`
	Seats       types.Int64  `tfsdk:"seats"`
	TrialEndsAt types.String `tfsdk:"trial_ends_at"`
	DeleteAfter types.String `tfsdk:"delete_after"`
	CreatedAt   types.String `tfsdk:"created_at"`
}

func newOrgDataSource() datasource.DataSource { return &orgDataSource{} }

func (d *orgDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org"
}

func computed(doc string) schema.StringAttribute {
	return schema.StringAttribute{MarkdownDescription: doc, Computed: true}
}

func (d *orgDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The organization the provider's key belongs to.",
		Attributes: map[string]schema.Attribute{
			"id":            computed("Organization id."),
			"name":          computed("Display name."),
			"slug":          computed("URL slug."),
			"role":          computed("The role the key acts as: `admin` for a manage key, `viewer` for a read key."),
			"plan":          computed("`comp`, `trial`, `team` or `enterprise`."),
			"status":        computed("`active` or `restricted`."),
			"member_count":  schema.Int64Attribute{MarkdownDescription: "Members now.", Computed: true},
			"member_limit":  schema.Int64Attribute{MarkdownDescription: "Most members the organization may have.", Computed: true},
			"seats":         schema.Int64Attribute{MarkdownDescription: "Seats a team or enterprise organization pays for.", Computed: true},
			"trial_ends_at": computed("When the trial ends; only while the plan is `trial`."),
			"delete_after":  computed("When a restricted organization is deleted, if automatic deletion is on."),
			"created_at":    computed("When the organization was created."),
		},
	}
}

func (d *orgDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

func (d *orgDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	var o client.Org
	if err := d.c.Do(ctx, http.MethodGet, d.c.OrgPath(), nil, &o); err != nil {
		resp.Diagnostics.AddError("Reading the organization failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &orgModel{
		ID: types.StringValue(o.ID), Name: types.StringValue(o.Name), Slug: types.StringValue(o.Slug),
		Role: types.StringValue(o.Role), Plan: types.StringValue(o.Plan), Status: types.StringValue(o.Status),
		MemberCount: types.Int64Value(o.MemberCount), MemberLimit: types.Int64Value(o.MemberLimit),
		Seats: types.Int64PointerValue(o.Seats), TrialEndsAt: timePointer(o.TrialEndsAt),
		DeleteAfter: timePointer(o.DeleteAfter), CreatedAt: timeString(o.CreatedAt),
	})...)
}

// --- pushward_org_members ---

type membersDataSource struct{ c *client.Client }

type memberItem struct {
	UserID   types.String `tfsdk:"user_id"`
	Nickname types.String `tfsdk:"nickname"`
	Role     types.String `tfsdk:"role"`
	Email    types.String `tfsdk:"email"`
	JoinedAt types.String `tfsdk:"joined_at"`
}

func newMembersDataSource() datasource.DataSource { return &membersDataSource{} }

func (d *membersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_members"
}

func (d *membersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every member of the organization, by role (owners first), then by when they joined. People with a pending invite are not members yet.",
		Attributes: map[string]schema.Attribute{
			"members": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"user_id":   computed("User id."),
					"nickname":  computed("The member's name, if they set one."),
					"role":      computed("`owner`, `admin`, `billing`, `member` or `viewer`."),
					"email":     computed("The member's email label; null when there is none."),
					"joined_at": computed("When they joined."),
				}},
			},
		},
	}
}

func (d *membersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

func (d *membersDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	members, err := client.List[client.Member](ctx, d.c, d.c.OrgPath("members"))
	if err != nil {
		resp.Diagnostics.AddError("Listing the members failed", err.Error())
		return
	}
	items := make([]memberItem, 0, len(members))
	for _, m := range members {
		items = append(items, memberItem{
			UserID: types.StringValue(m.UserID), Nickname: types.StringPointerValue(m.Nickname),
			Role: types.StringValue(m.Role), Email: types.StringPointerValue(m.Email), JoinedAt: timeString(m.JoinedAt),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &struct {
		Members []memberItem `tfsdk:"members"`
	}{items})...)
}

// --- pushward_org_devices ---

type devicesDataSource struct{ c *client.Client }

type deviceItem struct {
	UserID                  types.String `tfsdk:"user_id"`
	Nickname                types.String `tfsdk:"nickname"`
	DeviceID                types.String `tfsdk:"device_id"`
	Name                    types.String `tfsdk:"name"`
	Platform                types.String `tfsdk:"platform"`
	DeviceModel             types.String `tfsdk:"device_model"`
	OSVersion               types.String `tfsdk:"os_version"`
	AppVersion              types.String `tfsdk:"app_version"`
	CanReceiveNotifications types.Bool   `tfsdk:"can_receive_notifications"`
	CanStartLiveActivities  types.Bool   `tfsdk:"can_start_live_activities"`
	Delivery                types.Bool   `tfsdk:"delivery"`
	TagIDs                  types.Set    `tfsdk:"tag_ids"`
	CreatedAt               types.String `tfsdk:"created_at"`
}

func newDevicesDataSource() datasource.DataSource { return &devicesDataSource{} }

func (d *devicesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_devices"
}

func (d *devicesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	flag := func(doc string) schema.BoolAttribute {
		return schema.BoolAttribute{MarkdownDescription: doc, Computed: true}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every member's devices, with the organization's tags on each.",
		Attributes: map[string]schema.Attribute{
			"devices": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"user_id":                   computed("The owner's user id."),
					"nickname":                  computed("The owner's name, if they set one."),
					"device_id":                 computed("Device id."),
					"name":                      computed("The name the owner gave the device."),
					"platform":                  computed("`ios`, `ipados`, `watchos`, `macos` or `android`."),
					"device_model":              computed("Hardware model."),
					"os_version":                computed("Operating system version."),
					"app_version":               computed("PushWard app version."),
					"can_receive_notifications": flag("Whether the device can receive notifications."),
					"can_start_live_activities": flag("Whether a Live Activity can be started on the device."),
					"delivery":                  flag("Whether the organization reaches the device; its owner can switch that off."),
					"tag_ids": schema.SetAttribute{
						MarkdownDescription: "The organization's tags on the device.",
						ElementType:         types.StringType,
						Computed:            true,
					},
					"created_at": computed("When the device registered."),
				}},
			},
		},
	}
}

func (d *devicesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

func (d *devicesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	var out struct {
		Items []client.Device `json:"items"`
	}
	if err := d.c.Do(ctx, http.MethodGet, d.c.OrgPath("devices"), nil, &out); err != nil {
		resp.Diagnostics.AddError("Listing the devices failed", err.Error())
		return
	}
	items := make([]deviceItem, 0, len(out.Items))
	for _, dv := range out.Items {
		items = append(items, deviceItem{
			UserID: types.StringValue(dv.UserID), Nickname: types.StringPointerValue(dv.Nickname),
			DeviceID: types.StringValue(dv.DeviceID), Name: types.StringPointerValue(dv.Name),
			Platform: types.StringPointerValue(dv.Platform), DeviceModel: types.StringPointerValue(dv.DeviceModel),
			OSVersion: types.StringPointerValue(dv.OSVersion), AppVersion: types.StringPointerValue(dv.AppVersion),
			CanReceiveNotifications: types.BoolValue(dv.CanReceiveNotifications),
			CanStartLiveActivities:  types.BoolValue(dv.CanStartLiveActivities),
			Delivery:                types.BoolValue(dv.Delivery),
			TagIDs:                  stringSet(dv.TagIDs),
			CreatedAt:               timeString(dv.CreatedAt),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &struct {
		Devices []deviceItem `tfsdk:"devices"`
	}{items})...)
}
