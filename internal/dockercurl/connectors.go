package dockercurl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// DefaultSidecarImage is the curl image the sidecar connector runs.
const DefaultSidecarImage = "curlimages/curl:8.10.1"

// SidecarLabel marks the containers the sidecar connector creates.
const SidecarLabel = "co.withlogic.terraform-provider-ceryx.sidecar"

// Sidecar runs curl in a throwaway container that joins the network
// namespace of the target container, like
//
//	docker run --rm --network container:<id> --entrypoint curl <image> <args>
//
// It works whatever the target image contains. The image is pulled on first
// use if the Docker host does not have it.
type Sidecar struct {
	API   API
	Image string

	pullMu sync.Mutex
	pulled bool
}

func (s *Sidecar) Curl(ctx context.Context, containerID string, args []string) ([]byte, []byte, int, error) {
	if err := s.ensureImage(ctx); err != nil {
		return nil, nil, 0, err
	}

	created, err := s.API.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        s.Image,
			Entrypoint:   []string{"curl"},
			Cmd:          args,
			AttachStdout: true,
			AttachStderr: true,
			Labels:       map[string]string{SidecarLabel: containerID},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode("container:" + containerID),
		},
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("create curl sidecar for container %.12s: %w", containerID, err)
	}
	defer func() {
		_, _ = s.API.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	attached, err := s.API.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("attach to curl sidecar: %w", err)
	}
	defer attached.Close()

	// Wait for the next exit before starting, so the exit is not missed.
	wait := s.API.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	if _, err := s.API.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, nil, 0, fmt.Errorf("start curl sidecar for container %.12s: %w", containerID, err)
	}

	stdout, stderr, err := readOutput(ctx, attached.HijackedResponse)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read curl sidecar output: %w", err)
	}

	select {
	case res := <-wait.Result:
		if res.Error != nil && res.Error.Message != "" {
			return nil, nil, 0, fmt.Errorf("curl sidecar: %s", res.Error.Message)
		}
		return stdout, stderr, int(res.StatusCode), nil
	case err := <-wait.Error:
		return nil, nil, 0, fmt.Errorf("wait for curl sidecar: %w", err)
	}
}

func (s *Sidecar) ensureImage(ctx context.Context) error {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	if s.pulled {
		return nil
	}

	_, err := s.API.ImageInspect(ctx, s.Image)
	if cerrdefs.IsNotFound(err) {
		pull, pullErr := s.API.ImagePull(ctx, s.Image, client.ImagePullOptions{})
		if pullErr == nil {
			pullErr = pull.Wait(ctx)
			pull.Close()
		}
		if pullErr != nil {
			return fmt.Errorf("pull curl sidecar image %s: %w", s.Image, pullErr)
		}
	} else if err != nil {
		return fmt.Errorf("inspect curl sidecar image %s: %w", s.Image, err)
	}
	s.pulled = true
	return nil
}

// Exec runs curl inside the target container with docker exec, like
//
//	docker exec <id> curl <args>
//
// The target image must include curl.
type Exec struct {
	API API
}

func (e *Exec) Curl(ctx context.Context, containerID string, args []string) ([]byte, []byte, int, error) {
	created, err := e.API.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          append([]string{"curl"}, args...),
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("docker exec curl in container %.12s: %w", containerID, err)
	}

	attached, err := e.API.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("docker exec curl in container %.12s: %w", containerID, err)
	}
	defer attached.Close()

	stdout, stderr, err := readOutput(ctx, attached.HijackedResponse)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read docker exec output: %w", err)
	}

	// The exec can still show as running for a moment after its output ends.
	for i := 0; ; i++ {
		inspected, err := e.API.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return nil, nil, 0, fmt.Errorf("inspect docker exec: %w", err)
		}
		if !inspected.Running {
			if inspected.ExitCode == 126 || inspected.ExitCode == 127 {
				stderr = append(stderr, []byte(" (is curl installed in the container? The sidecar connector does not need it.)")...)
			}
			return stdout, stderr, inspected.ExitCode, nil
		}
		if i == 50 {
			return nil, nil, 0, fmt.Errorf("docker exec curl in container %.12s did not exit", containerID)
		}
		select {
		case <-ctx.Done():
			return nil, nil, 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// readOutput demultiplexes an attached stream until it ends or ctx is done.
func readOutput(ctx context.Context, h client.HijackedResponse) ([]byte, []byte, error) {
	stop := context.AfterFunc(ctx, h.Close)
	defer stop()

	var stdout, stderr bytes.Buffer
	_, err := stdcopy.StdCopy(&stdout, &stderr, io.LimitReader(h.Reader, 64<<20))
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if err != nil {
		return nil, nil, err
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}
