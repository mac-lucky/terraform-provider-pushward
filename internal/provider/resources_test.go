package provider

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
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
