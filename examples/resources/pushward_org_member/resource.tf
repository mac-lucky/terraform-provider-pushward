locals {
  team = {
    "ann@example.com"   = "member"
    "piotr@example.com" = "member"
    "audit@example.com" = "viewer"
  }
}

# Each address gets an invite by email. Until the person joins, status is
# "pending" and user_id is null.
resource "pushward_org_member" "team" {
  for_each = local.team
  email    = each.key
  role     = each.value
}
