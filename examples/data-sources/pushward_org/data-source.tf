data "pushward_org" "this" {}

output "seats_free" {
  value = data.pushward_org.this.member_limit - data.pushward_org.this.member_count
}
