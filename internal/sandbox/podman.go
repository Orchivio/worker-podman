package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const (
	pidsLimit   = "--pids-limit=512"
	memoryLimit = "--memory=4g"
	// removeTimeout bounds `podman rm -f`, which runs after the run context is done.
	removeTimeout = 30 * time.Second
	// waitDelay bounds how long Wait blocks on pipes after the CLI was killed.
	waitDelay = 5 * time.Second
)

// Podman runs sandboxes with rootless podman. It never passes
// --privileged, extra capabilities or a container-engine socket.
type Podman struct {
	Binary string
	// Command builds the podman process; nil means exec.CommandContext. Tests replace it.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// Args returns the `podman run` arguments for s.
func (p Podman) Args(s Spec) []string {
	args := []string{"run", "--rm", "-i", "--name", s.Name, "--network", s.Network,
		"--cap-drop=ALL", "--security-opt=no-new-privileges", pidsLimit, memoryLimit,
		// podman copies the host's *_proxy variables into the container by default.
		"--http-proxy=false"}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	for _, m := range s.Mounts {
		v := m.HostPath + ":" + m.ContainerPath
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
	}
	return append(args, s.Image)
}

// Run starts the container and waits for it. Killing the podman CLI does
// not stop the container, so timeout and cancellation remove it by name.
func (p Podman) Run(ctx context.Context, s Spec, stdout, stderr io.Writer) error {
	if s.Name == "" || s.Image == "" || s.Timeout <= 0 {
		return errors.New("sandbox: name, image and a positive timeout are required")
	}
	p.remove(ctx, s.Name) // leftover from a crashed worker
	runCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	cmd := p.command(runCtx, p.Args(s)...)
	cmd.Stdin = bytes.NewReader(s.Stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Remove the container first; the CLI then exits on its own.
	cmd.Cancel = func() error {
		p.remove(ctx, s.Name)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	if runCtx.Err() != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrTimeout
	}
	if err != nil {
		return fmt.Errorf("sandbox: podman run %s: %w", s.Name, err)
	}
	return nil
}

// RemoveOrphans removes all containers whose name starts with ContainerPrefix.
func (p Podman) RemoveOrphans(ctx context.Context) error {
	out, err := p.command(ctx, "ps", "-a", "--filter", "name=^"+ContainerPrefix, "--format", "{{.Names}}").Output()
	if err != nil {
		return fmt.Errorf("sandbox: list containers: %w", err)
	}
	for name := range strings.FieldsSeq(string(out)) {
		if strings.HasPrefix(name, ContainerPrefix) { // the filter is a regex; check again
			p.remove(ctx, name)
		}
	}
	return nil
}

func (p Podman) remove(ctx context.Context, name string) {
	rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	// Errors are expected when no such container exists; a real failure
	// surfaces on the next `run` as a name conflict.
	_ = p.command(rmCtx, "rm", "-f", "--time=0", name).Run()
}

func (p Podman) command(ctx context.Context, args ...string) *exec.Cmd {
	if p.Command != nil {
		return p.Command(ctx, p.Binary, args...)
	}
	return exec.CommandContext(ctx, p.Binary, args...)
}
