data "ceryx_routes" "all" {}

resource "ceryx_route" "app" {
  source = "app.example.com"
  target = "http://app:8000"
}

locals {
  managed   = toset([ceryx_route.app.source])
  unmanaged = setsubtract(data.ceryx_routes.all.sources, local.managed)
}

check "all_routes_managed" {
  assert {
    condition     = length(local.unmanaged) == 0
    error_message = "Routes not managed by Terraform: ${join(", ", local.unmanaged)}"
  }
}
