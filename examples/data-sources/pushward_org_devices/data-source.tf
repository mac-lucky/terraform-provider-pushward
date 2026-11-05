data "pushward_org_devices" "all" {}

output "devices_without_delivery" {
  value = [for d in data.pushward_org_devices.all.devices : d.device_id if !d.delivery]
}
