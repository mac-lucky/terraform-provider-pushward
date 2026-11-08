package provider

import (
	"fmt"
	"reflect"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

func oidcModel(issuer, secret string, version int64) *ssoModel {
	o := &ssoOIDCModel{Issuer: types.StringValue(issuer), ClientID: types.StringValue("pushward"), ClientSecret: types.StringNull(), WOVersion: types.Int64Null()}
	if secret != "" {
		o.ClientSecret = types.StringValue(secret)
	}
	if version != 0 {
		o.WOVersion = types.Int64Value(version)
	}
	return &ssoModel{Protocol: types.StringValue("oidc"), Enabled: types.BoolValue(true), RequireSSO: types.BoolValue(false), OIDC: o}
}

func samlModel(xml, url string) *ssoModel {
	s := &ssoSAMLModel{MetadataXML: types.StringNull(), MetadataURL: types.StringNull()}
	if xml != "" {
		s.MetadataXML = types.StringValue(xml)
	}
	if url != "" {
		s.MetadataURL = types.StringValue(url)
	}
	return &ssoModel{Protocol: types.StringValue("saml"), Enabled: types.BoolValue(false), RequireSSO: types.BoolValue(true), SAML: s}
}

// A save sends the client secret and the SAML metadata only when they
// changed, or with a new connection or identity provider, and the metadata
// only when there is some.
func TestSSOSends(t *testing.T) {
	const issuer = "https://idp.example.com"
	none := types.StringNull()
	unknownURL := samlModel("", "")
	unknownURL.SAML.MetadataURL = types.StringUnknown()
	for _, tc := range []struct {
		name         string
		plan, state  *ssoModel
		wo           types.String
		secret       string // "" for none
		sendMetadata bool
	}{
		{"create with a secret", oidcModel(issuer, "s1", 0), &ssoModel{}, none, "s1", false},
		{"create without one", oidcModel(issuer, "", 0), &ssoModel{}, none, "", false},
		{"unchanged", oidcModel(issuer, "s1", 0), oidcModel(issuer, "s1", 0), none, "", false},
		{"secret changed", oidcModel(issuer, "s2", 0), oidcModel(issuer, "s1", 0), none, "s2", false},
		{"secret set after an import", oidcModel(issuer, "s1", 0), oidcModel(issuer, "", 0), none, "s1", false},
		{"issuer changed", oidcModel("https://other.example.com", "s1", 0), oidcModel(issuer, "s1", 0), none, "s1", false},
		{"create write-only", oidcModel(issuer, "", 1), &ssoModel{}, types.StringValue("wo"), "wo", false},
		{"write-only, same version", oidcModel(issuer, "", 1), oidcModel(issuer, "", 1), types.StringValue("wo"), "", false},
		{"write-only, new version", oidcModel(issuer, "", 2), oidcModel(issuer, "", 1), types.StringValue("wo"), "wo", false},
		{"from saml to oidc", oidcModel(issuer, "", 1), samlModel("", "https://idp.example.com/m"), types.StringValue("wo"), "wo", false},
		{"saml create", samlModel("", "https://idp.example.com/m"), &ssoModel{}, none, "", true},
		{"saml create without metadata", unknownURL, &ssoModel{}, none, "", false},
		{"saml unchanged", samlModel("", "https://idp.example.com/m"), samlModel("", "https://idp.example.com/m"), none, "", false},
		{"saml url changed", samlModel("", "https://idp.example.com/n"), samlModel("", "https://idp.example.com/m"), none, "", true},
		{"saml xml after an import", samlModel("<md/>", ""), samlModel("", ""), none, "", true},
		{"saml xml after a url import", samlModel("<md/>", ""), samlModel("", "https://idp.example.com/m"), none, "", true},
		{"saml xml unchanged", samlModel("<md/>", ""), samlModel("<md/>", ""), none, "", false},
		{"saml xml dropped", samlModel("", ""), samlModel("<md/>", ""), none, "", false},
		{"saml without metadata after a url import", samlModel("", ""), samlModel("", "https://idp.example.com/m"), none, "", false},
		{"from oidc to saml", samlModel("<md/>", ""), oidcModel(issuer, "s1", 0), none, "", true},
		{"from oidc to saml without metadata", samlModel("", ""), oidcModel(issuer, "s1", 0), none, "", false},
	} {
		secret, metadata := ssoSends(tc.plan, tc.state, tc.wo)
		got := ""
		if secret != nil {
			got = *secret
		}
		if got != tc.secret || metadata != tc.sendMetadata {
			t.Errorf("%s: secret %q, metadata %v; want %q, %v", tc.name, got, metadata, tc.secret, tc.sendMetadata)
		}
	}
}

func TestSSOBody(t *testing.T) {
	secret := "s1"
	for _, tc := range []struct {
		name string
		got  map[string]any
		want map[string]any
	}{
		{"oidc keeping the secret", ssoBody(oidcModel("https://idp.example.com", "s1", 0), nil, false), map[string]any{
			"protocol": "oidc", "enabled": true, "require_sso": false,
			"oidc": map[string]any{"issuer": "https://idp.example.com", "client_id": "pushward"},
		}},
		{"oidc with the secret", ssoBody(oidcModel("https://idp.example.com", "s1", 0), &secret, false), map[string]any{
			"protocol": "oidc", "enabled": true, "require_sso": false,
			"oidc": map[string]any{"issuer": "https://idp.example.com", "client_id": "pushward", "client_secret": "s1"},
		}},
		{"saml keeping the provider", ssoBody(samlModel("", "https://idp.example.com/m"), nil, false), map[string]any{
			"protocol": "saml", "enabled": false, "require_sso": true, "saml": map[string]any{},
		}},
		{"saml url", ssoBody(samlModel("", "https://idp.example.com/m"), nil, true), map[string]any{
			"protocol": "saml", "enabled": false, "require_sso": true, "saml": map[string]any{"idp_metadata_url": "https://idp.example.com/m"},
		}},
		{"saml xml", ssoBody(samlModel("<md/>", ""), nil, true), map[string]any{
			"protocol": "saml", "enabled": false, "require_sso": true, "saml": map[string]any{"idp_metadata_xml": "<md/>"},
		}},
		{"saml with no url", ssoBody(samlModel("", ""), nil, true), map[string]any{
			"protocol": "saml", "enabled": false, "require_sso": true, "saml": map[string]any{},
		}},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s: body = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func (f *fakeAPI) ssoSent(secrets, metadata int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.ssoSecrets != secrets || f.ssoMetadata != metadata {
			return fmt.Errorf("the fake got %d secrets and %d metadata documents, want %d and %d", f.ssoSecrets, f.ssoMetadata, secrets, metadata)
		}
		return nil
	}
}

// ssoIs fails unless the server's connection has this secret and switches.
func (f *fakeAPI) ssoIs(secret string, enabled, requireSSO bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		s, e, r := f.ssoState()
		if s != secret || e != enabled || r != requireSSO {
			return fmt.Errorf("connection on the server: secret %q, enabled %v, require_sso %v; want %q, %v, %v", s, e, r, secret, enabled, requireSSO)
		}
		return nil
	}
}

// noSSO fails unless the fake has no connection.
func (f *fakeAPI) noSSO(*terraform.State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sso != nil {
		return fmt.Errorf("the single sign-on connection is left after destroy")
	}
	return nil
}

var ownerOnly = regexp.MustCompile(`(?s)only\s+an\s+owner\s+can\s+set\s+up\s+single\s+sign-on.*console\.pushward\.app.*terraform\s+import`)

// An admin key cannot set the connection up: an owner does in the console,
// and Terraform imports it. From then on it manages the switches and the
// client secret, which PushWard never returns, so it never shows in a plan.
func TestAccSSOConnectionOIDC(t *testing.T) {
	f := newFake(t)
	f.addDomain("example.com", true)
	const n = "pushward_org_sso_connection.this"
	cfg := func(issuer string, enabled bool, secret string) string {
		return f.config(fmt.Sprintf(`
resource "pushward_org_sso_connection" "this" {
  protocol    = "oidc"
  enabled     = %t
  require_sso = true

  oidc = {
    issuer        = %q
    client_id     = "pushward"
    client_secret = %q
  }
}
`, enabled, issuer, secret))
	}
	const issuer = "https://idp.example.com"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.noSSO,
		Steps: []resource.TestStep{
			{Config: cfg(issuer, true, "s1"), ExpectError: ownerOnly},
			{
				PreConfig:          func() { f.addSSO("oidc") },
				Config:             cfg(issuer, true, "s1"),
				ResourceName:       n,
				ImportState:        true,
				ImportStateId:      f.org.ID,
				ImportStatePersist: true,
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					a := is[0].Attributes
					if a["id"] != f.org.ID || a["protocol"] != "oidc" || a["oidc.issuer"] != issuer || a["enabled"] != "false" {
						return fmt.Errorf("imported %v", a)
					}
					if v, ok := a["oidc.client_secret"]; ok {
						return fmt.Errorf("client_secret after import = %q, want null", v)
					}
					return nil
				},
			},
			{
				// The first apply after the import sends the configured secret.
				Config: cfg(issuer, true, "s1"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "enabled", "true"),
					resource.TestCheckResourceAttr(n, "require_sso", "true"),
					resource.TestCheckResourceAttr(n, "redirect_uri", "https://console.example.com/sso"),
					resource.TestCheckResourceAttr(n, "sp_entity_id", "https://sp.example.com/saml"),
					resource.TestCheckResourceAttr(n, "acs_url", "https://sp.example.com/saml/acs"),
					resource.TestCheckResourceAttr(n, "sp_metadata_url", "https://sp.example.com/saml/metadata"),
					resource.TestCheckNoResourceAttr(n, "idp_entity_id"),
					resource.TestCheckResourceAttr(n, "oidc.client_id", "pushward"),
					resource.TestCheckNoResourceAttr(n, "saml.idp_metadata_url"),
					resource.TestCheckResourceAttr(n, "org_id", f.org.ID),
					f.ssoIs("s1", true, true),
					f.ssoSent(1, 0),
				),
			},
			{
				// A secret changed in the console is not seen: no diff.
				PreConfig: func() {
					f.mu.Lock()
					f.sso.secret = "rolled-in-console"
					f.mu.Unlock()
				},
				Config:   cfg(issuer, true, "s1"),
				PlanOnly: true,
			},
			{
				Config: cfg(issuer, false, "s2"),
				Check:  resource.ComposeAggregateTestCheckFunc(f.ssoIs("s2", false, true), f.ssoSent(2, 0)),
			},
			{
				// Only the switch changes: the stored secret stays.
				Config: cfg(issuer, true, "s2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(n, tfjsonpath.New("idp_entity_id"), knownvalue.Null()),
						plancheck.ExpectKnownValue(n, tfjsonpath.New("idp_certificate_fingerprints"), knownvalue.Null()),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(f.ssoIs("s2", true, true), f.ssoSent(2, 0)),
			},
			{ResourceName: n, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"oidc.client_secret"}},
			{Config: cfg("https://other.example.com", true, "s2"), ExpectError: ownerOnly},
		},
	})
}

