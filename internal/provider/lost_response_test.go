package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// A create whose response is lost after the server made the row is sent
// again with the same Idempotency-Key, and Terraform ends up with that one
// row and a clean plan.
func TestAccCreateResponseLost(t *testing.T) {
	for _, tc := range []struct {
		name, path, config string
		check              func(f *fakeAPI) resource.TestCheckFunc
	}{
		{
			// A second rule would also fail the order, which must name them all.
			name: "route", path: "routes",
			config: `
resource "pushward_org_route" "deploys" {
  slug_pattern = "deploy-*"
  applies_to   = "both"
}
resource "pushward_org_route_order" "this" {
  route_ids = [pushward_org_route.deploys.id]
}
`,
			check: func(f *fakeAPI) resource.TestCheckFunc {
				return f.rows("rules", func() int { return len(f.routes) })
			},
		},
		{
			name: "group", path: "groups",
			config: `resource "pushward_org_group" "ops" { name = "ops" }`,
			check: func(f *fakeAPI) resource.TestCheckFunc {
				return f.rows("groups", func() int { return len(f.groups) })
			},
		},
		{
			name: "key", path: "keys",
			config: `resource "pushward_org_key" "ci" { name = "ci" }`,
			check: func(f *fakeAPI) resource.TestCheckFunc {
				return resource.ComposeAggregateTestCheckFunc(
					f.rows("keys", func() int { return len(f.keys) }),
					resource.TestCheckResourceAttrSet("pushward_org_key.ci", "secret"),
					func(*terraform.State) error {
						if n := f.count("POST", "/roll"); n != 1 {
							return fmt.Errorf("rolled %d times, want 1", n)
						}
						return nil
					},
				)
			},
		},
		{
			// PushWard dedupes no domain create: the retry meets 409
			// org.domain_exists, and the resource adopts the domain.
			name: "domain", path: "domains",
			config: `resource "pushward_org_domain" "example" { domain = "example.com" }`,
			check: func(f *fakeAPI) resource.TestCheckFunc {
				return resource.ComposeAggregateTestCheckFunc(
					f.rows("domains", func() int { return len(f.domains) }),
					resource.TestCheckResourceAttr("pushward_org_domain.example", "status", "pending"),
				)
			},
		},
		{
			name: "invite", path: "invites",
			config: `
resource "pushward_org_member" "ann" {
  email = "ann@example.com"
  role  = "member"
}
`,
			check: func(f *fakeAPI) resource.TestCheckFunc {
				return resource.ComposeAggregateTestCheckFunc(
					f.rows("invites mailed", func() int { return len(f.invites) }),
					resource.TestCheckResourceAttr("pushward_org_member.ann", "status", "pending"),
				)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.loseResponse("POST /orgs/{org}/" + tc.path)
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: factories,
				CheckDestroy:             f.empty,
				Steps: []resource.TestStep{{
					Config: f.config(tc.config),
					Check: resource.ComposeAggregateTestCheckFunc(
						tc.check(f),
						func(*terraform.State) error {
							if n := f.count("POST", "/"+tc.path); n != 2 {
								return fmt.Errorf("POST /%s sent %d times, want the lost attempt and one retry", tc.path, n)
							}
							return nil
						},
					),
				}},
			})
		})
	}
}

// rows fails unless n, read under the fake's lock, is 1.
func (f *fakeAPI) rows(what string, n func() int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := n(); got != 1 {
			return fmt.Errorf("the fake has %d %s, want 1", got, what)
		}
		return nil
	}
}
