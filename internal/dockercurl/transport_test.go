package dockercurl

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
	"github.com/withlogicco/terraform-provider-ceryx/internal/ceryx"
	"github.com/withlogicco/terraform-provider-ceryx/internal/fakeceryx"
)

const swarmServiceLabel = "com.docker.swarm.service.name"

// localDocker is a fake Docker API that runs the curl of an exec or a
// sidecar as a local process, so curl and the output parsing run for real.
type localDocker struct {
	containers []container.Summary
	images     map[string]bool
	// curl replaces the curl binary, to simulate a container without it.
	curl string

	mu       sync.Mutex
	n        int
	execs    map[string]*process
	sidecars map[string]*process
	pulls    []string
	removed  []string
}

type process struct {
	target string
	argv   []string
	config client.ContainerCreateOptions

	server   net.Conn
	exitCode int
	done     chan struct{}
}

func (d *localDocker) nextID(prefix string) string {
	d.n++
	return fmt.Sprintf("%s-%d", prefix, d.n)
}

func (d *localDocker) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: d.containers}, nil
}

// connPair returns a client and server end of a TCP connection, so the
// client end supports CloseWrite like a real hijacked connection.
func connPair() (net.Conn, net.Conn, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return nil, nil, err
	}
	s, err := ln.Accept()
	return c, s, err
}

func (d *localDocker) run(p *process) error {
	argv := p.argv
	if argv[0] == "curl" && d.curl != "" {
		argv = append([]string{d.curl}, argv[1:]...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	mu := &sync.Mutex{}
	cmd.Stdout = &muxWriter{mu: mu, w: p.server, stream: stdcopy.Stdout}
	cmd.Stderr = &muxWriter{mu: mu, w: p.server, stream: stdcopy.Stderr}
	if err := cmd.Start(); err != nil {
		// Docker reports a missing executable like this, with exit code 127.
		_, _ = cmd.Stderr.Write([]byte("exec: \"" + argv[0] + "\": executable file not found in $PATH"))
		p.exitCode = 127
		p.server.Close()
		close(p.done)
		return nil
	}
	go func() {
		_ = cmd.Wait()
		p.exitCode = cmd.ProcessState.ExitCode()
		p.server.Close()
		close(p.done)
	}()
	return nil
}

func (d *localDocker) ExecCreate(_ context.Context, id string, o client.ExecCreateOptions) (client.ExecCreateResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.execs == nil {
		d.execs = map[string]*process{}
	}
	execID := d.nextID("exec")
	d.execs[execID] = &process{target: id, argv: o.Cmd, done: make(chan struct{})}
	return client.ExecCreateResult{ID: execID}, nil
}

func (d *localDocker) ExecAttach(_ context.Context, execID string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	d.mu.Lock()
	p := d.execs[execID]
	d.mu.Unlock()
	c, s, err := connPair()
	if err != nil {
		return client.ExecAttachResult{}, err
	}
	p.server = s
	if err := d.run(p); err != nil {
		return client.ExecAttachResult{}, err
	}
	return client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(c, "")}, nil
}

func (d *localDocker) ExecInspect(_ context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	d.mu.Lock()
	p := d.execs[execID]
	d.mu.Unlock()
	select {
	case <-p.done:
		return client.ExecInspectResult{ID: execID, ContainerID: p.target, ExitCode: p.exitCode}, nil
	default:
		return client.ExecInspectResult{ID: execID, ContainerID: p.target, Running: true}, nil
	}
}

func (d *localDocker) ImageInspect(_ context.Context, image string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.images[image] {
		return client.ImageInspectResult{}, cerrdefs.ErrNotFound.WithMessage("No such image: " + image)
	}
	return client.ImageInspectResult{}, nil
}

type pullResponse struct{ io.ReadCloser }

func (pullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}
func (pullResponse) Wait(context.Context) error { return nil }

func (d *localDocker) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pulls = append(d.pulls, ref)
	if d.images == nil {
		d.images = map[string]bool{}
	}
	d.images[ref] = true
	return pullResponse{io.NopCloser(strings.NewReader(""))}, nil
}

func (d *localDocker) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sidecars == nil {
		d.sidecars = map[string]*process{}
	}
	id := d.nextID("sidecar")
	argv := append(append([]string(nil), o.Config.Entrypoint...), o.Config.Cmd...)
	d.sidecars[id] = &process{argv: argv, config: o, done: make(chan struct{})}
	return client.ContainerCreateResult{ID: id}, nil
}

func (d *localDocker) ContainerAttach(_ context.Context, id string, _ client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	d.mu.Lock()
	p := d.sidecars[id]
	d.mu.Unlock()
	c, s, err := connPair()
	if err != nil {
		return client.ContainerAttachResult{}, err
	}
	p.server = s
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(c, "")}, nil
}

func (d *localDocker) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	d.mu.Lock()
	p := d.sidecars[id]
	d.mu.Unlock()
	return client.ContainerStartResult{}, d.run(p)
}

func (d *localDocker) ContainerWait(_ context.Context, id string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
	d.mu.Lock()
	p := d.sidecars[id]
	d.mu.Unlock()
	result := make(chan container.WaitResponse, 1)
	go func() {
		<-p.done
		result <- container.WaitResponse{StatusCode: int64(p.exitCode)}
	}()
	return client.ContainerWaitResult{Result: result, Error: make(chan error)}
}

func (d *localDocker) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, id)
	return client.ContainerRemoveResult{}, nil
}

// muxWriter frames writes the way the Docker daemon multiplexes output.
type muxWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	stream stdcopy.StdType
}