// client_secret_wo is never in the plan or the state; a new
// client_secret_wo_version sends it again.
func TestAccSSOConnectionWriteOnly(t *testing.T) {
	f := newFake(t)
	f.addDomain("example.com", true)
	f.addSSO("oidc")
	const n = "pushward_org_sso_connection.this"
	cfg := func(secret string, version int) string {
		return f.config(fmt.Sprintf(`
resource "pushward_org_sso_connection" "this" {
  protocol = "oidc"
  enabled  = true

  oidc = {
    issuer                   = "https://idp.example.com"
    client_id                = "pushward"
    client_secret_wo         = %q
    client_secret_wo_version = %d
  }
}
`, secret, version))
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_11_0)},
		CheckDestroy:             f.noSSO,
		Steps: []resource.TestStep{
			{
				Config: f.config(`
resource "pushward_org_sso_connection" "this" {
  protocol = "oidc"
  enabled  = true
  oidc = {
    issuer           = "https://idp.example.com"
    client_id        = "pushward"
    client_secret    = "s"
    client_secret_wo = "wo"
  }
}
`),
				ExpectError: regexp.MustCompile(`(?s)cannot\s+be\s+specified\s+when`),
			},
			{
				// The connection exists with the same provider: creating the
				// resource adopts it.
				Config: cfg("wo-1", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(n, "oidc.client_secret_wo"),
					resource.TestCheckNoResourceAttr(n, "oidc.client_secret"),
					resource.TestCheckResourceAttr(n, "oidc.client_secret_wo_version", "1"),
					f.ssoIs("wo-1", true, false),
					f.ssoSent(1, 0),
				),
			},
			{Config: cfg("wo-other", 1), PlanOnly: true},
			{
				Config: cfg("wo-2", 2),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(n, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(f.ssoIs("wo-2", true, false), f.ssoSent(2, 0)),
			},
		},
	})
}

