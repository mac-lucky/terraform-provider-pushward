# Starts pending; publish the TXT record below, then verify it with
# pushward_org_domain_verification.
resource "pushward_org_domain" "example" {
  domain = "example.com"
}

output "verification_record" {
  value = "${pushward_org_domain.example.txt_name} TXT \"${pushward_org_domain.example.txt_value}\""
}
