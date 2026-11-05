resource "pushward_org_group" "oncall" {
  name        = "oncall"
  description = "Gets deploy and incident alerts"

  # People who have not joined yet have no user_id; compact() leaves them out
  # and the next apply after they join adds them.
  member_ids = compact([for m in pushward_org_member.team : m.user_id])
}