// A SAML connection: a change that leaves the metadata alone sends none, so
// PushWard keeps the stored identity provider and asks it nothing. Metadata
// saved as a document after a URL import leaves no URL behind.
func TestAccSSOConnectionSAML(t *testing.T) {
	f := newFake(t)
	f.addDomain("example.com", true)
	f.addSSO("saml")
	const n = "pushward_org_sso_connection.this"
	cfg := func(saml string, requireSSO bool) string {
		return f.config(fmt.Sprintf(`
resource "pushward_org_sso_connection" "this" {
  protocol    = "saml"
  enabled     = true
  require_sso = %t

  saml = {
    %s
  }
}
`, requireSSO, saml))
	}
	url := func(u string) string { return fmt.Sprintf("idp_metadata_url = %q", u) }
	xml := fmt.Sprintf("idp_metadata_xml = %q", idpMetadataXML)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.noSSO,
		Steps: []resource.TestStep{
			{
				Config:        cfg(url(idpMetadataURL), false),
				ResourceName:  n,
				ImportState:   true,
				ImportStateId: "sso",
				ExpectError:   regexp.MustCompile(`(?s)Import\s+the\s+connection\s+by\s+its\s+organization\s+id.*not\s+"sso".*terraform\s+import\s+<address>\s+` + f.org.ID),
			},
			{
				Config:             cfg(url(idpMetadataURL), false),
				ResourceName:       n,
				ImportState:        true,
				ImportStateId:      f.org.ID,
				ImportStatePersist: true,
			},
			{
				Config: cfg(url(idpMetadataURL), false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "saml.idp_metadata_url", idpMetadataURL),
					resource.TestCheckNoResourceAttr(n, "oidc.issuer"),
					resource.TestCheckResourceAttr(n, "idp_entity_id", "https://idp.example.com/saml"),
					resource.TestCheckResourceAttr(n, "idp_certificate_fingerprints.#", "2"),
					resource.TestCheckResourceAttr(n, "idp_certificate_fingerprints.0", "0A:1B"),
					resource.TestCheckResourceAttr(n, "sp_entity_id", "https://sp.example.com/saml"),
					f.ssoIs("", true, false),
					f.ssoSent(0, 0),
				),
			},
			{
				// The plan keeps the identity provider's values: none are sent.
				Config: cfg(url(idpMetadataURL), true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(n, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(n, tfjsonpath.New("idp_entity_id"), knownvalue.StringExact("https://idp.example.com/saml")),
						plancheck.ExpectKnownValue(n, tfjsonpath.New("idp_certificate_fingerprints"), knownvalue.ListSizeExact(2)),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(f.ssoIs("", true, true), f.ssoSent(0, 0)),
			},
			{ResourceName: n, ImportState: true, ImportStateVerify: true},
			{Config: cfg(url(otherMetadataURL), true), ExpectError: ownerOnly},
			{
				// The same provider's metadata as a document: PushWard keeps no URL.
				Config: cfg(xml, true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(n, tfjsonpath.New("saml").AtMapKey("idp_metadata_url"), knownvalue.Null()),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(n, "saml.idp_metadata_url"),
					resource.TestCheckResourceAttr(n, "idp_entity_id", "https://idp.example.com/saml"),
					f.ssoSent(0, 1),
				),
			},
			{
				// Without metadata in the configuration nothing is fetched, so
				// the switches change while the URL already lists the next
				// certificate.
				PreConfig: func() {
					f.mu.Lock()
					f.idpMetadata[idpMetadataURL] = samlIdP{entityID: consoleIdP.entityID, ssoURL: consoleIdP.ssoURL, fingerprints: []string{"0A:1B", "9F:9F"}}
					f.mu.Unlock()
				},
				Config: cfg("", false),
				Check:  resource.ComposeAggregateTestCheckFunc(f.ssoIs("", true, false), f.ssoSent(0, 1)),
			},
		},
	})
}

