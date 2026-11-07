package provider

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const tagDevicesName = "pushward_org_tag_devices.pager"

// taggedIs checks the devices that carry the tag in *tagID on the server.
func (f *fakeAPI) taggedIs(tagID *string, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := f.tagged(*tagID); !slices.Equal(got, want) {
			return fmt.Errorf("devices with the tag on the server = %v, want %v", got, want)
		}
		return nil
	}
}

func TestAccTagDevices(t *testing.T) {
	f := newFake(t)
	ann := f.addMember("member", "ann@example.com")
	bob := f.addMember("member", "bob@example.com")
	f.addDevice(ann, "ann-phone")
	f.addDevice(ann, "ann-watch")
	f.addDevice(bob, "bob-phone")
	// The set is built from the devices data source, as the docs advise.
	cfg := func(deviceIDs ...string) string {
		return f.config(fmt.Sprintf(`
resource "pushward_org_tag" "pager" { name = "pager" }

data "pushward_org_devices" "all" {}

resource "pushward_org_tag_devices" "pager" {
  tag_id = pushward_org_tag.pager.id
  devices = [
    for d in data.pushward_org_devices.all.devices : { user_id = d.user_id, device_id = d.device_id }
    if contains(["%s"], d.device_id)
  ]
}
`, strings.Join(deviceIDs, `", "`)))
	}
	var tag string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy: func(s *terraform.State) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, d := range f.devices {
				if len(d.TagIDs) > 0 {
					return fmt.Errorf("device %s still has tags %v", d.DeviceID, d.TagIDs)
				}
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: cfg("ann-phone", "bob-phone"),
				Check: resource.ComposeAggregateTestCheckFunc(
					saveAttr("pushward_org_tag.pager", "id", &tag),
					resource.TestCheckResourceAttrPair(tagDevicesName, "id", "pushward_org_tag.pager", "id"),
					resource.TestCheckResourceAttr(tagDevicesName, "devices.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(tagDevicesName, "devices.*", map[string]string{"user_id": bob, "device_id": "bob-phone"}),
					f.taggedIs(&tag, "ann-phone", "bob-phone"),
				),
			},
			{
				Config: cfg("ann-phone"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(tagDevicesName, "devices.#", "1"),
					f.taggedIs(&tag, "ann-phone"),
				),
			},
			{ResourceName: tagDevicesName, ImportState: true, ImportStateVerify: true},
			{ResourceName: tagDevicesName, ImportState: true, ImportStateKind: resource.ImportBlockWithID},
			{
				// Tagged in the app: the next apply takes it off again.
				PreConfig: func() { f.tagDevice(tag, "ann-watch") },
				Config:    cfg("ann-phone"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(tagDevicesName, plancheck.ResourceActionUpdate)},
				},
				Check: f.taggedIs(&tag, "ann-phone"),
			},
			{
				// The tag was deleted: the resource drops out of the state and is
				// made again for the new tag. Kept in the state, it would be
				// replaced instead.
				PreConfig: func() { f.drop(tag) },
				Config:    cfg("ann-phone"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(tagDevicesName, plancheck.ResourceActionCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					attrNot(tagDevicesName, "id", &tag),
					saveAttr("pushward_org_tag.pager", "id", &tag),
					f.taggedIs(&tag, "ann-phone"),
				),
			},
			{
				// An unregistered device drops out of the data source and the state.
				PreConfig: func() { f.removeDevice("ann-phone") },
				Config:    cfg("ann-phone", "bob-phone"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(tagDevicesName, "devices.#", "1"),
					f.taggedIs(&tag, "bob-phone"),
				),
			},
		},
	})
	// One per apply that changed something, and one for the destroy.
	if n := f.count("POST", "/devices"); n != 6 {
		t.Errorf("%d tag devices requests, want 6", n)
	}
}

// A tag made in the console: the resource takes over its devices, and
// destroying it untags them but keeps the tag.
func TestAccTagDevicesConsoleTag(t *testing.T) {
	f := newFake(t)
	ann := f.addMember("member", "ann@example.com")
	f.addDevice(ann, "ann-phone")
	f.addDevice(ann, "ann-watch")
	tag := f.addTag("pager")
	f.tagDevice(tag, "ann-watch")
	cfg := func(deviceID string) string {
		return f.config(fmt.Sprintf(`
resource "pushward_org_tag_devices" "pager" {
  tag_id  = %q
  devices = [{ user_id = %q, device_id = %q }]
}
`, tag, ann, deviceID))
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy: func(s *terraform.State) error {
			if err := f.taggedIs(&tag)(s); err != nil {
				return err
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.tags[tag] == nil {
				return fmt.Errorf("destroy deleted the tag")
			}
			return nil
		},
		Steps: []resource.TestStep{
			{Config: cfg("ann-phone"), Check: f.taggedIs(&tag, "ann-phone")},
			{Config: cfg("no-such-device"), ExpectError: regexp.MustCompile(`org\.routing_invalid`)},
		},
	})
}

