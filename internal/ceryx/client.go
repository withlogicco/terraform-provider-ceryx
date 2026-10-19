// Package ceryx is a client for the Ceryx route API.
package ceryx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Settings are the per-route settings Ceryx stores next to the target.
type Settings struct {
	EnforceHTTPS    bool    `json:"enforce_https"`
	Mode            string  `json:"mode"`
	CertificatePath *string `json:"certificate_path"`
	KeyPath         *string `json:"key_path"`
}

// Route is a Ceryx route as returned by the API.
type Route struct {
	Source   string   `json:"source"`
	Target   string   `json:"target"`
	Settings Settings `json:"settings"`
}

// APIError is a non-2xx response from the Ceryx API.
type APIError struct {
	Method, Path string
	Status       int
	Detail       string
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("Ceryx API %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Detail)
	}
	return fmt.Sprintf("Ceryx API %s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// Client talks to one Ceryx API. Every path it requests ends with "/":
// some Ceryx API versions redirect paths without one, which turns a POST
// into a GET.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for the API at baseURL. The transport carries
// requests to the API; nil means a direct HTTP connection.
func NewClient(baseURL string, transport http.RoundTripper) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("API URL %q must be absolute, for example http://127.0.0.1:8080", baseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("API URL %q must use http or https", baseURL)
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func routePath(source string) string {
	return "/api/routes/" + url.PathEscape(source) + "/"
}

// ListRoutes returns every route Ceryx holds.
func (c *Client) ListRoutes(ctx context.Context) ([]Route, error) {
	var routes []Route
	if err := c.do(ctx, http.MethodGet, "/api/routes/", nil, &routes); err != nil {
		return nil, err
	}
	return routes, nil
}

// GetRoute returns the route for source. The boolean is false when Ceryx has
// no such route.
func (c *Client) GetRoute(ctx context.Context, source string) (Route, bool, error) {
	var route Route
	err := c.do(ctx, http.MethodGet, routePath(source), nil, &route)
	if IsNotFound(err) {
		return Route{}, false, nil
	}
	if err != nil {
		return Route{}, false, err
	}
	return route, true, nil
}

// CreateRoute stores a new route. Ceryx overwrites an existing route with
// the same source, so callers that must not clobber one check first.
func (c *Client) CreateRoute(ctx context.Context, route Route) (Route, error) {
	var created Route
	err := c.do(ctx, http.MethodPost, "/api/routes/", route, &created)
	return created, err
}

// UpdateRoute replaces the target and settings of the route for source.
//
// Ceryx merges the settings of a PUT into the stored ones and ignores nulls,
// so an unset certificate or key path is sent as an empty string, which
// Ceryx stores as unset.
func (c *Client) UpdateRoute(ctx context.Context, source, target string, settings Settings) (Route, error) {
	empty := ""
	if settings.CertificatePath == nil {
		settings.CertificatePath = &empty
	}
	if settings.KeyPath == nil {
		settings.KeyPath = &empty
	}
	body := struct {
		Target   string   `json:"target"`
		Settings Settings `json:"settings"`
	}{target, settings}

	var updated Route
	err := c.do(ctx, http.MethodPut, routePath(source), body, &updated)
	return updated, err
}

// DeleteRoute removes the route for source. Deleting a missing route succeeds.
func (c *Client) DeleteRoute(ctx context.Context, source string) error {
	err := c.do(ctx, http.MethodDelete, routePath(source), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// IsNotFound reports whether err is an HTTP 404 from the API.
func IsNotFound(err error) bool {
	apiErr, ok := err.(*APIError)
	return ok && apiErr.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Ceryx API %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("Ceryx API %s %s: read response: %w", method, path, err)
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		apiErr := &APIError{Method: method, Path: path, Status: res.StatusCode}
		var detail struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &detail) == nil && detail.Detail != "" {
			apiErr.Detail = detail.Detail
		} else {
			apiErr.Detail = strings.TrimSpace(string(data))
		}
		if loc := res.Header.Get("Location"); loc != "" {
			apiErr.Detail = strings.TrimSpace(apiErr.Detail + " (redirects to " + loc + ")")
		}
		return apiErr
	}

	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("Ceryx API %s %s: decode response: %w", method, path, err)
	}
	return nil
}
