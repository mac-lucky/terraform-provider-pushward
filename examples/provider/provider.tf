terraform {
  required_providers {
    pushward = {
      source = "mac-lucky/pushward"
    }
  }
}

# The key is read from PUSHWARD_ORG_API_TOKEN when api_key is not set.
provider "pushward" {
  api_key = var.pushward_org_key
}

variable "pushward_org_key" {
  type      = string
  sensitive = true
}