func TestAccTagDevicesUnknownTag(t *testing.T) {
	f := newFake(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: f.config(`
resource "pushward_org_tag_devices" "pager" {
  tag_id  = "00000000-0000-4000-8000-000000000999"
  devices = []
}
`),
			ExpectError: regexp.MustCompile(`(?s)not\s+a\s+tag\s+of\s+the\s+organization`),
		}},
	})
}

// Rows of one phone gain and lose the tag together on the server. A set that
// names only some of them fails and names the rest, and the phone keeps the
// tag; with every row listed the plan settles.
func TestAccTagDevicesPhoneRows(t *testing.T) {
	f := newFake(t)
	ann := f.addMember("member", "ann@example.com")
	f.addDevice(ann, "ann-phone")
	f.addTwin(ann, "ann-phone-2", "ann-phone")
	tag := f.addTag("pager")
	cfg := func(deviceIDs ...string) string {
		var refs []string
		for _, id := range deviceIDs {
			refs = append(refs, fmt.Sprintf("{ user_id = %q, device_id = %q }", ann, id))
		}
		return f.config(fmt.Sprintf("resource \"pushward_org_tag_devices\" \"pager\" {\n  tag_id  = %q\n  devices = [%s]\n}\n", tag, strings.Join(refs, ", ")))
	}
	missing := regexp.MustCompile(`(?s)carries\s+the\s+tag\s+but\s+is\s+not\s+in\s+devices:\s+user_id\s+\S+\s+device_id\s+ann-phone-2`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.taggedIs(&tag),
		Steps: []resource.TestStep{
			{Config: cfg("ann-phone"), ExpectError: missing},
			{Config: cfg("ann-phone", "ann-phone-2"), Check: f.taggedIs(&tag, "ann-phone", "ann-phone-2")},
			// Untagging one row would untag the phone: the apply fails and
			// puts the tag back, so the next plan for both rows is empty.
			{Config: cfg("ann-phone"), ExpectError: missing},
			{
				Config: cfg("ann-phone", "ann-phone-2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: f.taggedIs(&tag, "ann-phone", "ann-phone-2"),
			},
		},
	})
}

// Destroy untags only the devices in the state: one tagged in the app since
// the last refresh keeps the tag. The test framework destroys with
// -refresh=false, so the device tagged after the last step is not in the state.
func TestAccTagDevicesDestroyLeavesNewlyTagged(t *testing.T) {
	f := newFake(t)
	ann := f.addMember("member", "ann@example.com")
	f.addDevice(ann, "ann-phone")
	f.addDevice(ann, "ann-watch")
	tag := f.addTag("pager")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.taggedIs(&tag, "ann-watch"),
		Steps: []resource.TestStep{{
			Config:        f.config(fmt.Sprintf("resource \"pushward_org_tag_devices\" \"pager\" {\n  tag_id  = %q\n  devices = [{ user_id = %q, device_id = \"ann-phone\" }]\n}\n", tag, ann)),
			Check:         f.taggedIs(&tag, "ann-phone"),
			PostApplyFunc: func() { f.tagDevice(tag, "ann-watch") },
		}},
	})
}

// More devices carry the tag than one request may name: the change goes out
// in requests of at most 500 each.
func TestAccTagDevicesOverCap(t *testing.T) {
	f := newFake(t)
	ann := f.addMember("member", "ann@example.com")
	tag := f.addTag("pager")
	for i := range tagDeviceCap + 1 {
		id := fmt.Sprintf("dev-%03d", i)
		f.addDevice(ann, id)
		f.tagDevice(tag, id)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: f.config(fmt.Sprintf("resource \"pushward_org_tag_devices\" \"pager\" {\n  tag_id  = %q\n  devices = []\n}\n", tag)),
			Check:  f.taggedIs(&tag),
		}},
	})
	if n := f.count("POST", "/devices"); n != 2 {
		t.Errorf("%d tag devices requests, want 2", n)
	}
}
