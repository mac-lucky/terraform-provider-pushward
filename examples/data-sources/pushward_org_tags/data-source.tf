data "pushward_org_tags" "all" {}

# Tags made in the console, by name.
locals {
  tag_ids = { for t in data.pushward_org_tags.all.tags : t.name => t.id }
}

resource "pushward_org_route" "builds" {
  slug_pattern  = "build-*"
  applies_to    = "widgets"
  allow_tag_ids = [local.tag_ids["office-screens"]]
}
