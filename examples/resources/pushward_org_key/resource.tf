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

  # Bump the input to rotate the secret: the key is replaced.
  lifecycle {
    replace_triggered_by = [terraform_data.ci_key_rotation]
  }
}

resource "terraform_data" "ci_key_rotation" {
  input = "2026-11"
}

output "ci_key" {
  value     = pushward_org_key.ci.secret
  sensitive = true
}
