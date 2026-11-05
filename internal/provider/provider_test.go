package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

var factories = map[string]func() (tfprotov6.ProviderServer, error){
	"pushward": providerserver.NewProtocol6WithError(New("test")()),
}

// saveAttr stores an attribute of name in *dst.
func saveAttr(name, key string, dst *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("%s not in state", name)
		}
		*dst = rs.Primary.Attributes[key]
		return nil
	}
}

// attrNot fails when an attribute of name equals *old.
func attrNot(name, key string, old *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[name]
		if got := rs.Primary.Attributes[key]; got == *old {
			return fmt.Errorf("%s.%s is still %q", name, key, got)
		}
		return nil
	}
}

func attrIs(name, key string, want *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		return resource.TestCheckResourceAttr(name, key, *want)(s)
	}
}

func TestProviderEnvVars(t *testing.T) {
	f := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_from_env")
	t.Setenv("PUSHWARD_API_URL", f.srv.URL)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: `data "pushward_org" "this" {}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.pushward_org.this", "id", f.org.ID),
				resource.TestCheckResourceAttr("data.pushward_org.this", "name", "Acme"),
				resource.TestCheckResourceAttr("data.pushward_org.this", "slug", "acme"),
				resource.TestCheckResourceAttr("data.pushward_org.this", "plan", "team"),
				resource.TestCheckResourceAttr("data.pushward_org.this", "member_limit", "50"),
				resource.TestCheckResourceAttr("data.pushward_org.this", "created_at", "2026-01-02T03:04:05Z"),
				resource.TestCheckNoResourceAttr("data.pushward_org.this", "trial_ends_at"),
			),
		}},
	})
}

func TestProviderRefusesOtherKeys(t *testing.T) {
	f := newFake(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      fmt.Sprintf("provider \"pushward\" {\n  api_key = \"hlk_send\"\n  api_url = %q\n}\ndata \"pushward_org\" \"this\" {}\n", f.srv.URL),
			ExpectError: regexp.MustCompile(`(?s)org admin key.*console\.pushward\.app`),
		}},
	})
	if n := f.count("GET", "/orgs"); n != 0 {
		t.Errorf("an hlk_ key reached the API %d times", n)
	}
}

func TestProviderRefusesURLWithoutScheme(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      "provider \"pushward\" {\n  api_key = \"hlo_test\"\n  api_url = \"api.pushward.app\"\n}\ndata \"pushward_org\" \"this\" {}\n",
			ExpectError: regexp.MustCompile(`must be an http or https URL`),
		}},
	})
}

func TestProviderRefusesSeveralOrgs(t *testing.T) {
	f := newFake(t)
	f.extraOrgs = 1
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      f.config(`data "pushward_org" "this" {}`),
			ExpectError: regexp.MustCompile(`exactly one organization`),
		}},
	})
}

func TestDataSources(t *testing.T) {
	f := newFake(t)
	a := f.addMember("member", "a@example.com")
	f.addMember("viewer", "")
	f.addMember("member", "c@example.com")
	model, platform := "iPhone17,1", "ios"
	f.devices = []client.Device{{UserID: a, DeviceID: "dev-1", Platform: &platform, DeviceModel: &model, Delivery: true, TagIDs: []string{"t1", "t2"}}}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: f.config(`
data "pushward_org_members" "all" {}
data "pushward_org_devices" "all" {}
`),
			Check: resource.ComposeAggregateTestCheckFunc(
				// Four members across two pages of two.
				resource.TestCheckResourceAttr("data.pushward_org_members.all", "members.#", "4"),
				resource.TestCheckResourceAttr("data.pushward_org_members.all", "members.0.role", "owner"),
				resource.TestCheckResourceAttr("data.pushward_org_members.all", "members.1.user_id", a),
				resource.TestCheckResourceAttr("data.pushward_org_members.all", "members.1.email", "a@example.com"),
				resource.TestCheckNoResourceAttr("data.pushward_org_members.all", "members.2.email"),
				resource.TestCheckResourceAttr("data.pushward_org_members.all", "members.3.email", "c@example.com"),
				resource.TestCheckResourceAttr("data.pushward_org_devices.all", "devices.#", "1"),
				resource.TestCheckResourceAttr("data.pushward_org_devices.all", "devices.0.device_id", "dev-1"),
				resource.TestCheckResourceAttr("data.pushward_org_devices.all", "devices.0.platform", "ios"),
				resource.TestCheckResourceAttr("data.pushward_org_devices.all", "devices.0.delivery", "true"),
				resource.TestCheckResourceAttr("data.pushward_org_devices.all", "devices.0.tag_ids.#", "2"),
			),
		}},
	})
}
