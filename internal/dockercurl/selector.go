// Package dockercurl reaches an HTTP API that listens inside a Docker
// container by running curl in that container's network namespace.
package dockercurl

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Selector picks one running container. Every field that is set must match.
type Selector struct {
	// ID matches a full container ID or a unique prefix of one.
	ID string
	// Name matches the container name exactly, with or without a leading "/".
	Name string
	// Labels match containers that carry every label. An empty value
	// matches any value of that label.
	Labels map[string]string
}

// IsZero reports whether the selector has no criteria.
func (s Selector) IsZero() bool {
	return s.ID == "" && s.Name == "" && len(s.Labels) == 0
}

func (s Selector) String() string {
	var parts []string
	if s.ID != "" {
		parts = append(parts, "id="+s.ID)
	}
	if s.Name != "" {
		parts = append(parts, "name="+s.Name)
	}
	for _, k := range sortedKeys(s.Labels) {
		if v := s.Labels[k]; v != "" {
			parts = append(parts, "label "+k+"="+v)
		} else {
			parts = append(parts, "label "+k)
		}
	}
	return strings.Join(parts, ", ")
}

func (s Selector) filters() client.Filters {
	f := client.Filters{}
	if s.ID != "" {
		f.Add("id", s.ID)
	}
	if s.Name != "" {
		f.Add("name", strings.TrimPrefix(s.Name, "/"))
	}
	for _, k := range sortedKeys(s.Labels) {
		if v := s.Labels[k]; v != "" {
			f.Add("label", k+"="+v)
		} else {
			f.Add("label", k)
		}
	}
	return f
}

// matches applies the checks Docker's filters leave loose: the name filter
// matches substrings, so require an exact name here.
func (s Selector) matches(c container.Summary) bool {
	if s.Name == "" {
		return true
	}
	want := "/" + strings.TrimPrefix(s.Name, "/")
	for _, n := range c.Names {
		if n == want {
			return true
		}
	}
	return false
}

// Resolve returns the ID of the one running container that s selects.
func (s Selector) Resolve(ctx context.Context, api API) (string, error) {
	if s.IsZero() {
		return "", fmt.Errorf("container selector is empty: set at least one of id, name or labels")
	}
	res, err := api.ContainerList(ctx, client.ContainerListOptions{Filters: s.filters()})
	if err != nil {
		return "", fmt.Errorf("list containers: %w", err)
	}

	var found []container.Summary
	for _, c := range res.Items {
		if s.matches(c) {
			found = append(found, c)
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("no running container matches %s", s)
	case 1:
		return found[0].ID, nil
	default:
		names := make([]string, 0, len(found))
		for _, c := range found {
			names = append(names, describe(c))
		}
		return "", fmt.Errorf("%d running containers match %s: %s; narrow the selector so it matches one", len(found), s, strings.Join(names, ", "))
	}
}

func describe(c container.Summary) string {
	id := c.ID
	if len(id) > 12 {
		id = id[:12]
	}
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/") + " (" + id + ")"
	}
	return id
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
