data "ceryx_route" "app" {
  source = "app.example.com"
}

output "app_target" {
  value = data.ceryx_route.app.target
}
