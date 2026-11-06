package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/worker/v1"
	"github.com/soemii/orchivio-worker/internal/sandbox"
)

const (
	testRunID  = "0f8e1a2b-1111-4222-8333-444455556666"
	testBranch = "orchivio/orc-1-0f8e1a2b"
	testSlug   = "acme/app"
)

// fakeToken matches the redact patterns.
var fakeToken = "ghs_" + strings.Repeat("b", 36)

var testNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// fakeServer records what the runner sends.
type fakeServer struct {
	workerv1.UnimplementedWorkerServiceServer
	mu         sync.Mutex
	creds      *pluginv1.RepoCredentials
	credWrites []bool
	events     []*pluginv1.AgentEvent
	completes  []*workerv1.CompleteRunRequest
	registers  []*workerv1.RegisterRequest
	leases     []*workerv1.RunAssignment // handed out in order, then nothing
	completed  chan struct{}
}

func (f *fakeServer) Register(_ context.Context, req *workerv1.RegisterRequest) (*workerv1.RegisterResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers = append(f.registers, req)
	return &workerv1.RegisterResponse{}, nil
}

func (f *fakeServer) LeaseRun(context.Context, *workerv1.LeaseRunRequest) (*workerv1.LeaseRunResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.leases) == 0 {
		return &workerv1.LeaseRunResponse{}, nil
	}
	a := f.leases[0]
	f.leases = f.leases[1:]
	return &workerv1.LeaseRunResponse{Run: a}, nil
}

func (f *fakeServer) ReportEvents(stream workerv1.WorkerService_ReportEventsServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&workerv1.ReportEventsResponse{})
		}
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.events = append(f.events, req.GetEvent())
		f.mu.Unlock()
	}
}

func (f *fakeServer) GetRepoCredentials(_ context.Context, req *workerv1.GetRepoCredentialsRequest) (*pluginv1.RepoCredentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credWrites = append(f.credWrites, req.GetWrite())
	return f.creds, nil
}

func (f *fakeServer) CompleteRun(_ context.Context, req *workerv1.CompleteRunRequest) (*workerv1.CompleteRunResponse, error) {
	f.mu.Lock()
	f.completes = append(f.completes, req)
	f.mu.Unlock()
	if f.completed != nil {
		f.completed <- struct{}{}
	}
	return &workerv1.CompleteRunResponse{}, nil
}

func (f *fakeServer) complete(t *testing.T) *workerv1.CompleteRunRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.completes) != 1 {
		t.Fatalf("CompleteRun calls = %d, want 1", len(f.completes))
	}
	return f.completes[0]
}

func dial(t *testing.T, srv *fakeServer) workerv1.WorkerServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return workerv1.NewWorkerServiceClient(conn)
}

// fakeSandbox records the spec and runs fn in place of a container.
type fakeSandbox struct {
	spec    sandbox.Spec
	fn      func(ctx context.Context, s sandbox.Spec, stdout, stderr io.Writer) error
	orphans int
}

func (f *fakeSandbox) RemoveOrphans(context.Context) error {
	f.orphans++
	return nil
}

func (f *fakeSandbox) Run(ctx context.Context, s sandbox.Spec, stdout, stderr io.Writer) error {
	f.spec = s
	return f.fn(ctx, s, stdout, stderr)
}

func emit(t *testing.T, w io.Writer, events ...*pluginv1.AgentEvent) {
	t.Helper()
	for _, e := range events {
		b, err := protojson.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "%s\n", b)
	}
}

func finished(text string) *pluginv1.AgentEvent {
	return &pluginv1.AgentEvent{Type: "run.finished", Status: "success", Text: text}
}

func started() *pluginv1.AgentEvent {
	return &pluginv1.AgentEvent{Type: "run.started", SessionId: "sess-1"}
}

// testGit runs git for test setup, isolated from the developer's config.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "protocol.file.allow=always", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newOrigin creates a bare remote with main (README.md, src/main.go, .orchivio.yaml).
func newOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	testGit(t, root, "init", "--bare", origin)
	testGit(t, root, "init", seed)
	writeFile(t, seed, "README.md", "hello\n")
	writeFile(t, seed, "src/main.go", "package main\n")
	writeFile(t, seed, ".orchivio.yaml", "instructions: use tabs\nforbiddenPaths: [src/secret/]\n")
	testGit(t, seed, "add", "-A")
	testGit(t, seed, "commit", "-m", "init")
	testGit(t, seed, "push", origin, "HEAD:refs/heads/main")
	return origin
}

