# An owner sets the connection up in the PushWard console and it is imported
# (see below). Terraform then turns it on and rolls the client secret.
resource "pushward_org_sso_connection" "this" {
  protocol    = "oidc"
  enabled     = true
  require_sso = true

  oidc = {
    issuer    = "https://login.example.com"
    client_id = "pushward"

    # Never stored in the plan or state. Bump the version to send a rolled
    # secret.
    client_secret_wo         = var.sso_client_secret
    client_secret_wo_version = 1
  }

  # Turning it on needs a verified domain. Once one is verified this holds a
  # change up only while Terraform verifies a domain again after it was
  # removed or replaced; to change the connection first (say, turn
  # require_sso off), take this line out for that apply.
  depends_on = [pushward_org_domain_verification.example]
}

variable "sso_client_secret" {
  type      = string
  sensitive = true
  ephemeral = true
}

# A SAML 2.0 connection takes the identity provider's metadata instead, or
# none at all: saml = {} keeps what the owner saved, and Terraform manages the
# switches only.
#
#   protocol = "saml"
#   saml = {
#     idp_metadata_url = "https://login.example.com/saml/metadata"
#   }
