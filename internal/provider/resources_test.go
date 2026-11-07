package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

// empty fails unless the fake holds nothing of what Terraform made.
func (f *fakeAPI) empty(*terraform.State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.groups) + len(f.tags) + len(f.routes) + len(f.keys); n != 0 {
		return fmt.Errorf("%d groups, tags, routes or keys left after destroy", n)
	}
	return nil
}

func TestAccGroup(t *testing.T) {
	f := newFake(t)
	u1 := f.addMember("member", "a@example.com")
	u2 := f.addMember("member", "b@example.com")
	const n = "pushward_org_group.ops"
	cfg := func(name, extra string) string {
		return f.config(fmt.Sprintf("resource \"pushward_org_group\" \"ops\" {\n  name = %q\n%s}\n", name, extra))
	}
	first := cfg("ops", fmt.Sprintf("  description = \"On call\"\n  member_ids = [%q]\n", u1))
	second := cfg("ops-team", fmt.Sprintf("  member_ids = [%q, %q]\n", u1, u2))
	var id string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config: first,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "name", "ops"),
					resource.TestCheckResourceAttr(n, "description", "On call"),
					resource.TestCheckTypeSetElemAttr(n, "member_ids.*", u1),
					saveAttr(n, "id", &id),
				),
			},
			{
				Config: second,
				Check: resource.ComposeAggregateTestCheckFunc(
					attrIs(n, "id", &id),
					resource.TestCheckResourceAttr(n, "name", "ops-team"),
					resource.TestCheckNoResourceAttr(n, "description"),
					resource.TestCheckResourceAttr(n, "member_ids.#", "2"),
				),
			},
			{ResourceName: n, ImportState: true, ImportStateVerify: true},
			{ResourceName: n, ImportState: true, ImportStateKind: resource.ImportBlockWithID},
			{
				PreConfig: func() { f.drop(id) },
				Config:    second,
				Check:     resource.ComposeAggregateTestCheckFunc(attrNot(n, "id", &id), resource.TestCheckResourceAttr(n, "member_ids.#", "2")),
			},
			{
				Config: cfg("ops-team", ""),
				Check:  resource.TestCheckResourceAttr(n, "member_ids.#", "0"),
			},
		},
	})
}

func TestAccGroupNameValidated(t *testing.T) {
	f := newFake(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      f.config(`resource "pushward_org_group" "x" { name = "Ops Team" }`),
			ExpectError: regexp.MustCompile(`lowercase letters`),
		}},
	})
}