func originHas(origin, ref string) bool {
	return exec.Command("git", "--git-dir", origin, "rev-parse", "--verify", "-q", ref).Run() == nil
}

type env struct {
	srv    *fakeServer
	sb     *fakeSandbox
	runner *Runner
	work   string
	origin string
}

func newEnv(t *testing.T, fn func(ctx context.Context, s sandbox.Spec, stdout, stderr io.Writer) error) *env {
	t.Helper()
	origin := newOrigin(t)
	srv := &fakeServer{creds: &pluginv1.RepoCredentials{
		Username:  "x-access-token",
		Token:     fakeToken,
		ExpiresAt: timestamppb.New(testNow.Add(time.Hour)),
		CloneUrls: map[string]string{testSlug: "file://" + origin},
	}}
	sb := &fakeSandbox{fn: fn}
	work := filepath.Join(t.TempDir(), "work")
	cfg := Config{WorkerID: "w1", WorkDir: work, Image: "img:dev", Network: "orchivio-sandbox",
		ProxyURL: "http://orchivio-proxy:8090", PollInterval: 10 * time.Millisecond}
	return &env{srv: srv, sb: sb, runner: New(cfg, dial(t, srv), sb, func() time.Time { return testNow }), work: work, origin: origin}
}

func assignment(kind string) *workerv1.RunAssignment {
	a := &workerv1.RunAssignment{
		RunId: testRunID, Kind: kind,
		Repos:        []*workerv1.Repo{{Slug: testSlug, BaseBranch: "main", AllowedPaths: []string{"src/"}}},
		Spec:         &pluginv1.RunSpec{RunId: testRunID, Prompt: "do it", Instructions: "profile"},
		MaxDiffLines: 100,
		MaxDuration:  durationpb.New(time.Minute),
		TicketRef:    "ORC-1", TicketTitle: "Fix the thing",
	}
	if kind == "task" {
		a.Branch = testBranch
	}
	return a
}

func execute(t *testing.T, e *env, a *workerv1.RunAssignment) *workerv1.CompleteRunRequest {
	t.Helper()
	if err := e.runner.Execute(context.Background(), a); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return e.srv.complete(t)
}