// A SAML connection the owner set up is adopted with no metadata in the
// configuration, and managed while its metadata URL is down.
func TestAccSSOConnectionSAMLAdopt(t *testing.T) {
	f := newFake(t)
	f.addDomain("example.com", true)
	f.addSSO("saml")
	f.mu.Lock()
	delete(f.idpMetadata, idpMetadataURL)
	f.mu.Unlock()
	const n = "pushward_org_sso_connection.this"
	cfg := func(saml string, requireSSO bool) string {
		return f.config(fmt.Sprintf(`
resource "pushward_org_sso_connection" "this" {
  protocol    = "saml"
  enabled     = true
  require_sso = %t
  saml        = { %s }
}
`, requireSSO, saml))
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.noSSO,
		Steps: []resource.TestStep{
			{
				Config:      cfg(fmt.Sprintf("idp_metadata_url = %q", idpMetadataURL), true),
				ExpectError: regexp.MustCompile(`org\.sso_metadata_invalid`),
			},
			{
				Config: cfg("", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(n, "saml.idp_metadata_url", idpMetadataURL),
					resource.TestCheckNoResourceAttr(n, "saml.idp_metadata_xml"),
					resource.TestCheckResourceAttr(n, "idp_entity_id", "https://idp.example.com/saml"),
					f.ssoIs("", true, true),
					f.ssoSent(0, 0),
				),
			},
			{
				Config: cfg("", false),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(n, tfjsonpath.New("saml").AtMapKey("idp_metadata_url"), knownvalue.StringExact(idpMetadataURL)),
						plancheck.ExpectKnownValue(n, tfjsonpath.New("idp_entity_id"), knownvalue.StringExact("https://idp.example.com/saml")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(f.ssoIs("", true, false), f.ssoSent(0, 0)),
			},
		},
	})
}

