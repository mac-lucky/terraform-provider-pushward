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

// --- pushward_org_groups ---

type groupsDataSource struct{ c *client.Client }

type groupItem struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	MemberIDs   types.Set    `tfsdk:"member_ids"`
}

func newGroupsDataSource() datasource.DataSource { return &groupsDataSource{} }

func (d *groupsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_groups"
}

func (d *groupsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every group of the organization. Use it to reference groups made in the console by name, " +
			"for example `{ for g in data.pushward_org_groups.all.groups : g.name => g.id }`, or to find the id that " +
			"`terraform import` takes.",
		Attributes: map[string]schema.Attribute{
			"groups": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":          computed("Group id."),
					"name":        computed("Name, unique in the organization."),
					"description": computed("Description; null when there is none."),
					"member_ids": schema.SetAttribute{
						MarkdownDescription: "User ids of the group's members.",
						ElementType:         types.StringType,
						Computed:            true,
					},
				}},
			},
		},
	}
}

func (d *groupsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

func (d *groupsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	groups, err := client.List[client.Group](ctx, d.c, d.c.OrgPath("groups"))
	if err != nil {
		resp.Diagnostics.AddError("Listing the groups failed", err.Error())
		return
	}
	items := make([]groupItem, 0, len(groups))
	for _, g := range groups {
		items = append(items, groupItem{
			ID: types.StringValue(g.ID), Name: types.StringValue(g.Name),
			Description: types.StringPointerValue(g.Description), MemberIDs: stringSet(g.MemberIDs),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &struct {
		Groups []groupItem `tfsdk:"groups"`
	}{items})...)
}

// --- pushward_org_tags ---

type tagsDataSource struct{ c *client.Client }

type tagItem struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	DeviceCount types.Int64  `tfsdk:"device_count"`
}

func newTagsDataSource() datasource.DataSource { return &tagsDataSource{} }

func (d *tagsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_tags"
}

func (d *tagsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every device tag of the organization. Use it to reference tags made in the console by name, " +
			"for example `{ for t in data.pushward_org_tags.all.tags : t.name => t.id }`, or to find the id that " +
			"`terraform import` takes.",
		Attributes: map[string]schema.Attribute{
			"tags": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":           computed("Tag id."),
					"name":         computed("Name, unique in the organization."),
					"description":  computed("Description; null when there is none."),
					"device_count": schema.Int64Attribute{MarkdownDescription: "How many member devices carry the tag.", Computed: true},
				}},
			},
		},
	}
}

func (d *tagsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

func (d *tagsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	tags, err := client.List[client.Tag](ctx, d.c, d.c.OrgPath("tags"))
	if err != nil {
		resp.Diagnostics.AddError("Listing the tags failed", err.Error())
		return
	}
	items := make([]tagItem, 0, len(tags))
	for _, t := range tags {
		items = append(items, tagItem{
			ID: types.StringValue(t.ID), Name: types.StringValue(t.Name),
			Description: types.StringPointerValue(t.Description), DeviceCount: types.Int64Value(t.DeviceCount),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &struct {
		Tags []tagItem `tfsdk:"tags"`
	}{items})...)
}