func TestExecuteQuestionCompletesAnswered(t *testing.T) {
	e := newEnv(t, func(_ context.Context, _ sandbox.Spec, stdout, _ io.Writer) error {
		emit(t, stdout, started(), &pluginv1.AgentEvent{Type: "message", Text: "thinking"})
		fmt.Fprintln(stdout, "not json")
		emit(t, stdout, finished("42"))
		return nil
	})
	got := execute(t, e, assignment("question"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_ANSWERED || got.GetText() != "42" || got.GetSessionId() != "sess-1" {
		t.Fatalf("complete = %v", got)
	}
	if len(e.srv.events) != 3 {
		t.Fatalf("events = %d, want 3", len(e.srv.events))
	}
	if !slices.Equal(e.srv.credWrites, []bool{false}) {
		t.Fatalf("credential writes = %v, want [false]", e.srv.credWrites)
	}
	m := e.sb.spec.Mounts
	if len(m) != 1 || m[0].ContainerPath != "/work/app" || !m[0].ReadOnly {
		t.Fatalf("mounts = %+v", m)
	}
	var spec pluginv1.RunSpec
	if err := protojson.Unmarshal(e.sb.spec.Stdin, &spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.GetWorkdirs(), []string{"/work/app"}) || !strings.Contains(spec.GetInstructions(), "use tabs") {
		t.Fatalf("stdin spec = %v", &spec)
	}
	if e.sb.spec.Name != "orchivio-run-"+testRunID || e.sb.spec.Timeout != time.Minute {
		t.Fatalf("name/timeout = %q %v", e.sb.spec.Name, e.sb.spec.Timeout)
	}
}

func TestExecuteQuestionAskedCompletesWaiting(t *testing.T) {
	e := newEnv(t, func(_ context.Context, _ sandbox.Spec, stdout, _ io.Writer) error {
		emit(t, stdout, started(), &pluginv1.AgentEvent{Type: "question.asked", Text: "Which repo?"}, finished("asked"))
		return nil
	})
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_WAITING || got.GetText() != "Which repo?" || got.GetSessionId() != "sess-1" {
		t.Fatalf("complete = %v", got)
	}
	if originHas(e.origin, "refs/heads/"+testBranch) {
		t.Fatal("waiting run must not push")
	}
}

func changeFile(name, content string) func(context.Context, sandbox.Spec, io.Writer, io.Writer) error {
	return func(_ context.Context, s sandbox.Spec, stdout, _ io.Writer) error {
		if err := os.WriteFile(filepath.Join(s.Mounts[0].HostPath, name), []byte(content), 0o644); err != nil {
			return err
		}
		b, _ := protojson.Marshal(finished("done"))
		fmt.Fprintf(stdout, "%s\n", b)
		return nil
	}
}

func TestExecuteTaskPushesAndCompletesPushed(t *testing.T) {
	e := newEnv(t, changeFile("src/new.go", "package main\n"))
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_PUSHED || got.GetText() != "done" {
		t.Fatalf("complete = %v", got)
	}
	if got.GetPushedBranches()[testSlug] != testBranch {
		t.Fatalf("pushed branches = %v", got.GetPushedBranches())
	}
	if msg := testGit(t, e.origin, "log", "-1", "--format=%s", testBranch); msg != "ORC-1: Fix the thing" {
		t.Fatalf("commit message = %q", msg)
	}
	if !slices.Equal(e.srv.credWrites, []bool{true}) {
		t.Fatalf("credential writes = %v, want [true]", e.srv.credWrites)
	}
	if e.sb.spec.Mounts[0].ReadOnly {
		t.Fatal("task mount must be writable")
	}
}

func TestExecuteTaskWithoutChangesCompletesAnswered(t *testing.T) {
	e := newEnv(t, func(_ context.Context, _ sandbox.Spec, stdout, _ io.Writer) error {
		emit(t, stdout, finished("nothing to do"))
		return nil
	})
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_ANSWERED || got.GetText() != "nothing to do" {
		t.Fatalf("complete = %v", got)
	}
}

func TestExecuteTaskOutsideAllowedPathsFails(t *testing.T) {
	e := newEnv(t, changeFile("README.md", "changed\n"))
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_FAILED || !strings.Contains(got.GetText(), "outside allowed paths") {
		t.Fatalf("complete = %v", got)
	}
	if originHas(e.origin, "refs/heads/"+testBranch) {
		t.Fatal("rejected run must not push")
	}
}

func TestExecuteTaskForbiddenPathFromRepoFileFails(t *testing.T) {
	e := newEnv(t, func(ctx context.Context, s sandbox.Spec, stdout, stderr io.Writer) error {
		os.MkdirAll(filepath.Join(s.Mounts[0].HostPath, "src/secret"), 0o755)
		return changeFile("src/secret/x.go", "package secret\n")(ctx, s, stdout, stderr)
	})
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_FAILED || !strings.Contains(got.GetText(), "outside allowed paths") {
		t.Fatalf("complete = %v", got)
	}
}

func TestExecuteRefreshesExpiringCredentialsBeforePush(t *testing.T) {
	e := newEnv(t, changeFile("src/new.go", "package main\n"))
	e.srv.creds.ExpiresAt = timestamppb.New(testNow.Add(30 * time.Second))
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_PUSHED {
		t.Fatalf("complete = %v", got)
	}
	if !slices.Equal(e.srv.credWrites, []bool{true, true}) {
		t.Fatalf("credential writes = %v, want [true true]", e.srv.credWrites)
	}
}

func TestExecuteFollowUpChecksOutExistingBranch(t *testing.T) {
	var sawFollowUp bool
	e := newEnv(t, func(ctx context.Context, s sandbox.Spec, stdout, stderr io.Writer) error {
		_, err := os.Stat(filepath.Join(s.Mounts[0].HostPath, "src/first.go"))
		sawFollowUp = err == nil
		return changeFile("src/second.go", "package main\n")(ctx, s, stdout, stderr)
	})
	seed := filepath.Join(t.TempDir(), "seed")
	testGit(t, filepath.Dir(seed), "clone", e.origin, seed)
	writeFile(t, seed, "src/first.go", "package main\n")
	testGit(t, seed, "add", "-A")
	testGit(t, seed, "commit", "-m", "first run")
	testGit(t, seed, "push", "origin", "HEAD:refs/heads/"+testBranch)

	got := execute(t, e, assignment("task"))
	if !sawFollowUp {
		t.Fatal("agent copy was not checked out at the existing branch")
	}
	if got.GetOutcome() != workerv1.CompleteRunRequest_PUSHED {
		t.Fatalf("complete = %v", got)
	}
	if parent := testGit(t, e.origin, "log", "-1", "--format=%s", testBranch+"~1"); parent != "first run" {
		t.Fatalf("parent = %q, want first run", parent)
	}
}

func TestExecuteSandboxEnvHasNoRealSecrets(t *testing.T) {
	var agentConfig, pristineConfig []byte
	e := newEnv(t, func(ctx context.Context, s sandbox.Spec, stdout, stderr io.Writer) error {
		agentConfig, _ = os.ReadFile(filepath.Join(s.Mounts[0].HostPath, ".git", "config"))
		pristineConfig, _ = os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(s.Mounts[0].HostPath)), "pristine", "app.git", "config"))
		return changeFile("src/new.go", "package main\n")(ctx, s, stdout, stderr)
	})
	execute(t, e, assignment("task"))
	for k, v := range e.sb.spec.Env {
		if strings.Contains(v, fakeToken) {
			t.Fatalf("env %s holds the token", k)
		}
	}
	if e.sb.spec.Env["ANTHROPIC_API_KEY"] != "orchivio-proxy-placeholder" || e.sb.spec.Env["ANTHROPIC_BASE_URL"] != "http://orchivio-proxy:8090" {
		t.Fatalf("env = %v", e.sb.spec.Env)
	}
	if strings.Contains(string(e.sb.spec.Stdin), fakeToken) || strings.Contains(string(agentConfig), "[remote") || len(agentConfig) == 0 {
		t.Fatalf("token in stdin or remote in agent .git/config:\n%s", agentConfig)
	}
	if len(pristineConfig) == 0 || strings.Contains(string(pristineConfig), "[remote") {
		t.Fatalf("pristine clone still has a remote:\n%s", pristineConfig)
	}
}

