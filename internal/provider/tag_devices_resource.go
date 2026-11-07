package provider

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

// tagDeviceCap is the most devices one tag devices request may add, and the
// most it may remove.
const tagDeviceCap = 500

type tagDevicesResource struct{ c *client.Client }

type tagDevicesModel struct {
	ID      types.String `tfsdk:"id"`
	TagID   types.String `tfsdk:"tag_id"`
	Devices types.Set    `tfsdk:"devices"`
	OrgID   types.String `tfsdk:"org_id"`
}

// deviceRef is one member's device, in the state and in the request body.
type deviceRef struct {
	UserID   string `tfsdk:"user_id" json:"user_id"`
	DeviceID string `tfsdk:"device_id" json:"device_id"`
}

var deviceRefType = types.ObjectType{AttrTypes: map[string]attr.Type{"user_id": types.StringType, "device_id": types.StringType}}

func newTagDevicesResource() resource.Resource { return &tagDevicesResource{} }

func (r *tagDevicesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_tag_devices"
}

func (r *tagDevicesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The devices that carry one tag. Declare at most one per tag; tags without one keep the devices " +
			"set in the PushWard console or app.\n\n" +
			"`devices` replaces the tag's devices, including ones tagged in the app or console: an apply takes the tag off " +
			"every device the set leaves out. A device that unregisters drops out of the set, so build the set from " +
			"`data.pushward_org_devices` rather than hard-coding device ids. Destroying it takes the tag off its devices.\n\n" +
			"PushWard tags a phone, not a row: a phone that registered again under a new device id has several rows " +
			"that share its push token, and they gain and lose a tag together. List all of a phone's rows or none; an " +
			"apply that lists only some fails, names the missing rows and leaves the phone tagged.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The tag id.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"tag_id": schema.StringAttribute{
				MarkdownDescription: "The tag. Changing it replaces the resource.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{lowerID},
			},
			"devices": schema.SetNestedAttribute{
				MarkdownDescription: "Every device that carries the tag, at most 500. Empty takes it off every device.",
				Required:            true,
				Validators:          []validator.Set{setvalidator.SizeAtMost(tagDeviceCap)},
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"user_id": schema.StringAttribute{
						MarkdownDescription: "The device owner's user id.",
						Required:            true,
						Validators:          []validator.String{lowerID},
					},
					"device_id": schema.StringAttribute{
						MarkdownDescription: "Device id.",
						Required:            true,
					},
				}},
			},
			"org_id": orgIDAttribute(),
		},
	}
}

func (r *tagDevicesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

// ImportState takes a tag id.
func (r *tagDevicesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("tag_id"), req.ID)...)
}

// current reads the devices that carry tagID now. gone means the tag is.
func (r *tagDevicesResource) current(ctx context.Context, tagID string) (refs []deviceRef, gone bool, err error) {
	err = r.c.Do(ctx, http.MethodGet, r.c.OrgPath("tags", tagID), nil, &client.Tag{})
	if client.IsGone(err, client.CodeTagNotFound) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	devices, err := client.List[client.Device](ctx, r.c, r.c.OrgPath("devices"))
	if err != nil {
		return nil, false, err
	}
	refs = []deviceRef{}
	for _, d := range devices {
		if slices.Contains(d.TagIDs, tagID) {
			refs = append(refs, deviceRef{UserID: d.UserID, DeviceID: d.DeviceID})
		}
	}
	return refs, false, nil
}

// minus is the refs in a that b lacks, sorted so requests are deterministic.
func minus(a, b []deviceRef) []deviceRef {
	out := slices.DeleteFunc(slices.Clone(a), func(d deviceRef) bool { return slices.Contains(b, d) })
	slices.SortFunc(out, func(x, y deviceRef) int {
		return cmp.Or(strings.Compare(x.UserID, y.UserID), strings.Compare(x.DeviceID, y.DeviceID))
	})
	return out
}

// post tags the add devices and untags the remove ones, in requests of at
// most tagDeviceCap each: remove can be longer when the tag was put on more
// devices outside Terraform. A failure part way is safe, since every apply
// works from a fresh read.
func (r *tagDevicesResource) post(ctx context.Context, tagID string, add, remove []deviceRef) error {
	for len(add) > 0 || len(remove) > 0 {
		a, rm := add[:min(tagDeviceCap, len(add))], remove[:min(tagDeviceCap, len(remove))]
		add, remove = add[len(a):], remove[len(rm):]
		body := map[string]any{"add": a, "remove": rm}
		if err := r.c.Do(ctx, http.MethodPost, r.c.OrgPath("tags", tagID, "devices"), body, nil); err != nil {
			return err
		}
	}
	return nil
}

