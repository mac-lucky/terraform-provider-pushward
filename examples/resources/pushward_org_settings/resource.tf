# Declare at most one. Destroying it leaves the name as it is.
resource "pushward_org_settings" "this" {
  name = "Acme Inc"
}
