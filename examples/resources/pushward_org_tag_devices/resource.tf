data "pushward_org_devices" "all" {}

# The pager tag on every device of the on-call group's members. Built from
# the data source, so a device that unregisters simply drops out.
resource "pushward_org_tag_devices" "pager" {
  tag_id = pushward_org_tag.pager.id
  devices = [
    for d in data.pushward_org_devices.all.devices : { user_id = d.user_id, device_id = d.device_id }
    if contains(pushward_org_group.oncall.member_ids, d.user_id)
  ]
}
