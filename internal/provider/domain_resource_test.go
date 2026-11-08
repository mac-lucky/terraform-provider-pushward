package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

func TestCanonicalDomain(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":         "example.com",
		" Example.COM. ":      "example.com",
		"B\u00fccher.de":      "xn--bcher-kva.de",
		"xn--bcher-kva.de":    "xn--bcher-kva.de",
		"_dmarc.Example.com.": "_dmarc.example.com",
		"":                    "",
	} {
		if got := canonicalDomain(in); got != want {
			t.Errorf("canonicalDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifyAgain(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&client.Error{Status: 422, Code: client.CodeDomainNotVerified}, true},
		{&client.Error{Status: 503, Code: "service.unavailable"}, true},
		{&client.Error{Status: 409, Code: client.CodeDomainTaken}, false},
		{&client.Error{Status: 404, Code: client.CodeDomainNotFound}, false},
		{&client.Error{Status: 422, Code: "org.domain_invalid"}, false},
		{&client.Error{Status: 500}, true},
		{&client.Error{Status: 502}, true},
		{&url.Error{Op: "Post", URL: "https://api.pushward.app", Err: errors.New("connection reset by peer")}, true},
		{&url.Error{Op: "Post", URL: "https://api.pushward.app", Err: &tls.CertificateVerificationError{Err: errors.New("unknown authority")}}, false},
		{errors.New("encoding the request"), false},
	} {
		if got := verifyAgain(tc.err); got != tc.want {
			t.Errorf("verifyAgain(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestAccDomain(t *testing.T) {
	f := newFake(t)
	const n = "pushward_org_domain.example"
	cfg := func(domain string) string {
		return f.config(fmt.Sprintf("resource \"pushward_org_domain\" \"example\" {\n  domain = %q\n}\n", domain))
	}
	var id string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config: cfg("example.com"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "domain", "example.com"),
					resource.TestCheckResourceAttr(n, "status", "pending"),
					resource.TestCheckResourceAttr(n, "txt_name", "_pushward-verification.example.com"),
					resource.TestMatchResourceAttr(n, "txt_value", regexp.MustCompile(`^pushward-verification=\w+$`)),
					resource.TestCheckNoResourceAttr(n, "verified_at"),
					resource.TestCheckResourceAttr(n, "org_id", f.org.ID),
					saveAttr(n, "id", &id),
				),
			},
			{ResourceName: n, ImportState: true, ImportStateVerify: true},
			{ResourceName: n, ImportState: true, ImportStateId: "example.com", ImportStateVerify: true},
			{ResourceName: n, ImportState: true, ImportStateId: "Example.COM.", ImportStateVerify: true},
			{ResourceName: n, ImportState: true, ImportStateKind: resource.ImportBlockWithID},
			{
				// Removed in the console: the next apply adds it again.
				PreConfig: func() { f.drop(id) },
				Config:    cfg("example.com"),
				Check:     resource.ComposeAggregateTestCheckFunc(attrNot(n, "id", &id), saveAttr(n, "id", &id)),
			},
			{
				Config: cfg("example.org"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(n, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					attrNot(n, "id", &id),
					f.rows("domains", func() int { return len(f.domains) }),
					func(*terraform.State) error {
						if s := f.domainStatus("example.org"); s != "pending" {
							return fmt.Errorf("example.org is %q on the server, want pending", s)
						}
						return nil
					},
				),
			},
		},
	})
}

// A domain added in the console: the error says how to import it, and no
// create is sent.
func TestAccDomainExistsNamesImport(t *testing.T) {
	f := newFake(t)
	f.addDomain("example.com", false)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      f.config(`resource "pushward_org_domain" "example" { domain = "example.com" }`),
			ExpectError: regexp.MustCompile(`(?s)already\s+has\s+the\s+domain\s+example\.com.*terraform\s+import`),
		}},
	})
	if n := f.count("POST", "/domains"); n != 0 {
		t.Errorf("POST /domains sent %d times", n)
	}
}

// The state keeps the domain as written. Another spelling of it is no
// change in PushWard; another domain replaces it.
func TestAccDomainSpelling(t *testing.T) {
	f := newFake(t)
	const n = "pushward_org_domain.example"
	cfg := func(domain string) string {
		return f.config(fmt.Sprintf("resource \"pushward_org_domain\" \"example\" {\n  domain = %q\n}\n", domain))
	}
	var id string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config: cfg("Example.COM."),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "domain", "Example.COM."),
					resource.TestCheckResourceAttr(n, "txt_name", "_pushward-verification.example.com"),
					saveAttr(n, "id", &id),
				),
			},
			{
				Config: cfg("example.com"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(n, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "domain", "example.com"),
					resource.TestCheckResourceAttrPtr(n, "id", &id),
				),
			},
			{
				Config: cfg("B\u00fccher.de"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(n, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "domain", "B\u00fccher.de"),
					resource.TestCheckResourceAttr(n, "txt_name", "_pushward-verification.xn--bcher-kva.de"),
					f.rows("domains", func() int { return len(f.domains) }),
				),
			},
		},
	})
	if n := f.count("POST", "/domains"); n != 2 {
		t.Errorf("POST /domains sent %d times, want 2", n)
	}
}

// A domain the console adds between the list and the create is not this
// resource's: the 409 on the first attempt fails it with how to import.
func TestAccDomainAddedMeanwhile(t *testing.T) {
	f := newFake(t)
	f.mu.Lock()
	f.addFirst["example.com"] = true
	f.mu.Unlock()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      f.config(`resource "pushward_org_domain" "example" { domain = "example.com" }`),
			ExpectError: regexp.MustCompile(`(?s)already\s+has\s+the\s+domain\s+example\.com.*terraform\s+import`),
		}},
	})
	if n := f.count("POST", "/domains"); n != 1 {
		t.Errorf("POST /domains sent %d times, want 1", n)
	}
}

