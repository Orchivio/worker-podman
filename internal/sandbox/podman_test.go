package sandbox

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPodmanArgsQuestionMountsReadOnly(t *testing.T) {
	s := Spec{
		Name: "orchivio-run-r1", Image: "img:dev", Network: "orchivio-sandbox",
		Mounts: []Mount{{HostPath: "/h/repos/x", ContainerPath: "/work/x", ReadOnly: true}},
		Env:    map[string]string{"B": "2", "A": "1"},
	}
	got := Podman{Binary: "podman"}.Args(s)
	want := []string{"run", "--rm", "-i", "--name", "orchivio-run-r1", "--network", "orchivio-sandbox",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=512", "--memory=4g",
		"-e", "A=1", "-e", "B=2", "-v", "/h/repos/x:/work/x:ro", "img:dev"}
	if !slices.Equal(got, want) {
		t.Fatalf("args\n got %q\nwant %q", got, want)
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--privileged") || strings.Contains(joined, "podman.sock") {
		t.Fatalf("forbidden flag in %q", joined)
	}
}

func TestPodmanArgsTaskMountsWritable(t *testing.T) {
	s := Spec{Name: "n", Image: "i", Network: "net", Mounts: []Mount{{HostPath: "/h/x", ContainerPath: "/work/x"}}}
	got := Podman{}.Args(s)
	if !slices.Contains(got, "/h/x:/work/x") {
		t.Fatalf("want writable mount in %q", got)
	}
}

func TestPodmanRunRejectsMissingName(t *testing.T) {
	err := Podman{Binary: "podman"}.Run(context.Background(), Spec{Image: "i"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("want error for empty name")
	}
}

// recorder fakes the podman binary: "run" sleeps, everything else succeeds.
type recorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recorder) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.mu.Unlock()
	if args[0] == "run" {
		return exec.CommandContext(ctx, "sleep", "10")
	}
	return exec.CommandContext(ctx, "true")
}

func TestExecuteTimeoutRemovesContainer(t *testing.T) {
	rec := &recorder{}
	p := Podman{Binary: "podman", Command: rec.command}
	start := time.Now()
	err := p.Run(context.Background(), Spec{Name: "orchivio-run-r1", Image: "i", Timeout: 100 * time.Millisecond}, io.Discard, io.Discard)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("run was not killed on timeout")
	}
	rm := []string{"rm", "-f", "--time=0", "orchivio-run-r1"}
	if len(rec.calls) != 3 || !slices.Equal(rec.calls[0], rm) || rec.calls[1][0] != "run" || !slices.Equal(rec.calls[2], rm) {
		t.Fatalf("calls = %q, want rm, run, rm", rec.calls)
	}
}

func TestPodmanCancelRemovesContainer(t *testing.T) {
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := Podman{Binary: "podman", Command: rec.command}.Run(ctx, Spec{Name: "orchivio-run-r2", Image: "i", Timeout: time.Minute}, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if last := rec.calls[len(rec.calls)-1]; !slices.Equal(last, []string{"rm", "-f", "--time=0", "orchivio-run-r2"}) {
		t.Fatalf("last call = %q, want rm -f", last)
	}
}
