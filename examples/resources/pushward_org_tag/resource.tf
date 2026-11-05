# Which devices carry the tag is set in the console or the app.
resource "pushward_org_tag" "pager" {
  name        = "pager"
  description = "Phones on the on-call rota"
}