// fastVerify makes the verification ask again every few milliseconds.
func fastVerify(t *testing.T) {
	old := verifyEvery
	verifyEvery = 10 * time.Millisecond
	t.Cleanup(func() { verifyEvery = old })
}

func (f *fakeAPI) missVerifies(name string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyMisses[name] = n
}

// verifies fails unless the fake got n verify requests.
func (f *fakeAPI) verifies(n int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := f.count("POST", "/verify"); got != n {
			return fmt.Errorf("%d verify requests, want %d", got, n)
		}
		return nil
	}
}

const verifiedDomainConfig = `
resource "pushward_org_domain" "example" {
  domain = "example.com"
}

resource "pushward_org_domain_verification" "example" {
  domain_id = pushward_org_domain.example.id
}
`

// The TXT record shows up after a few lookups: the verification asks until
// it does. When the domain lapses back to pending, the verification stays,
// the domain's status shows the lapse, and replacing the verification
// verifies the domain again.
func TestAccDomainVerification(t *testing.T) {
	fastVerify(t)
	f := newFake(t)
	f.missVerifies("example.com", 3)
	const n = "pushward_org_domain_verification.example"
	verified := func(*terraform.State) error {
		if s := f.domainStatus("example.com"); s != "verified" {
			return fmt.Errorf("example.com is %q on the server, want verified", s)
		}
		return nil
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{
			{
				Config: f.config(verifiedDomainConfig),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(n, "id", "pushward_org_domain.example", "id"),
					resource.TestCheckResourceAttrPair(n, "domain_id", "pushward_org_domain.example", "id"),
					resource.TestCheckResourceAttr(n, "org_id", f.org.ID),
					verified,
					f.verifies(4),
				),
			},
			{
				PreConfig: func() { f.lapse("example.com") },
				Config:    f.config(verifiedDomainConfig),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(n, plancheck.ResourceActionNoop)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pushward_org_domain.example", "status", "pending"),
					resource.TestCheckNoResourceAttr("pushward_org_domain.example", "verified_at"),
					f.verifies(4),
				),
			},
			{
				PreConfig: func() { f.missVerifies("example.com", 1) },
				Config:    f.config(verifiedDomainConfig),
				Taint:     []string{n},
				Check:     resource.ComposeAggregateTestCheckFunc(verified, f.verifies(6)),
			},
			{
				// Destroying the verification alone leaves the domain verified.
				Config: f.config(`resource "pushward_org_domain" "example" { domain = "example.com" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pushward_org_domain.example", "status", "verified"),
					resource.TestCheckResourceAttrSet("pushward_org_domain.example", "verified_at"),
					verified,
				),
			},
		},
	})
}

// Another organization verified the domain first: no point in asking again.
func TestAccDomainVerificationTaken(t *testing.T) {
	fastVerify(t)
	f := newFake(t)
	f.mu.Lock()
	f.taken["example.com"] = true
	f.mu.Unlock()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{{
			Config:      f.config(verifiedDomainConfig),
			ExpectError: regexp.MustCompile(`(?s)Another\s+organization\s+verified\s+this\s+domain.*org\.domain_taken`),
		}},
	})
	if n := f.count("POST", "/verify"); n != 1 {
		t.Errorf("%d verify requests, want 1", n)
	}
}

func TestAccDomainVerificationTimeout(t *testing.T) {
	fastVerify(t)
	f := newFake(t)
	f.missVerifies("example.com", 1000)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.empty,
		Steps: []resource.TestStep{{
			Config: f.config(`
resource "pushward_org_domain" "example" {
  domain = "example.com"
}

resource "pushward_org_domain_verification" "example" {
  domain_id = pushward_org_domain.example.id
  timeouts {
    create = "300ms"
  }
}
`),
			ExpectError: regexp.MustCompile(`(?s)not\s+verified\s+in\s+time.*within\s+300ms.*org\.domain_not_verified`),
		}},
	})
	if n := f.count("POST", "/verify"); n < 2 {
		t.Errorf("%d verify requests, want it asked again", n)
	}
}

// An apply stopped while the TXT record is not visible yet says so, not that
// the create timeout ran out.
func TestDomainVerificationCancelled(t *testing.T) {
	fastVerify(t)
	f := newFake(t)
	id := f.addDomain("example.com", false)
	f.missVerifies("example.com", 1000)
	c := client.New(f.srv.URL, "hlo_test", "test")
	c.OrgID = f.org.ID
	r := &domainVerificationResource{c: c}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sr fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &sr)
	null := tftypes.NewValue(sr.Schema.Type().TerraformType(ctx), nil)
	plan := tfsdk.Plan{Schema: sr.Schema, Raw: null}
	if d := plan.SetAttribute(ctx, path.Root("domain_id"), id); d.HasError() {
		t.Fatal(d)
	}
	time.AfterFunc(50*time.Millisecond, cancel)
	resp := fwresource.CreateResponse{State: tfsdk.State{Schema: sr.Schema, Raw: null}}
	r.Create(ctx, fwresource.CreateRequest{Plan: plan}, &resp)
	if d := resp.Diagnostics; len(d) != 1 || d[0].Summary() != "Verifying the domain was cancelled" {
		t.Errorf("diagnostics = %v, want the cancel error", d)
	}
	if n := f.count("POST", "/verify"); n < 2 {
		t.Errorf("%d verify requests, want it asked again before the cancel", n)
	}
}