// apply makes the planned devices the tag's devices.
func (r *tagDevicesResource) apply(ctx context.Context, plan *tagDevicesModel, diags *diag.Diagnostics) {
	want := []deviceRef{}
	diags.Append(plan.Devices.ElementsAs(ctx, &want, false)...)
	if diags.HasError() {
		return
	}
	tagID := plan.TagID.ValueString()
	cur, gone, err := r.current(ctx, tagID)
	if err != nil {
		diags.AddError("Reading the tag's devices failed", err.Error())
		return
	}
	if gone {
		diags.AddError("The tag does not exist",
			"Tag "+tagID+" is not a tag of the organization. Use the id of a pushward_org_tag, or one from the pushward_org_tags data source.")
		return
	}
	add, remove := minus(want, cur), minus(cur, want)
	if len(add)+len(remove) > 0 {
		if err := r.post(ctx, tagID, add, remove); err != nil {
			diags.AddError("Setting the tag's devices failed", err.Error())
			return
		}
		if !r.settled(ctx, tagID, want, diags) {
			return
		}
	}
	plan.ID = plan.TagID
}

// settled checks that exactly the want devices carry the tag. PushWard tags a
// phone, not a row: a member's rows that share a push token (a phone that
// registered again under a new device id) gain and lose a tag together, so a
// set naming only some of a phone's rows can never hold. When a remove took
// the tag off a wanted row that way, settled puts it back on that phone, then
// names the rows the set must add.
func (r *tagDevicesResource) settled(ctx context.Context, tagID string, want []deviceRef, diags *diag.Diagnostics) bool {
	after, _, err := r.current(ctx, tagID)
	if lost := minus(want, after); err == nil && len(lost) > 0 {
		if err = r.post(ctx, tagID, lost, nil); err == nil {
			after, _, err = r.current(ctx, tagID)
		}
	}
	if err != nil {
		diags.AddError("Setting the tag's devices failed", err.Error())
		return false
	}
	var rows []string
	for _, d := range minus(after, want) {
		rows = append(rows, "carries the tag but is not in devices: user_id "+d.UserID+", device_id "+d.DeviceID)
	}
	for _, d := range minus(want, after) {
		rows = append(rows, "is in devices but does not carry the tag: user_id "+d.UserID+", device_id "+d.DeviceID)
	}
	if len(rows) == 0 {
		return true
	}
	diags.AddError("Some rows of a phone are missing from devices",
		"PushWard tags a phone as a whole: the rows of a member that share a push token (a phone that registered again "+
			"under a new device id) gain and lose a tag together. After the change:\n\n  "+strings.Join(rows, "\n  ")+
			"\n\nList every row of such a phone in devices, or none of them. A set built from data.pushward_org_devices "+
			"by user_id lists every row.")
	return false
}

func (r *tagDevicesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan tagDevicesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.OrgID = types.StringValue(r.c.OrgID)
	r.apply(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *tagDevicesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state tagDevicesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	cur, gone, err := r.current(ctx, state.TagID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading the tag's devices failed", err.Error())
		return
	}
	if gone {
		resp.State.RemoveResource(ctx)
		return
	}
	devices, d := types.SetValueFrom(ctx, deviceRefType, cur)
	resp.Diagnostics.Append(d...)
	state.ID = state.TagID
	state.Devices = devices
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *tagDevicesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state tagDevicesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	plan.OrgID = state.OrgID
	r.apply(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

// Delete takes the tag off the devices in the state that still carry it, and
// leaves the ones tagged since alone.
func (r *tagDevicesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state tagDevicesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	mine := []deviceRef{}
	resp.Diagnostics.Append(state.Devices.ElementsAs(ctx, &mine, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tagID := state.TagID.ValueString()
	cur, gone, err := r.current(ctx, tagID)
	if err != nil {
		resp.Diagnostics.AddError("Reading the tag's devices failed", err.Error())
		return
	}
	if gone {
		return
	}
	// The state's devices that still carry the tag: cur minus what is not mine.
	if err := r.post(ctx, tagID, nil, minus(cur, minus(cur, mine))); err != nil {
		resp.Diagnostics.AddError("Taking the tag off its devices failed", err.Error())
	}
}