func TestExecuteTimeoutFails(t *testing.T) {
	e := newEnv(t, func(ctx context.Context, _ sandbox.Spec, stdout, _ io.Writer) error {
		emit(t, stdout, started())
		<-ctx.Done()
		return ctx.Err()
	})
	a := assignment("task")
	a.MaxDuration = durationpb.New(100 * time.Millisecond)
	got := execute(t, e, a)
	if got.GetOutcome() != workerv1.CompleteRunRequest_FAILED || got.GetText() != "run exceeded max duration" {
		t.Fatalf("complete = %v", got)
	}
}

func TestExecuteAgentErrorFails(t *testing.T) {
	e := newEnv(t, func(_ context.Context, _ sandbox.Spec, stdout, _ io.Writer) error {
		emit(t, stdout, &pluginv1.AgentEvent{Type: "run.finished", Status: "error", Text: "max turns"})
		return nil
	})
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_FAILED || got.GetText() != "agent failed: max turns" {
		t.Fatalf("complete = %v", got)
	}
}

func TestExecuteCleansUpWorkDirs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fn      func(context.Context, sandbox.Spec, io.Writer, io.Writer) error
		outcome workerv1.CompleteRunRequest_Outcome
		text    string
	}{
		{"success", changeFile("src/new.go", "package main\n"), workerv1.CompleteRunRequest_PUSHED, "done"},
		{"adapter failure", func(_ context.Context, _ sandbox.Spec, _, stderr io.Writer) error {
			fmt.Fprintln(stderr, "leaked "+fakeToken)
			return errors.New("exit status 1")
		}, workerv1.CompleteRunRequest_FAILED, "runtime adapter failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.fn)
			got := execute(t, e, assignment("task"))
			if got.GetOutcome() != tc.outcome || got.GetText() != tc.text {
				t.Fatalf("complete = %v", got)
			}
			dir := filepath.Join(e.work, testRunID)
			for _, gone := range []string{"pristine", "repos"} {
				if _, err := os.Stat(filepath.Join(dir, gone)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s still exists: %v", gone, err)
				}
			}
			info, err := os.Stat(filepath.Join(dir, "raw.log"))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("raw.log: %v %v", info, err)
			}
			raw, _ := os.ReadFile(filepath.Join(dir, "raw.log"))
			if strings.Contains(string(raw), fakeToken) {
				t.Fatalf("raw.log holds the token: %s", raw)
			}
		})
	}
}