// What the server would refuse, trim or reformat is refused at plan time,
// before it could fail an apply or leave a value in state that never matches
// the configuration.
func TestAccPlanTimeValidation(t *testing.T) {
	f := newFake(t)
	const upper = `["0B000000-0000-4000-8000-000000000001"]`
	for _, tc := range []struct{ typ, attrs, want string }{
		{"pushward_org_group", "name = \"ops\"\ndescription = <<-EOT\n  On call\nEOT", `whitespace`},
		{"pushward_org_tag", `name = "pager"` + "\n" + `description = " Phones"`, `whitespace`},
		{"pushward_org_tag", `name = "pager"` + "\n" + `description = "   "`, `whitespace`},
		{"pushward_org_route", `slug_pattern = "*"` + "\n" + `applies_to = "both"` + "\n" + `note = "All\n"`, `whitespace`},
		{"pushward_org_key", `name = "ci"` + "\n" + `expires_at = "2027-06-30"`, `RFC 3339`},
		{"pushward_org_key", `name = "ci"` + "\n" + `target = {}`, `at least one group or tag`},
		{"pushward_org_key", `name = "ci"` + "\n" + `target = { group_ids = [] }`, `at least one group or tag`},
		{"pushward_org_key", `name = "ci"` + "\n" + `permissions = { emails = "send" }` + "\n" +
			`target = { tag_ids = ["0b000000-0000-4000-8000-000000000001"] }`, `cannot send emails`},
		{"pushward_org_key", `name = "ci"` + "\n" + `target = { group_ids = ` + upper + ` }`, `must be lowercase`},
		{"pushward_org_group", `name = "ops"` + "\n" + `member_ids = ` + upper, `must be lowercase`},
		{"pushward_org_route", `slug_pattern = "*"` + "\n" + `applies_to = "both"` + "\n" + `allow_group_ids = ` + upper, `must be lowercase`},
		{"pushward_org_route", `slug_pattern = "*"` + "\n" + `applies_to = "both"` + "\n" + `allow_tag_ids = ` + upper, `must be lowercase`},
		{"pushward_org_route_order", `route_ids = ` + upper, `must be lowercase`},
		{"pushward_org_tag_devices", `tag_id = "0B000000-0000-4000-8000-000000000001"` + "\n" + `devices = []`, `must be lowercase`},
		{"pushward_org_tag_devices", `tag_id = "t"` + "\n" + `devices = [{ user_id = "0B000000-0000-4000-8000-000000000001", device_id = "d" }]`, `must be lowercase`},
		{"pushward_org_settings", `name = " Acme"`, `whitespace`},
		{"pushward_org_settings", `name = "` + strings.Repeat("a", 51) + `"`, `between 1 and 50`},
		{"pushward_org_settings", `name = ""`, `between 1 and 50`},
	} {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: f.config(fmt.Sprintf("resource %q \"x\" {\n%s\n}\n", tc.typ, tc.attrs)),
				// Terraform wraps the message wherever a space is.
				ExpectError: regexp.MustCompile(strings.ReplaceAll(tc.want, " ", `\s+`)),
			}},
		})
	}
	if n := f.count("POST", "") + f.count("PUT", ""); n != 0 {
		t.Errorf("refused configurations sent %d writes", n)
	}
}

func TestAccTag(t *testing.T) {
	f := newFake(t)
	const n = "pushward_org_tag.pager"
	cfg := func(name, desc string) string {
		d := ""
		if desc != "" {
			d = fmt.Sprintf("  description = %q\n", desc)
		}
		return f.config(fmt.Sprintf("resource \"pushward_org_tag\" \"pager\" {\n  name = %q\n%s}\n", name, d))
	}
	var id string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config: cfg("pager", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "name", "pager"),
					resource.TestCheckNoResourceAttr(n, "description"),
					saveAttr(n, "id", &id),
				),
			},
			{
				Config: cfg("pager", "Phones on the rota"),
				Check: resource.ComposeAggregateTestCheckFunc(
					attrIs(n, "id", &id),
					resource.TestCheckResourceAttr(n, "description", "Phones on the rota"),
				),
			},
			{ResourceName: n, ImportState: true, ImportStateVerify: true},
			{ResourceName: n, ImportState: true, ImportStateKind: resource.ImportBlockWithID},
			{
				PreConfig: func() { f.drop(id) },
				Config:    cfg("pager", "Phones on the rota"),
				Check:     attrNot(n, "id", &id),
			},
		},
	})
}

