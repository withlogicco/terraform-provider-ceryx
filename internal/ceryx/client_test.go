package ceryx_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/withlogicco/terraform-provider-ceryx/internal/ceryx"
	"github.com/withlogicco/terraform-provider-ceryx/internal/fakeceryx"
)

func newClient(t *testing.T) (*ceryx.Client, *fakeceryx.API) {
	t.Helper()
	api := fakeceryx.New()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	c, err := ceryx.NewClient(server.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, api
}

func ptr(s string) *string { return &s }

func TestRouteLifecycle(t *testing.T) {
	ctx := context.Background()
	c, api := newClient(t)

	if _, found, err := c.GetRoute(ctx, "app.example.com"); err != nil || found {
		t.Fatalf("GetRoute on empty API = found %v, err %v", found, err)
	}

	created, err := c.CreateRoute(ctx, ceryx.Route{
		Source:   "app.example.com",
		Target:   "http://app:8000",
		Settings: ceryx.Settings{EnforceHTTPS: true, Mode: "proxy", CertificatePath: ptr("/c.pem"), KeyPath: ptr("/k.pem")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Target != "http://app:8000" || !created.Settings.EnforceHTTPS || *created.Settings.CertificatePath != "/c.pem" {
		t.Fatalf("created = %+v", created)
	}

	// Clearing the certificate paths must reach Ceryx even though it ignores
	// null settings on PUT.
	updated, err := c.UpdateRoute(ctx, "app.example.com", "http://web:80", ceryx.Settings{Mode: "redirect"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Target != "http://web:80" || updated.Settings.EnforceHTTPS || updated.Settings.Mode != "redirect" ||
		updated.Settings.CertificatePath != nil || updated.Settings.KeyPath != nil {
		t.Fatalf("updated = %+v", updated)
	}
	if v := api.Setting("app.example.com", "certificate_path"); v != "" {
		t.Fatalf("stored certificate_path = %v, want cleared", v)
	}

	routes, err := c.ListRoutes(ctx)
	if err != nil || len(routes) != 1 || routes[0].Source != "app.example.com" {
		t.Fatalf("ListRoutes = %+v, %v", routes, err)
	}

	if err := c.DeleteRoute(ctx, "app.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRoute(ctx, "app.example.com"); err != nil {
		t.Fatalf("deleting a missing route: %v", err)
	}
	if _, ok := api.Target("app.example.com"); ok {
		t.Fatal("route still stored after delete")
	}
}

func TestWildcardSourceIsEscaped(t *testing.T) {
	ctx := context.Background()
	c, api := newClient(t)
	api.Put("$wildcard", "http://fallback", nil)

	route, found, err := c.GetRoute(ctx, "$wildcard")
	if err != nil || !found || route.Target != "http://fallback" {
		t.Fatalf("GetRoute($wildcard) = %+v, %v, %v", route, found, err)
	}
}

func TestUpdateMissingRouteIsNotFound(t *testing.T) {
	c, _ := newClient(t)
	_, err := c.UpdateRoute(context.Background(), "missing.example.com", "http://x", ceryx.Settings{Mode: "proxy"})
	if !ceryx.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestErrorDetail(t *testing.T) {
	c, _ := newClient(t)
	_, err := c.CreateRoute(context.Background(), ceryx.Route{Source: "a.example.com"})
	if err == nil || err.Error() != "Ceryx API POST /api/routes/: HTTP 400: The 'source' and 'target' fields are required." {
		t.Fatalf("err = %v", err)
	}
}

func TestNewClientRejectsRelativeURL(t *testing.T) {
	for _, u := range []string{"ceryx:8080", "/api", "ftp://ceryx"} {
		if _, err := ceryx.NewClient(u, nil); err == nil {
			t.Errorf("NewClient(%q) succeeded", u)
		}
	}
}
