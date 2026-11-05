# Every rule of the organization, first match first.
resource "pushward_org_route_order" "this" {
  route_ids = [
    pushward_org_route.deploys.id,
    pushward_org_route.rest.id,
  ]
}