func TestExecutePreparationFailureCompletesFailed(t *testing.T) {
	e := newEnv(t, changeFile("src/new.go", "x"))
	e.srv.creds.CloneUrls[testSlug] = "file:///does/not/exist.git"
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_FAILED || got.GetText() != "repository preparation failed" {
		t.Fatalf("complete = %v", got)
	}
	if e.sb.spec.Name != "" {
		t.Fatal("sandbox must not start")
	}
	if _, err := os.Stat(filepath.Join(e.work, testRunID, "pristine")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pristine still exists: %v", err)
	}
}

func TestExecuteRejectsInvalidRunID(t *testing.T) {
	e := newEnv(t, changeFile("src/new.go", "x"))
	a := assignment("task")
	a.RunId = "../escape"
	if err := e.runner.Execute(context.Background(), a); err == nil {
		t.Fatal("want error for path-like run id")
	}
	if got := e.srv.complete(t); got.GetOutcome() != workerv1.CompleteRunRequest_FAILED || got.GetText() != "invalid run assignment" {
		t.Fatalf("complete = %v", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.work), "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("run dir created outside the work dir")
	}
}

func TestExecuteRunDirFailureCompletesFailed(t *testing.T) {
	e := newEnv(t, changeFile("src/new.go", "x"))
	e.runner.cfg.WorkDir = filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(e.runner.cfg.WorkDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.runner.Execute(context.Background(), assignment("task")); err == nil {
		t.Fatal("want error when the run dir cannot be created")
	}
	got := e.srv.complete(t)
	if got.GetOutcome() != workerv1.CompleteRunRequest_FAILED || got.GetText() != "worker could not prepare run" {
		t.Fatalf("complete = %v", got)
	}
	if e.sb.spec.Name != "" || len(e.srv.credWrites) != 0 {
		t.Fatal("run must stop before credentials and sandbox")
	}
}

func TestSnippetRedactsBeforeTruncating(t *testing.T) {
	got := snippet([]byte(strings.Repeat("x", 190) + fakeToken))
	if strings.Contains(got, "ghs_") || !strings.HasSuffix(got, "****") {
		t.Fatalf("snippet = %q", got)
	}
}

func TestCloneURLInsertsTokenOnlyForHTTPS(t *testing.T) {
	creds := &pluginv1.RepoCredentials{Username: "x-access-token", Token: "tok"}
	got, err := cloneURL("https://github.com/acme/app.git", creds)
	if err != nil || got != "https://x-access-token:tok@github.com/acme/app.git" {
		t.Fatalf("https = %q %v", got, err)
	}
	if got, _ := cloneURL("file:///tmp/o.git", creds); got != "file:///tmp/o.git" {
		t.Fatalf("file = %q", got)
	}
	if _, err := cloneURL("ext::sh -c x", creds); err == nil {
		t.Fatal("want error for ext transport")
	}
}

func TestCommitMessageTruncatesTo72(t *testing.T) {
	a := &workerv1.RunAssignment{TicketRef: "ORC-1", TicketTitle: strings.Repeat("x", 100) + "\nsecond"}
	if got := commitMessage(a); len(got) != 72 || !strings.HasPrefix(got, "ORC-1: xxx") {
		t.Fatalf("message = %q (%d)", got, len(got))
	}
}

func TestLoopRegistersAndExecutesLeasedRun(t *testing.T) {
	e := newEnv(t, func(_ context.Context, _ sandbox.Spec, stdout, _ io.Writer) error {
		emit(t, stdout, finished("42"))
		return nil
	})
	e.srv.leases = []*workerv1.RunAssignment{assignment("question")}
	e.srv.completed = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.runner.Loop(ctx) }()
	select {
	case <-e.srv.completed:
	case <-time.After(10 * time.Second):
		t.Fatal("run was not completed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Loop: %v", err)
	}
	r := e.srv.registers[0]
	if r.GetWorkerId() != "w1" || r.GetContractVersion() != "worker.v1" || r.GetCapacity() != 1 {
		t.Fatalf("register = %v", r)
	}
	if e.sb.orphans != 1 {
		t.Fatalf("RemoveOrphans calls = %d, want 1", e.sb.orphans)
	}
}