func TestAccRoute(t *testing.T) {
	f := newFake(t)
	const n = "pushward_org_route.deploys"
	base := `
resource "pushward_org_group" "ops" { name = "ops" }
resource "pushward_org_tag" "pager" { name = "pager" }
`
	first := f.config(base + `
resource "pushward_org_route" "deploys" {
  slug_pattern    = "deploy-*"
  applies_to      = "both"
  allow_group_ids = [pushward_org_group.ops.id]
  note            = "Deploys go to ops"
}
`)
	second := f.config(base + `
resource "pushward_org_route" "deploys" {
  slug_pattern  = "deploy-prod-*"
  applies_to    = "activities"
  allow_tag_ids = [pushward_org_tag.pager.id]
}
`)
	var id string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config: first,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "slug_pattern", "deploy-*"),
					resource.TestCheckResourceAttr(n, "applies_to", "both"),
					resource.TestCheckTypeSetElemAttrPair(n, "allow_group_ids.*", "pushward_org_group.ops", "id"),
					resource.TestCheckResourceAttr(n, "allow_tag_ids.#", "0"),
					resource.TestCheckResourceAttr(n, "note", "Deploys go to ops"),
					saveAttr(n, "id", &id),
				),
			},
			{
				Config: second,
				Check: resource.ComposeAggregateTestCheckFunc(
					attrIs(n, "id", &id),
					resource.TestCheckResourceAttr(n, "slug_pattern", "deploy-prod-*"),
					resource.TestCheckResourceAttr(n, "applies_to", "activities"),
					resource.TestCheckResourceAttr(n, "allow_group_ids.#", "0"),
					resource.TestCheckTypeSetElemAttrPair(n, "allow_tag_ids.*", "pushward_org_tag.pager", "id"),
					resource.TestCheckNoResourceAttr(n, "note"),
				),
			},
			{ResourceName: n, ImportState: true, ImportStateVerify: true},
			{ResourceName: n, ImportState: true, ImportStateKind: resource.ImportBlockWithID},
			{
				PreConfig: func() { f.drop(id) },
				Config:    second,
				Check:     attrNot(n, "id", &id),
			},
		},
	})
	if n := f.count("POST", "/routes/apply"); n != 0 {
		t.Errorf("routes/apply called %d times; every change applies itself", n)
	}
}

