package dockercurl

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestSelectorFilters(t *testing.T) {
	s := Selector{
		ID:     "abc",
		Name:   "/ceryx",
		Labels: map[string]string{"com.docker.swarm.service.name": "ingress_ingress-ceryx", "role": "proxy", "managed": ""},
	}
	f := s.filters()
	if !f["id"]["abc"] || !f["name"]["ceryx"] {
		t.Fatalf("filters = %v", f)
	}
	for _, want := range []string{"com.docker.swarm.service.name=ingress_ingress-ceryx", "role=proxy", "managed"} {
		if !f["label"][want] {
			t.Errorf("label filter %q missing from %v", want, f["label"])
		}
	}
}

func TestSelectorResolve(t *testing.T) {
	ctx := context.Background()
	ceryx := container.Summary{ID: "aaaaaaaaaaaaaaaa", Names: []string{"/ceryx"}}
	ceryxAPI := container.Summary{ID: "bbbbbbbbbbbbbbbb", Names: []string{"/ceryx-api"}}

	tests := []struct {
		name     string
		selector Selector
		running  []container.Summary
		want     string
		wantErr  string
	}{
		{name: "one match", selector: Selector{Labels: map[string]string{"com.docker.swarm.service.name": "ingress"}}, running: []container.Summary{ceryx}, want: ceryx.ID},
		{name: "exact name", selector: Selector{Name: "ceryx"}, running: []container.Summary{ceryx, ceryxAPI}, want: ceryx.ID},
		{name: "name with slash", selector: Selector{Name: "/ceryx-api"}, running: []container.Summary{ceryx, ceryxAPI}, want: ceryxAPI.ID},
		{name: "none", selector: Selector{Name: "ceryx"}, running: nil, wantErr: "no running container matches name=ceryx"},
		{name: "several", selector: Selector{Labels: map[string]string{"role": "proxy"}}, running: []container.Summary{ceryx, ceryxAPI}, wantErr: "2 running containers match label role=proxy: ceryx (aaaaaaaaaaaa), ceryx-api (bbbbbbbbbbbb)"},
		{name: "empty", selector: Selector{}, wantErr: "container selector is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.selector.Resolve(ctx, &localDocker{containers: tt.running})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Resolve = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestSSHArgs(t *testing.T) {
	got, err := sshArgs("ssh://root@sl-prod-00:2222")
	if err != nil {
		t.Fatal(err)
	}
	want := "-o ConnectTimeout=30 -p 2222 -- root@sl-prod-00 docker system dial-stdio"
	if strings.Join(got, " ") != want {
		t.Fatalf("sshArgs = %q, want %q", strings.Join(got, " "), want)
	}
	if _, err := sshArgs("ssh://host/path"); err == nil {
		t.Fatal("ssh host with path accepted")
	}
}
