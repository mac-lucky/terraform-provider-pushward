data "pushward_org_members" "all" {}

# Everyone with the viewer role, for a read-only group.
resource "pushward_org_group" "auditors" {
  name       = "auditors"
  member_ids = [for m in data.pushward_org_members.all.members : m.user_id if m.role == "viewer"]
}
