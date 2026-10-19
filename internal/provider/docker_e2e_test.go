package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/withlogicco/terraform-provider-ceryx/internal/ceryx"
	"github.com/withlogicco/terraform-provider-ceryx/internal/dockercurl"
)

// TestAccDockerE2E runs a real Ceryx container and manages a route in it
// through each connector. It needs a Docker daemon and runs only when
// CERYX_E2E_IMAGE names a Ceryx image, for example:
//
//	CERYX_E2E_IMAGE=ceryx:v1 TF_ACC=1 go test ./internal/provider -run E2E
//
// The exec connector needs curl in that image. The sidecar test deletes
// curl from its Ceryx container first, to show the sidecar does not need it.
func TestAccDockerE2E(t *testing.T) {
	image := os.Getenv("CERYX_E2E_IMAGE")
	if image == "" {
		t.Skip("set CERYX_E2E_IMAGE to run against a real Ceryx container")
	}
	t.Run("sidecar", func(t *testing.T) { testDockerE2E(t, image, "sidecar") })
	t.Run("exec", func(t *testing.T) { testDockerE2E(t, image, "exec") })
}

func testDockerE2E(t *testing.T, image, connector string) {
	label := fmt.Sprintf("ceryx-provider-e2e=%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--label", label,
		"-e", "CERYX_STORAGE_BACKEND=sqlite", "-e", "CERYX_DISABLE_LETS_ENCRYPT=true", image).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	// Check Ceryx independently of the provider, from a curl container in
	// the Ceryx container's network namespace.
	api := func(path string) (string, error) {
		out, err := exec.Command("docker", "run", "--rm", "--network", "container:"+id,
			dockercurl.DefaultSidecarImage, "-sS", defaultDockerAPI+path).CombinedOutput()
		return string(out), err
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if out, err := api("/api/routes/"); err == nil && strings.HasPrefix(strings.TrimSpace(out), "[") {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Fatalf("Ceryx API did not come up: %s", logs)
		}
		time.Sleep(time.Second)
	}

	if connector == "sidecar" {
		if out, err := exec.Command("docker", "exec", id, "sh", "-c", "rm -f $(command -v curl) && ! command -v curl").CombinedOutput(); err != nil {
			t.Fatalf("remove curl from Ceryx: %v: %s", err, out)
		}
	}

	key, value, _ := strings.Cut(label, "=")
	config := func(target string) string {
		return fmt.Sprintf(`
terraform {
  required_providers {
    ceryx = { source = "withlogicco/ceryx" }
  }
}

provider "ceryx" {
  docker {
    container {
      labels = { %q = %q }
    }

    connector {
      type = %q
    }
  }
}

resource "ceryx_route" "app" {
  source        = "e2e.example.com"
  target        = %q
  enforce_https = true
}

data "ceryx_routes" "all" {
  depends_on = [ceryx_route.app]
}
`, key, value, connector, target)
	}

	route := func() (ceryx.Route, error) {
		out, err := api("/api/routes/e2e.example.com/")
		var r ceryx.Route
		if err == nil {
			err = json.Unmarshal([]byte(out), &r)
		}
		if err != nil {
			return r, fmt.Errorf("%v: %s", err, out)
		}
		return r, nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy: func(*terraform.State) error {
			if out, _ := api("/api/routes/e2e.example.com/"); !strings.Contains(out, "No route found") {
				return fmt.Errorf("route still in Ceryx: %s", out)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: config("http://app:8000"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ceryx_route.app", "target", "http://app:8000"),
					resource.TestCheckResourceAttr("data.ceryx_routes.all", "routes.#", "1"),
					func(*terraform.State) error {
						r, err := route()
						if err != nil || r.Target != "http://app:8000" || !r.Settings.EnforceHTTPS {
							return fmt.Errorf("Ceryx reports %+v (%v)", r, err)
						}
						return nil
					},
				),
			},
			{Config: config("http://web:80"), Check: resource.TestCheckResourceAttr("ceryx_route.app", "target", "http://web:80")},
			{ResourceName: "ceryx_route.app", ImportState: true, ImportStateId: "e2e.example.com", ImportStateVerify: true, ImportStateVerifyIdentifierAttribute: "source"},
		},
	})

	// The sidecar connector leaves no containers behind.
	out, _ = exec.Command("docker", "ps", "-a", "-q", "--filter", "label="+dockercurl.SidecarLabel+"="+id).CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Fatalf("sidecar containers left behind: %s", s)
	}
}
