// Package fakeceryx is an in-memory Ceryx route API for tests. It follows
// the Ceryx Lua API: POST overwrites, PUT merges non-null settings into the
// stored ones, DELETE answers 204 even for missing routes, and paths without
// a trailing slash redirect, as some Ceryx versions do.
package fakeceryx

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
)

type route struct {
	target   string
	settings map[string]any
}

// API is an http.Handler that serves /api/routes/.
type API struct {
	mu     sync.Mutex
	routes map[string]route
}

func New() *API { return &API{routes: map[string]route{}} }

var defaults = map[string]any{"enforce_https": false, "mode": "proxy", "certificate_path": "", "key_path": ""}

func merge(dst map[string]any, src map[string]any) {
	for k := range defaults {
		if v, ok := src[k]; ok && v != nil {
			dst[k] = v
		}
	}
}

func serialize(source string, r route) map[string]any {
	s := map[string]any{}
	for k, v := range r.settings {
		s[k] = v
	}
	for _, k := range []string{"certificate_path", "key_path"} {
		if s[k] == "" {
			s[k] = nil
		}
	}
	return map[string]any{"source": source, "target": r.target, "settings": s}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	if !strings.HasPrefix(p, "/api/routes") {
		reply(w, 404, map[string]string{"detail": "Invalid route: " + p})
		return
	}
	if !strings.HasSuffix(p, "/") {
		http.Redirect(w, r, p+"/", http.StatusMovedPermanently)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(p, "/api/routes"), "/")
	host, err := url.PathUnescape(rest)
	if err != nil || strings.Contains(host, "/") {
		reply(w, 404, map[string]string{"detail": "Invalid route: " + p})
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	switch {
	case host == "" && r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, k := range a.sources() {
			out = append(out, serialize(k, a.routes[k]))
		}
		reply(w, 200, out)
	case host == "" && r.Method == http.MethodPost:
		var body struct {
			Source   string         `json:"source"`
			Target   string         `json:"target"`
			Settings map[string]any `json:"settings"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			reply(w, 400, map[string]string{"detail": "Invalid JSON in request body"})
			return
		}
		if body.Source == "" || body.Target == "" {
			reply(w, 400, map[string]string{"detail": "The 'source' and 'target' fields are required."})
			return
		}
		rt := route{target: body.Target, settings: map[string]any{}}
		merge(rt.settings, defaults)
		merge(rt.settings, body.Settings)
		a.routes[body.Source] = rt
		reply(w, 201, serialize(body.Source, rt))
	case host != "" && r.Method == http.MethodGet:
		rt, ok := a.routes[host]
		if !ok {
			reply(w, 404, map[string]string{"detail": "No route found for " + host})
			return
		}
		reply(w, 200, serialize(host, rt))
	case host != "" && r.Method == http.MethodPut:
		rt, ok := a.routes[host]
		if !ok {
			reply(w, 404, map[string]string{"detail": "No route found for " + host})
			return
		}
		var body struct {
			Target   *string        `json:"target"`
			Settings map[string]any `json:"settings"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			reply(w, 400, map[string]string{"detail": "Invalid JSON in request body"})
			return
		}
		if body.Target != nil {
			rt.target = *body.Target
		}
		merge(rt.settings, body.Settings)
		a.routes[host] = rt
		reply(w, 200, serialize(host, rt))
	case host != "" && r.Method == http.MethodDelete:
		delete(a.routes, host)
		w.WriteHeader(http.StatusNoContent)
	default:
		reply(w, 405, map[string]string{"detail": "Method not allowed"})
	}
}

func (a *API) sources() []string {
	keys := make([]string, 0, len(a.routes))
	for k := range a.routes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Put stores a route directly, as if another client had created it.
func (a *API) Put(source, target string, settings map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rt := route{target: target, settings: map[string]any{}}
	merge(rt.settings, defaults)
	merge(rt.settings, settings)
	a.routes[source] = rt
}

// Delete removes a route directly.
func (a *API) Delete(source string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.routes, source)
}

// Target returns the stored target and whether the route exists.
func (a *API) Target(source string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rt, ok := a.routes[source]
	return rt.target, ok
}

// Setting returns one stored setting of a route.
func (a *API) Setting(source, key string) any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.routes[source].settings[key]
}
