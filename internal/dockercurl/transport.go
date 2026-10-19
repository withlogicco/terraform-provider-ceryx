package dockercurl

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/moby/moby/client"
)

// API is the part of the Docker Engine API the transport and connectors use.
type API interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)

	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)

	ImageInspect(ctx context.Context, image string, options ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerAttach(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// Connector runs curl with args in the network namespace of a container
// and returns curl's stdout, stderr and exit code.
type Connector interface {
	Curl(ctx context.Context, containerID string, args []string) (stdout, stderr []byte, exitCode int, err error)
}

// Transport is an http.RoundTripper that turns each request into one curl
// call, run by Connector next to the container Selector picks. The request
// URL is resolved inside that container's network namespace, so
// http://127.0.0.1:8080 reaches a server on the container's loopback.
type Transport struct {
	API       API
	Selector  Selector
	Connector Connector
}

// curlMaxTime bounds each curl call, matching the API client's timeout.
const curlMaxTime = "60"

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	ctx := req.Context()

	containerID, err := t.Selector.Resolve(ctx, t.API)
	if err != nil {
		return nil, err
	}

	args, err := curlArgs(req)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exitCode, err := t.Connector.Curl(ctx, containerID, args)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(stdout))
		}
		return nil, fmt.Errorf("curl next to container %.12s exited with code %d: %s", containerID, exitCode, msg)
	}
	return parseCurlOutput(stdout, req)
}

// curlArgs returns the curl arguments for req: silent but with errors, the
// response headers included in the output, and the body passed literally.
func curlArgs(req *http.Request) ([]string, error) {
	args := []string{"--silent", "--show-error", "--include", "--max-time", curlMaxTime, "--request", req.Method}

	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range req.Header[k] {
			args = append(args, "--header", k+": "+v)
		}
	}

	if req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if len(body) > 0 {
			args = append(args, "--data-raw", string(body))
		}
	}
	return append(args, "--url", req.URL.String()), nil
}

// parseCurlOutput reads the output of curl --include: the response head as
// received, then the body as curl decoded it. Interim 1xx heads come first.
func parseCurlOutput(out []byte, req *http.Request) (*http.Response, error) {
	for {
		i := bytes.Index(out, []byte("\r\n\r\n"))
		if i < 0 {
			return nil, fmt.Errorf("curl printed no HTTP response: %q", truncate(out, 200))
		}
		head, rest := out[:i+4], out[i+4:]
		res, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(head)), req)
		if err != nil {
			return nil, fmt.Errorf("parse curl output: %w", err)
		}
		if res.StatusCode >= 100 && res.StatusCode < 200 {
			out = rest
			continue
		}
		// curl has already undone any chunked transfer encoding.
		res.TransferEncoding = nil
		res.Header.Del("Transfer-Encoding")
		res.ContentLength = int64(len(rest))
		res.Body = io.NopCloser(bytes.NewReader(rest))
		return res, nil
	}
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}
