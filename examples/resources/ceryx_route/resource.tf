resource "ceryx_route" "app" {
  source        = "app.example.com"
  target        = "http://app:8000"
  enforce_https = true
}

resource "ceryx_route" "www" {
  source = "www.example.com"
  target = "https://app.example.com"
  mode   = "redirect"
}