func (m *muxWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	header := [8]byte{byte(m.stream)}
	binary.BigEndian.PutUint32(header[4:], uint32(len(p)))
	if _, err := m.w.Write(header[:]); err != nil {
		return 0, err
	}
	return m.w.Write(p)
}

var ceryxID = "0123456789abcdef0123456789abcdef"

func newDocker() *localDocker {
	return &localDocker{containers: []container.Summary{{
		ID:     ceryxID,
		Names:  []string{"/ingress_ceryx.1.abc"},
		Labels: map[string]string{swarmServiceLabel: "ingress_ceryx"},
	}}}
}

func newCeryx(t *testing.T, docker *localDocker, connector Connector) (*ceryx.Client, *fakeceryx.API) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	api := fakeceryx.New()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	c, err := ceryx.NewClient(server.URL, &Transport{
		API:       docker,
		Selector:  Selector{Labels: map[string]string{swarmServiceLabel: "ingress_ceryx"}},
		Connector: connector,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, api
}

func exerciseRoutes(t *testing.T, c *ceryx.Client, api *fakeceryx.API) {
	t.Helper()
	ctx := context.Background()
	// A target with characters a shell would mangle reaches Ceryx intact.
	target := `http://app:8000/a b?c='d'&e="f"`
	if _, err := c.CreateRoute(ctx, ceryx.Route{Source: "app.example.com", Target: target, Settings: ceryx.Settings{EnforceHTTPS: true, Mode: "proxy"}}); err != nil {
		t.Fatal(err)
	}
	route, found, err := c.GetRoute(ctx, "app.example.com")
	if err != nil || !found || route.Target != target || !route.Settings.EnforceHTTPS {
		t.Fatalf("GetRoute = %+v, %v, %v", route, found, err)
	}
	if _, found, err := c.GetRoute(ctx, "missing.example.com"); err != nil || found {
		t.Fatalf("GetRoute(missing) = %v, %v", found, err)
	}
	if _, err := c.UpdateRoute(ctx, "app.example.com", "http://web:80", ceryx.Settings{Mode: "redirect"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRoute(ctx, "app.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.Target("app.example.com"); ok {
		t.Fatal("route still stored after delete")
	}
}

func TestSidecarConnector(t *testing.T) {
	docker := newDocker()
	c, api := newCeryx(t, docker, &Sidecar{API: docker, Image: DefaultSidecarImage})
	exerciseRoutes(t, c, api)

	if len(docker.pulls) != 1 || docker.pulls[0] != DefaultSidecarImage {
		t.Fatalf("pulls = %v, want one pull of %s", docker.pulls, DefaultSidecarImage)
	}
	if len(docker.removed) != len(docker.sidecars) {
		t.Fatalf("removed %d of %d sidecars", len(docker.removed), len(docker.sidecars))
	}
	sidecar := docker.sidecars["sidecar-1"].config
	if got := string(sidecar.HostConfig.NetworkMode); got != "container:"+ceryxID {
		t.Fatalf("network mode = %q", got)
	}
	if sidecar.Config.Image != DefaultSidecarImage || strings.Join(sidecar.Config.Entrypoint, " ") != "curl" {
		t.Fatalf("sidecar config = %+v", sidecar.Config)
	}
	if sidecar.Config.Labels[SidecarLabel] != ceryxID {
		t.Fatalf("sidecar labels = %v", sidecar.Config.Labels)
	}
}

func TestSidecarSkipsPullOfPresentImage(t *testing.T) {
	docker := newDocker()
	docker.images = map[string]bool{"curl:local": true}
	c, _ := newCeryx(t, docker, &Sidecar{API: docker, Image: "curl:local"})
	if _, err := c.ListRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(docker.pulls) != 0 {
		t.Fatalf("pulled %v", docker.pulls)
	}
}

func TestExecConnector(t *testing.T) {
	docker := newDocker()
	c, api := newCeryx(t, docker, &Exec{API: docker})
	exerciseRoutes(t, c, api)

	p := docker.execs["exec-1"]
	if p.target != ceryxID || p.argv[0] != "curl" {
		t.Fatalf("exec = %q in %q", p.argv, p.target)
	}
	if len(docker.sidecars) != 0 || len(docker.pulls) != 0 {
		t.Fatal("exec connector created sidecars or pulled images")
	}
}

func TestExecConnectorWithoutCurl(t *testing.T) {
	docker := newDocker()
	docker.curl = "/nonexistent/curl"
	c, _ := newCeryx(t, docker, &Exec{API: docker})
	_, err := c.ListRoutes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exited with code 127") || !strings.Contains(err.Error(), "sidecar connector does not need it") {
		t.Fatalf("err = %v", err)
	}
}

func TestCurlConnectionError(t *testing.T) {
	docker := newDocker()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c, err := ceryx.NewClient("http://"+addr, &Transport{API: docker, Selector: Selector{ID: "0123"}, Connector: &Sidecar{API: docker, Image: "curl"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListRoutes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exited with code 7") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseCurlOutput(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/routes/", nil)

	// curl prints the head as received and the body already de-chunked.
	out := "HTTP/1.1 100 Continue\r\n\r\n" +
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n[]\n"
	res, err := parseCurlOutput([]byte(out), req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(body) != "[]\n" || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("res = %d %q %v", res.StatusCode, body, res.Header)
	}

	if _, err := parseCurlOutput([]byte("garbage"), req); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestCurlArgs(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/routes/", strings.NewReader(`{"source":"a"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	args, err := curlArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `--silent --show-error --include --max-time 60 --request POST --header Accept: application/json --header Content-Type: application/json --data-raw {"source":"a"} --url http://127.0.0.1:8080/api/routes/`
	if got := strings.Join(args, " "); got != want {
		t.Fatalf("args =\n%s\nwant\n%s", got, want)
	}
}
