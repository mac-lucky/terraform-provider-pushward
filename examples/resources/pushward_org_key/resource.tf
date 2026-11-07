# A sending key for CI that may start and end deploy activities and send
# notifications, only to the on-call group.
resource "pushward_org_key" "ci" {
  name = "ci"

  permissions = {
    activities    = "manage"
    notifications = "send"
  }

  activity_slugs = ["deploy-*"]
  expires_at     = "2027-06-30T00:00:00Z"

  target = {
    group_ids = [pushward_org_group.oncall.id]
  }

  # Change the value to roll the secret in place: same key id, a new
  # secret, and the old one stops working at once.
  rotation_triggers = {
    rotated = "2026-11"
  }
}

output "ci_key" {
  value     = pushward_org_key.ci.secret
  sensitive = true
}
