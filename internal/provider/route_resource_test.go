package provider

import (
	"fmt"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/withlogicco/terraform-provider-ceryx/internal/fakeceryx"
)

func factories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){"ceryx": providerserver.NewProtocol6WithError(New("test"))}
}

func newFakeCeryx(t *testing.T) (*fakeceryx.API, string) {
	t.Helper()
	api := fakeceryx.New()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	return api, server.URL
}

func providerConfig(url string) string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    ceryx = { source = "withlogicco/ceryx" }
  }
}

provider "ceryx" {
  endpoint = %q
}
`, url)
}

func routeConfig(url, body string) string {
	return providerConfig(url) + `
resource "ceryx_route" "app" {
  source = "app.example.com"
` + body + `
}
`
}

func TestAccRouteLifecycle(t *testing.T) {
	api, url := newFakeCeryx(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy: func(*terraform.State) error {
			if _, ok := api.Target("app.example.com"); ok {
				return fmt.Errorf("route still in Ceryx after destroy")
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: routeConfig(url, `  target = "http://app:8000"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ceryx_route.app", "target", "http://app:8000"),
					resource.TestCheckResourceAttr("ceryx_route.app", "enforce_https", "false"),
					resource.TestCheckResourceAttr("ceryx_route.app", "mode", "proxy"),
					resource.TestCheckNoResourceAttr("ceryx_route.app", "certificate_path"),
					func(*terraform.State) error {
						if target, _ := api.Target("app.example.com"); target != "http://app:8000" {
							return fmt.Errorf("Ceryx target = %q", target)
						}
						return nil
					},
				),
			},
			{
				Config: routeConfig(url, `
  target           = "http://web:80"
  enforce_https    = true
  certificate_path = "/etc/ceryx/app.crt"
  key_path         = "/etc/ceryx/app.key"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ceryx_route.app", "target", "http://web:80"),
					resource.TestCheckResourceAttr("ceryx_route.app", "enforce_https", "true"),
					resource.TestCheckResourceAttr("ceryx_route.app", "certificate_path", "/etc/ceryx/app.crt"),
				),
			},
			{
				ResourceName:                         "ceryx_route.app",
				ImportState:                          true,
				ImportStateId:                        "app.example.com",
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "source",
			},
			{
				// Removing settings from the configuration resets them in Ceryx.
				Config: routeConfig(url, `  target = "http://web:80"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ceryx_route.app", "enforce_https", "false"),
					resource.TestCheckNoResourceAttr("ceryx_route.app", "certificate_path"),
					func(*terraform.State) error {
						if v := api.Setting("app.example.com", "certificate_path"); v != "" {
							return fmt.Errorf("Ceryx certificate_path = %v, want cleared", v)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAccRouteRecreatedAfterOutOfBandDelete(t *testing.T) {
	api, url := newFakeCeryx(t)
	config := routeConfig(url, `  target = "http://app:8000"`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: config},
			{PreConfig: func() { api.Delete("app.example.com") }, Config: config, Check: func(*terraform.State) error {
				if _, ok := api.Target("app.example.com"); !ok {
					return fmt.Errorf("route not recreated")
				}
				return nil
			}},
		},
	})
}

func TestAccRouteDetectsDrift(t *testing.T) {
	api, url := newFakeCeryx(t)
	config := routeConfig(url, `  target = "http://app:8000"`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() { api.Put("app.example.com", "http://elsewhere:1", nil) },
				Config:    config,
				Check: func(*terraform.State) error {
					if target, _ := api.Target("app.example.com"); target != "http://app:8000" {
						return fmt.Errorf("drift not corrected: target = %q", target)
					}
					return nil
				},
			},
		},
	})
}

func TestAccRouteRefusesToOverwriteExistingRoute(t *testing.T) {
	api, url := newFakeCeryx(t)
	api.Put("app.example.com", "http://someone-else:80", nil)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: routeConfig(url, `  target = "http://app:8000"`), ExpectError: regexp.MustCompile("Route already exists")},
		},
	})
	if target, _ := api.Target("app.example.com"); target != "http://someone-else:80" {
		t.Fatalf("existing route overwritten: target = %q", target)
	}
}

func TestAccRouteValidatesMode(t *testing.T) {
	_, url := newFakeCeryx(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: routeConfig(url, `
  target = "http://app:8000"
  mode   = "tunnel"`), ExpectError: regexp.MustCompile(`value must be one of`)},
		},
	})
}

func TestAccRouteDataSources(t *testing.T) {
	api, url := newFakeCeryx(t)
	api.Put("other.example.com", "http://other:80", map[string]any{"enforce_https": true, "mode": "redirect"})
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: providerConfig(url) + `
data "ceryx_route" "other" {
  source = "other.example.com"
}

data "ceryx_routes" "all" {}
`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.ceryx_route.other", "target", "http://other:80"),
				resource.TestCheckResourceAttr("data.ceryx_route.other", "enforce_https", "true"),
				resource.TestCheckResourceAttr("data.ceryx_route.other", "mode", "redirect"),
				resource.TestCheckResourceAttr("data.ceryx_routes.all", "routes.#", "1"),
				resource.TestCheckResourceAttr("data.ceryx_routes.all", "routes.0.source", "other.example.com"),
				resource.TestCheckTypeSetElemAttr("data.ceryx_routes.all", "sources.*", "other.example.com"),
			),
		}},
	})
}

func TestAccRouteDataSourceMissing(t *testing.T) {
	_, url := newFakeCeryx(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      providerConfig(url) + `data "ceryx_route" "x" { source = "missing.example.com" }`,
			ExpectError: regexp.MustCompile("Route not found"),
		}},
	})
}

func TestAccProviderConfigErrors(t *testing.T) {
	head := `
terraform {
  required_providers {
    ceryx = { source = "withlogicco/ceryx" }
  }
}
`
	use := `
data "ceryx_routes" "all" {}
`
	t.Setenv(endpointEnv, "")
	cases := map[string]struct{ provider, err string }{
		"nothing":         {`provider "ceryx" {}`, "Missing Ceryx API configuration"},
		"both":            {`provider "ceryx" {` + "\n" + `endpoint = "http://x"` + "\n" + `docker {` + "\n" + `container { name = "ceryx" }` + "\n}\n}", "Set either endpoint or a docker block"},
		"no container":    {`provider "ceryx" {` + "\n" + `docker {}` + "\n}", "Missing container selector"},
		"empty selector":  {`provider "ceryx" {` + "\n" + `docker {` + "\n" + `container {}` + "\n}\n}", "Empty container selector"},
		"image with exec": {`provider "ceryx" {` + "\n" + `docker {` + "\n" + `container { name = "ceryx" }` + "\n" + `connector {` + "\n" + `type = "exec"` + "\n" + `image = "curl"` + "\n}\n}\n}", "image applies only"},
		"bad connector":   {`provider "ceryx" {` + "\n" + `docker {` + "\n" + `container { name = "ceryx" }` + "\n" + `connector {` + "\n" + `type = "ssh"` + "\n}\n}\n}", "value must be one of"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{{
					Config:      head + c.provider + use,
					ExpectError: regexp.MustCompile(regexp.QuoteMeta(c.err)),
				}},
			})
		})
	}
}