// A lapsed domain keeps its verification in the state, so a connection that
// depends on it still changes at once: here require_sso and the connection
// go off without waiting for a TXT record that is gone.
func TestAccSSOConnectionAfterLapse(t *testing.T) {
	fastVerify(t)
	f := newFake(t)
	f.addSSO("oidc")
	cfg := func(on bool) string {
		return f.config(verifiedDomainConfig + fmt.Sprintf(`
resource "pushward_org_sso_connection" "this" {
  protocol    = "oidc"
  enabled     = %t
  require_sso = %t
  oidc = {
    issuer    = "https://idp.example.com"
    client_id = "pushward"
  }
  depends_on = [pushward_org_domain_verification.example]
}
`, on, on))
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.noSSO,
		Steps: []resource.TestStep{
			{Config: cfg(true), Check: resource.ComposeAggregateTestCheckFunc(f.ssoIs("console-secret", true, true), f.verifies(1))},
			{
				PreConfig: func() {
					f.lapse("example.com")
					f.missVerifies("example.com", 1000)
				},
				Config: cfg(false),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("pushward_org_domain_verification.example", plancheck.ResourceActionNoop),
						plancheck.ExpectResourceAction("pushward_org_sso_connection.this", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pushward_org_domain.example", "status", "pending"),
					f.ssoIs("console-secret", false, false),
					f.verifies(1),
				),
			},
		},
	})
}

// The connection deleted in the console drops out of the state; the next
// apply would set it up again, which takes an owner.
func TestAccSSOConnectionGone(t *testing.T) {
	f := newFake(t)
	f.addSSO("oidc")
	cfg := f.config(`
resource "pushward_org_sso_connection" "this" {
  protocol = "oidc"
  enabled  = false
  oidc = {
    issuer    = "https://idp.example.com"
    client_id = "pushward"
  }
}
`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.noSSO,
		Steps: []resource.TestStep{
			{Config: cfg, Check: f.ssoSent(0, 0)},
			{
				PreConfig: func() {
					f.mu.Lock()
					f.sso = nil
					f.mu.Unlock()
				},
				Config:      cfg,
				ExpectError: ownerOnly,
			},
		},
	})
}
