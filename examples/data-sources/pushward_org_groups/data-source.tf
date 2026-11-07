data "pushward_org_groups" "all" {}

# Groups made in the console, by name.
locals {
  group_ids = { for g in data.pushward_org_groups.all.groups : g.name => g.id }
}

resource "pushward_org_route" "incidents" {
  slug_pattern    = "incident-*"
  applies_to      = "both"
  allow_group_ids = [local.group_ids["oncall"]]
}
