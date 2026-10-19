package dockercurl

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// NewDockerClient returns a Docker Engine API client for host. An empty host
// uses DOCKER_HOST, DOCKER_TLS_VERIFY and DOCKER_CERT_PATH, falling back to
// the local daemon. An ssh:// host runs "docker system dial-stdio" on the
// remote machine through the local ssh command, as the Docker CLI does, so
// it uses the same SSH config, keys and agent.
func NewDockerClient(host string) (*client.Client, error) {
	if host == "" {
		host = os.Getenv(client.EnvOverrideHost)
	}

	if strings.HasPrefix(host, "ssh://") {
		args, err := sshArgs(host)
		if err != nil {
			return nil, err
		}
		return client.New(
			client.WithHost("http://docker.ssh"),
			client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialCommand(ctx, "ssh", args...)
			}),
		)
	}

	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	return client.New(opts...)
}

func sshArgs(host string) ([]string, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("invalid Docker host %q: %w", host, err)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("invalid Docker host %q: missing host name", host)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("invalid Docker host %q: ssh hosts take no path", host)
	}
	args := []string{"-o", "ConnectTimeout=30"}
	if u.Port() != "" {
		args = append(args, "-p", u.Port())
	}
	dest := u.Hostname()
	if u.User != nil {
		dest = u.User.Username() + "@" + dest
	}
	return append(args, "--", dest, "docker", "system", "dial-stdio"), nil
}

// dialCommand runs a command and uses its stdin and stdout as a connection.
func dialCommand(ctx context.Context, name string, args ...string) (net.Conn, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = &tail{max: 4096}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	return &commandConn{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

type commandConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	once   sync.Once
}

func (c *commandConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	if err == io.EOF {
		if s := strings.TrimSpace(c.cmd.Stderr.(*tail).String()); s != "" {
			return n, fmt.Errorf("%s: %s", c.cmd.Path, s)
		}
	}
	return n, err
}

func (c *commandConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// CloseWrite closes the command's stdin, ending the stream towards the daemon.
func (c *commandConn) CloseWrite() error { return c.stdin.Close() }

func (c *commandConn) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		_ = c.stdout.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	})
	return nil
}

func (c *commandConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c *commandConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c *commandConn) SetDeadline(t time.Time) error      { return nil }
func (c *commandConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *commandConn) SetWriteDeadline(t time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "command" }
func (dummyAddr) String() string  { return "command" }

type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