func TestAccRouteOrder(t *testing.T) {
	f := newFake(t)
	const n = "pushward_org_route_order.this"
	cfg := func(order string) string {
		return f.config(`
resource "pushward_org_route" "a" {
  slug_pattern = "a-*"
  applies_to   = "both"
}
resource "pushward_org_route" "b" {
  slug_pattern = "b-*"
  applies_to   = "widgets"
}
resource "pushward_org_route_order" "this" {
  route_ids = ` + order + `
}
`)
	}
	order := func(names ...string) resource.TestCheckFunc {
		return func(s *terraform.State) error {
			var want []string
			for _, name := range names {
				want = append(want, s.RootModule().Resources["pushward_org_route."+name].Primary.ID)
			}
			if got := f.routeOrder(); !slices.Equal(got, want) {
				return fmt.Errorf("order on the server = %v, want %v", got, want)
			}
			return nil
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config: cfg(`[pushward_org_route.b.id, pushward_org_route.a.id]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					order("b", "a"),
					resource.TestCheckResourceAttr(n, "id", f.org.ID),
					resource.TestCheckResourceAttrPair(n, "route_ids.0", "pushward_org_route.b", "id"),
				),
			},
			{
				Config: cfg(`[pushward_org_route.a.id, pushward_org_route.b.id]`),
				Check:  order("a", "b"),
			},
			{ResourceName: n, ImportState: true, ImportStateId: "order", ImportStateVerify: true},
			{ResourceName: n, ImportState: true, ImportStateId: "order", ImportStateKind: resource.ImportBlockWithID},
			{
				// Reordered outside Terraform: the next apply puts it back.
				PreConfig: func() {
					f.mu.Lock()
					slices.Reverse(f.routes)
					f.mu.Unlock()
				},
				Config: cfg(`[pushward_org_route.a.id, pushward_org_route.b.id]`),
				Check:  order("a", "b"),
			},
		},
	})
}

func TestAccKey(t *testing.T) {
	f := newFake(t)
	const n = "pushward_org_key.ci"
	base := `
resource "pushward_org_group" "ops" { name = "ops" }
resource "pushward_org_tag" "pager" { name = "pager" }
resource "pushward_org_key" "mail" { name = "mail" }
`
	// A year ahead, written with an offset: the state keeps this spelling.
	expires := time.Now().In(time.FixedZone("", 3600)).AddDate(1, 0, 0).Truncate(time.Second).Format(time.RFC3339)
	first := f.config(base + fmt.Sprintf(`
resource "pushward_org_key" "ci" {
  name           = "ci"
  permissions    = { activities = "update", widgets = "write" }
  activity_slugs = ["deploy-*"]
  expires_at     = %q
  target         = { group_ids = [pushward_org_group.ops.id] }
}
`, expires))
	second := f.config(base + `
resource "pushward_org_key" "ci" {
  name        = "ci-deploy"
  permissions = { activities = "manage", notifications = "send" }
  target      = { group_ids = [pushward_org_group.ops.id] }
}
`)
	third := f.config(base + `
resource "pushward_org_key" "ci" {
  name        = "ci-deploy"
  permissions = { activities = "manage", notifications = "send" }
  target      = { group_ids = [pushward_org_group.ops.id], tag_ids = [pushward_org_tag.pager.id] }
}
`)
	var id, secret string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config: first,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(n, "secret", regexp.MustCompile(`^hlk_`)),
					resource.TestCheckResourceAttr(n, "permissions.activities", "update"),
					resource.TestCheckResourceAttr(n, "permissions.notifications", "schedule"),
					resource.TestCheckResourceAttr(n, "permissions.widgets", "write"),
					resource.TestCheckResourceAttr(n, "permissions.emails", "none"),
					resource.TestCheckTypeSetElemAttr(n, "activity_slugs.*", "deploy-*"),
					resource.TestCheckResourceAttr(n, "widget_slugs.#", "0"),
					resource.TestCheckResourceAttr(n, "expires_at", expires),
					resource.TestCheckTypeSetElemAttrPair(n, "target.group_ids.*", "pushward_org_group.ops", "id"),
					resource.TestCheckResourceAttr(n, "target.tag_ids.#", "0"),
					resource.TestCheckResourceAttr("pushward_org_key.mail", "permissions.activities", "manage"),
					resource.TestCheckNoResourceAttr("pushward_org_key.mail", "target"),
					saveAttr(n, "id", &id),
					saveAttr(n, "secret", &secret),
				),
			},
			{
				Config: second,
				Check: resource.ComposeAggregateTestCheckFunc(
					attrIs(n, "id", &id),
					attrIs(n, "secret", &secret),
					resource.TestCheckResourceAttr(n, "name", "ci-deploy"),
					resource.TestCheckResourceAttr(n, "permissions.notifications", "send"),
					resource.TestCheckResourceAttr(n, "permissions.widgets", "none"),
					resource.TestCheckResourceAttr(n, "activity_slugs.#", "0"),
					resource.TestCheckNoResourceAttr(n, "expires_at"),
				),
			},
			{
				ResourceName: n, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"secret"},
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					if v, ok := is[0].Attributes["secret"]; ok {
						return fmt.Errorf("secret after import = %q, want null", v)
					}
					return nil
				},
			},
			{ResourceName: n, ImportState: true, ImportStateKind: resource.ImportBlockWithID},
			{
				Config: third,
				Check: resource.ComposeAggregateTestCheckFunc(
					attrNot(n, "id", &id),
					attrNot(n, "secret", &secret),
					resource.TestCheckResourceAttr(n, "target.tag_ids.#", "1"),
					saveAttr(n, "id", &id),
				),
			},
			{
				PreConfig: func() { f.drop(id) },
				Config:    third,
				Check:     attrNot(n, "id", &id),
			},
		},
	})
}

// A 404 that is not the resource's own code is an error, never a removal.
func TestAccLostOrgKeepsState(t *testing.T) {
	f := newFake(t)
	cfg := f.config(`resource "pushward_org_tag" "pager" { name = "pager" }`)
	var id string
	setGone := func(v bool) func() {
		return func() {
			f.mu.Lock()
			f.orgGone = v
			f.mu.Unlock()
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{Config: cfg, Check: saveAttr("pushward_org_tag.pager", "id", &id)},
			{PreConfig: setGone(true), Config: cfg, ExpectError: regexp.MustCompile(`org\.not_found`)},
			{PreConfig: setGone(false), Config: cfg, Check: attrIs("pushward_org_tag.pager", "id", &id)},
		},
	})
	if n := f.count("POST", "/tags"); n != 1 {
		t.Errorf("the tag was created %d times", n)
	}
}

// A key of another organization is an error, never a reason to read every
// resource as gone and create it again in that organization.
func TestAccOtherOrgKeyKeepsState(t *testing.T) {
	for _, tc := range []struct{ addr, body string }{
		{"pushward_org_group.x", `resource "pushward_org_group" "x" { name = "ops" }`},
		{"pushward_org_tag.x", `resource "pushward_org_tag" "x" { name = "pager" }`},
		{"pushward_org_route.x", "resource \"pushward_org_route\" \"x\" {\n  slug_pattern = \"*\"\n  applies_to = \"both\"\n}"},
		{"pushward_org_route_order.x", `resource "pushward_org_route_order" "x" { route_ids = [] }`},
		{"pushward_org_key.x", `resource "pushward_org_key" "x" { name = "ci" }`},
		{"pushward_org_member.x", "resource \"pushward_org_member\" \"x\" {\n  email = \"ann@example.com\"\n  role = \"member\"\n}"},
		{"pushward_org_tag_devices.x", "resource \"pushward_org_tag\" \"t\" { name = \"pager\" }\n" +
			"resource \"pushward_org_tag_devices\" \"x\" {\n  tag_id = pushward_org_tag.t.id\n  devices = []\n}"},
		{"pushward_org_settings.x", `resource "pushward_org_settings" "x" { name = "Acme" }`},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			a, b := newFake(t), newFake(t)
			b.mu.Lock()
			b.org.ID = "0a000000-0000-4000-8000-000000000002"
			b.mu.Unlock()
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: a.config(tc.body), Check: resource.TestCheckResourceAttr(tc.addr, "org_id", a.org.ID)},
					{
						Config:      b.config(tc.body),
						ExpectError: regexp.MustCompile(`(?s)another\s+organization.*` + a.org.ID + `.*` + b.org.ID),
					},
					{Config: a.config(tc.body), PlanOnly: true},
				},
			})
			if n := b.count("POST", "") + b.count("PUT", ""); n != 0 {
				t.Errorf("the other organization got %d writes", n)
			}
		})
	}
}

// Update and Delete check the organization too: a saved plan applied with
// another organization's key, or a destroy with -refresh=false, never goes
// through Read. Without the check, Delete would meet the resource's own 404
// over there and drop it from the state.
func TestOtherOrgKeyUpdateDelete(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c := client.New(f.srv.URL, "hlo_test", "test")
	c.OrgID = f.org.ID
	const other = "0a000000-0000-4000-8000-000000000002"
	for _, newRes := range (&pushwardProvider{}).Resources(ctx) {
		r := newRes()
		var md fwresource.MetadataResponse
		r.Metadata(ctx, fwresource.MetadataRequest{ProviderTypeName: "pushward"}, &md)
		t.Run(md.TypeName, func(t *testing.T) {
			r.(fwresource.ResourceWithConfigure).Configure(ctx, fwresource.ConfigureRequest{ProviderData: c}, &fwresource.ConfigureResponse{})
			var sr fwresource.SchemaResponse
			r.Schema(ctx, fwresource.SchemaRequest{}, &sr)
			null := tftypes.NewValue(sr.Schema.Type().TerraformType(ctx), nil)
			st := tfsdk.State{Schema: sr.Schema, Raw: null}
			d := st.SetAttribute(ctx, path.Root("id"), "00000000-0000-4000-8000-000000000099")
			if d.Append(st.SetAttribute(ctx, path.Root("org_id"), other)...); d.HasError() {
				t.Fatal(d)
			}
			newState := func() tfsdk.State { return tfsdk.State{Schema: sr.Schema, Raw: null} }

			upd := fwresource.UpdateResponse{State: newState()}
			r.Update(ctx, fwresource.UpdateRequest{Plan: tfsdk.Plan{Schema: sr.Schema, Raw: st.Raw}, State: st}, &upd)
			del := fwresource.DeleteResponse{State: newState()}
			r.Delete(ctx, fwresource.DeleteRequest{State: st}, &del)
			ops := map[string]diag.Diagnostics{"Update": upd.Diagnostics}
			// These Deletes send nothing at all; the request check below covers them.
			if md.TypeName != "pushward_org_route_order" && md.TypeName != "pushward_org_settings" {
				ops["Delete"] = del.Diagnostics
			}
			for op, diags := range ops {
				if len(diags) != 1 || diags[0].Summary() != "The org admin key belongs to another organization" {
					t.Errorf("%s diagnostics = %v, want the other-organization error", op, diags)
				}
			}
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Errorf("requests reached the other organization: %v", f.requests)
	}
}

func TestFakeRefusesEmailsOnTargetedKeyUpdate(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c := client.New(f.srv.URL, "hlo_test", "test")
	c.OrgID = f.org.ID
	var k client.Key
	body := map[string]any{"name": "ci", "target": map[string]any{"tag_ids": []string{"0b000000-0000-4000-8000-000000000001"}}}
	if err := c.Do(ctx, "POST", c.OrgPath("keys"), body, &k); err != nil {
		t.Fatal(err)
	}
	perms := map[string]string{"activities": "manage", "notifications": "schedule", "widgets": "none", "emails": "send"}
	err := c.Do(ctx, "PATCH", c.OrgPath("keys", k.ID), map[string]any{"name": "ci-mail", "permissions": perms}, nil)
	if !client.HasCode(err, "org.key_permission_unavailable") {
		t.Fatalf("PATCH emails = send on a targeted key: err = %v, want 422 org.key_permission_unavailable", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.keys[k.ID]; got.Name != "ci" || got.Permissions.Emails != "none" {
		t.Errorf("refused PATCH changed the key: name %q, emails %q", got.Name, got.Permissions.Emails)
	}
}

func TestAccRouteOrderNamesMissingRule(t *testing.T) {
	f := newFake(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{{
			Config: f.config(`
resource "pushward_org_route" "a" {
  slug_pattern = "a-*"
  applies_to   = "both"
}
resource "pushward_org_route" "b" {
  slug_pattern = "b-*"
  applies_to   = "widgets"
}
resource "pushward_org_route_order" "this" {
  route_ids  = [pushward_org_route.a.id]
  depends_on = [pushward_org_route.b]
}
`),
			ExpectError: regexp.MustCompile(`(?s)route_ids must name every routing rule.*Missing:\s+\S+ \(b-\*\)`),
		}},
	})
}

func TestAccKeyRoll(t *testing.T) {
	f := newFake(t)
	const n = "pushward_org_key.ci"
	cfg := func(triggers string) string {
		return f.config("resource \"pushward_org_key\" \"ci\" {\n  name = \"ci\"\n" + triggers + "}\n")
	}
	// requests checks how many rolls and PATCHes the fake got.
	requests := func(rolls, patches int) resource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := f.count("POST", "/roll"); got != rolls {
				return fmt.Errorf("%d rolls, want %d", got, rolls)
			}
			if got := f.count("PATCH", ""); got != patches {
				return fmt.Errorf("%d PATCHes, want %d", got, patches)
			}
			return nil
		}
	}
	var id, secret string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				// Creating the key never rolls it.
				Config: cfg(`  rotation_triggers = { rotated = "2026-11" }` + "\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "rotation_triggers.rotated", "2026-11"),
					saveAttr(n, "id", &id),
					saveAttr(n, "secret", &secret),
					requests(0, 0),
				),
			},
			{
				Config: cfg(`  rotation_triggers = { rotated = "2026-12" }` + "\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(n, plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue(n, tfjsonpath.New("secret")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					attrIs(n, "id", &id),
					attrNot(n, "secret", &secret),
					resource.TestMatchResourceAttr(n, "secret", regexp.MustCompile(`^hlk_`)),
					saveAttr(n, "secret", &secret),
					requests(1, 0),
				),
			},
			{
				// A rename with the same triggers PATCHes and keeps the secret.
				Config: f.config("resource \"pushward_org_key\" \"ci\" {\n  name = \"ci-deploy\"\n  rotation_triggers = { rotated = \"2026-12\" }\n}\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					attrIs(n, "secret", &secret),
					resource.TestCheckResourceAttr(n, "name", "ci-deploy"),
					requests(1, 1),
				),
			},
			{
				// Removing the triggers does not roll.
				Config: f.config("resource \"pushward_org_key\" \"ci\" {\n  name = \"ci-deploy\"\n}\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(n, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					attrIs(n, "id", &id),
					attrIs(n, "secret", &secret),
					resource.TestCheckNoResourceAttr(n, "rotation_triggers.%"),
					requests(1, 1),
				),
			},
		},
	})
}

// An imported key has no secret; setting rotation_triggers rolls one into the
// state.
func TestAccKeyRollAfterImport(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c := client.New(f.srv.URL, "hlo_test", "test")
	c.OrgID = f.org.ID
	var k client.Key
	if err := c.Do(ctx, "POST", c.OrgPath("keys"), map[string]any{"name": "ci"}, &k); err != nil {
		t.Fatal(err)
	}
	const n = "pushward_org_key.ci"
	cfg := func(triggers string) string {
		return f.config("resource \"pushward_org_key\" \"ci\" {\n  name = \"ci\"\n" + triggers + "}\n")
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config:             cfg(""),
				ResourceName:       n,
				ImportState:        true,
				ImportStateId:      k.ID,
				ImportStatePersist: true,
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					if v, ok := is[0].Attributes["secret"]; ok {
						return fmt.Errorf("secret after import = %q, want null", v)
					}
					return nil
				},
			},
			{
				Config: cfg(`  rotation_triggers = { rotated = "2026-11" }` + "\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "id", k.ID),
					resource.TestMatchResourceAttr(n, "secret", regexp.MustCompile(`^hlk_`)),
					func(s *terraform.State) error {
						if got := s.RootModule().Resources[n].Primary.Attributes["secret"]; got == *k.Secret {
							return fmt.Errorf("secret was not rolled")
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAccOrgSettings(t *testing.T) {
	f := newFake(t)
	const n = "pushward_org_settings.this"
	cfg := func(name string) string {
		return f.config(fmt.Sprintf("resource \"pushward_org_settings\" \"this\" {\n  name = %q\n}\n", name))
	}
	nameIs := func(want string) func(*terraform.State) error {
		return func(*terraform.State) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.org.Name != want {
				return fmt.Errorf("organization name on the server = %q, want %q", f.org.Name, want)
			}
			return nil
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		// Destroy sends nothing: the name stays.
		CheckDestroy: nameIs("Acme Inc"),
		Steps: []resource.TestStep{
			{
				Config: cfg("Acme Corp"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "id", f.org.ID),
					resource.TestCheckResourceAttr(n, "name", "Acme Corp"),
					nameIs("Acme Corp"),
				),
			},
			{Config: cfg("Acme Inc"), Check: nameIs("Acme Inc")},
			{ResourceName: n, ImportState: true, ImportStateId: "settings", ImportStateVerify: true},
			{ResourceName: n, ImportState: true, ImportStateId: "settings", ImportStateKind: resource.ImportBlockWithID},
			{
				// Renamed in the console: the next apply puts it back.
				PreConfig: func() {
					f.mu.Lock()
					f.org.Name = "Other"
					f.mu.Unlock()
				},
				Config: cfg("Acme Inc"),
				Check:  nameIs("Acme Inc"),
			},
		},
	})
	if n := f.count("DELETE", ""); n != 0 {
		t.Errorf("destroy sent %d DELETEs", n)
	}
}

// A name a group or tag made outside Terraform already has: the error says
// how to import it.
func TestAccNameTakenNamesImport(t *testing.T) {
	f := newFake(t)
	f.addGroup("ops")
	f.addTag("pager")
	for _, tc := range []struct{ body, want string }{
		{`resource "pushward_org_group" "ops" { name = "ops" }`, `group\s+named\s+ops.*terraform\s+import.*pushward_org_groups.*org\.name_taken`},
		{`resource "pushward_org_tag" "pager" { name = "pager" }`, `tag\s+named\s+pager.*terraform\s+import.*pushward_org_tags.*org\.name_taken`},
	} {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config:      f.config(tc.body),
				ExpectError: regexp.MustCompile(`(?s)` + tc.want),
			}},
		})
	}
}
