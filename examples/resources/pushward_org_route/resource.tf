# Deploy activities and their notifications go to the on-call group and to
# devices tagged pager.
resource "pushward_org_route" "deploys" {
  slug_pattern    = "deploy-*"
  applies_to      = "both"
  allow_group_ids = [pushward_org_group.oncall.id]
  allow_tag_ids   = [pushward_org_tag.pager.id]
  note            = "Deploys go to on-call"
}

# Anything else reaches nobody but the group.
resource "pushward_org_route" "rest" {
  slug_pattern    = "*"
  applies_to      = "both"
  allow_group_ids = [pushward_org_group.oncall.id]
}
