// Package runner leases runs from the server, prepares the repos on the
// host, runs the untrusted agent in a fresh sandbox, streams its events
// back and, for tasks, pushes through the guarded push step.
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/worker/v1"
	"github.com/orchivio/orchestrator/contract/redact"
	"github.com/orchivio/worker-podman/internal/push"
	"github.com/orchivio/worker-podman/internal/sandbox"
)

const (
	contractVersion = "worker.v1"
	capacity        = 1

	kindQuestion = "question"
	kindTask     = "task"

	eventStarted  = "run.started"
	eventQuestion = "question.asked"
	eventFinished = "run.finished"
	statusSuccess = "success"

	workdirPrefix      = "/work/"
	apiKeyPlaceholder  = "orchivio-proxy-placeholder"
	proxyHost          = "orchivio-proxy"
	defaultMaxDuration = 30 * time.Minute
	credentialMargin   = time.Minute
	completeTimeout    = 30 * time.Second
	maxLineBytes       = 10 << 20
	logSnippetBytes    = 200
	maxSubjectRunes    = 72

	msgTimeout     = "run exceeded max duration"
	msgAdapter     = "runtime adapter failed"
	msgStopped     = "worker stopped"
	msgCredentials = "repository credentials unavailable"
	msgPrepare     = "repository preparation failed"
	msgInternal    = "internal worker error"
	msgBadRun      = "invalid run assignment"
	msgRunDir      = "worker could not prepare run"
)

// runIDPattern keeps the run id safe as a path element and container name.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Config holds the worker settings.
type Config struct {
	WorkerID, WorkDir, Image, Network, ProxyURL string
	PollInterval                                time.Duration
}

// Runner executes one leased run at a time.
type Runner struct {
	cfg Config
	srv workerv1.WorkerServiceClient
	sb  sandbox.Provider
	now func() time.Time
}

// New returns a runner; now is injected for credential expiry checks.
func New(cfg Config, srv workerv1.WorkerServiceClient, sb sandbox.Provider, now func() time.Time) *Runner {
	return &Runner{cfg: cfg, srv: srv, sb: sb, now: now}
}

// Loop registers the worker and then leases and executes runs until ctx
// is cancelled. gRPC errors are logged and retried after PollInterval.
func (r *Runner) Loop(ctx context.Context) error {
	if err := r.sb.RemoveOrphans(ctx); err != nil {
		slog.Warn("remove orphan sandboxes", "error", err)
	}
	for {
		_, err := r.srv.Register(ctx, &workerv1.RegisterRequest{WorkerId: r.cfg.WorkerID, ContractVersion: contractVersion, Capacity: capacity})
		if err == nil {
			break
		}
		slog.Warn("register failed", "error", err)
		if !r.wait(ctx) {
			return nil
		}
	}
	slog.Info("worker registered", "worker", r.cfg.WorkerID)
	for {
		resp, err := r.srv.LeaseRun(ctx, &workerv1.LeaseRunRequest{WorkerId: r.cfg.WorkerID})
		if err != nil && ctx.Err() == nil {
			slog.Warn("lease failed", "error", err)
		}
		if a := resp.GetRun(); err == nil && a != nil {
			if err := r.Execute(ctx, a); err != nil {
				slog.Error("run failed", "run", a.GetRunId(), "error", err)
			}
			continue
		}
		if !r.wait(ctx) {
			return nil
		}
	}
}

func (r *Runner) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(r.cfg.PollInterval):
		return true
	}
}

// Execute runs one assignment and always reports it with CompleteRun.
// Clones are deleted afterwards; <work>/<run>/raw.log is kept.
func (r *Runner) Execute(ctx context.Context, a *workerv1.RunAssignment) error {
	// Report even unusable assignments, or the run stays leased and comes back.
	if !runIDPattern.MatchString(a.GetRunId()) {
		return errors.Join(fmt.Errorf("runner: invalid run id %q", a.GetRunId()), r.complete(ctx, a, failed(msgBadRun)))
	}
	dir := filepath.Join(r.cfg.WorkDir, a.GetRunId())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.Join(fmt.Errorf("runner: create run dir: %w", err), r.complete(ctx, a, failed(msgRunDir)))
	}
	defer cleanup(dir)
	return r.complete(ctx, a, r.run(ctx, a, dir))
}

func (r *Runner) complete(ctx context.Context, a *workerv1.RunAssignment, res *workerv1.CompleteRunRequest) error {
	res.RunId, res.WorkerId = a.GetRunId(), r.cfg.WorkerID
	// The run context may be cancelled (shutdown); still report the outcome.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	if _, err := r.srv.CompleteRun(cctx, res); err != nil {
		return fmt.Errorf("runner: complete run %s: %w", a.GetRunId(), err)
	}
	slog.Info("run completed", "run", a.GetRunId(), "outcome", res.GetOutcome().String())
	return nil
}

func cleanup(dir string) {
	for _, sub := range []string{"pristine", "repos", "home"} {
		if err := forceRemove(filepath.Join(dir, sub)); err != nil {
			slog.Error("cleanup failed", "dir", filepath.Join(dir, sub), "error", err)
		}
	}
}

func (r *Runner) run(ctx context.Context, a *workerv1.RunAssignment, dir string) *workerv1.CompleteRunRequest {
	if a.GetKind() != kindQuestion && a.GetKind() != kindTask {
		return failed(fmt.Sprintf("unsupported run kind %q", a.GetKind()))
	}
	creds, err := r.credentials(ctx, a)
	if err != nil {
		slog.Error("get repo credentials", "run", a.GetRunId(), "error", err)
		return failed(msgCredentials)
	}
	repos, err := prepare(ctx, a, dir, creds)
	if err != nil {
		slog.Error("prepare repos", "run", a.GetRunId(), "error", err)
		if errors.Is(err, errRepoFile) {
			return failed(err.Error())
		}
		return failed(msgPrepare)
	}
	ev, sbErr := r.sandbox(ctx, a, dir, repos)
	if ev == nil {
		slog.Error("sandbox setup", "run", a.GetRunId(), "error", sbErr)
		return failed(msgInternal)
	}
	return r.outcome(ctx, a, repos, creds, ev, sbErr)
}

func (r *Runner) credentials(ctx context.Context, a *workerv1.RunAssignment) (*pluginv1.RepoCredentials, error) {
	return r.srv.GetRepoCredentials(ctx, &workerv1.GetRepoCredentialsRequest{
		RunId: a.GetRunId(), WorkerId: r.cfg.WorkerID, Write: a.GetKind() == kindTask})
}

func (r *Runner) outcome(ctx context.Context, a *workerv1.RunAssignment, repos []preparedRepo, creds *pluginv1.RepoCredentials, ev *observed, sbErr error) *workerv1.CompleteRunRequest {
	res := func() *workerv1.CompleteRunRequest {
		switch {
		case ctx.Err() != nil:
			return failed(msgStopped)
		case errors.Is(sbErr, errTimeout):
			return failed(msgTimeout)
		case ev.question != "":
			return &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_WAITING, Text: ev.question}
		case ev.finished == nil:
			slog.Error("runtime adapter ended without run.finished", "run", a.GetRunId(), "error", sbErr)
			return failed(msgAdapter)
		case ev.finished.GetStatus() != statusSuccess:
			return failed(agentFailure(ev.finished.GetText()))
		case a.GetKind() == kindQuestion:
			return answered(ev.finished.GetText())
		}
		return r.push(ctx, a, repos, creds, ev.finished.GetText())
	}()
	res.SessionId = ev.sessionID
	return res
}

func agentFailure(text string) string {
	if text == "" {
		return "agent failed"
	}
	return "agent failed: " + text
}

// push runs the guarded push step for every repo. At least one push →
// PUSHED; no changes anywhere → ANSWERED; any violation → FAILED.
func (r *Runner) push(ctx context.Context, a *workerv1.RunAssignment, repos []preparedRepo, creds *pluginv1.RepoCredentials, text string) *workerv1.CompleteRunRequest {
	if exp := creds.GetExpiresAt(); exp != nil && exp.AsTime().Before(r.now().Add(credentialMargin)) {
		fresh, err := r.credentials(ctx, a)
		if err != nil {
			slog.Error("refresh repo credentials", "run", a.GetRunId(), "error", err)
			return failed(msgCredentials)
		}
		creds = fresh
	}
	pushed := map[string]string{}
	for _, repo := range repos {
		remote, err := cloneURL(creds.GetCloneUrls()[repo.slug], creds)
		if err != nil {
			slog.Error("push remote", "run", a.GetRunId(), "repo", repo.slug, "error", err)
			return failed(repo.slug + ": push failed")
		}
		res, err := push.Push(ctx, push.Request{
			PristineGitDir: repo.pristine, AgentDir: repo.agent,
			BaseBranch: repo.base, Branch: a.GetBranch(),
			AllowedPaths: repo.allowedPaths, ForbiddenPaths: repo.file.ForbiddenPaths,
			MaxDiffLines: int(a.GetMaxDiffLines()), CommitMessage: commitMessage(a), RemoteURL: remote,
		})
		if err != nil {
			return failed(pushFailure(a.GetRunId(), repo.slug, creds.GetToken(), err))
		}
		if res.Pushed {
			pushed[repo.slug] = a.GetBranch()
		}
	}
	if len(pushed) == 0 {
		return answered(text)
	}
	return &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_PUSHED, Text: text, PushedBranches: pushed}
}

// pushFailure shows boundary violations (repo-relative, no host paths)
// and hides everything else behind a generic message.
func pushFailure(runID, slug, token string, err error) string {
	for _, known := range []error{push.ErrPathNotAllowed, push.ErrDiffTooLarge, push.ErrSecretInDiff, push.ErrBadBranch} {
		if errors.Is(err, known) {
			return slug + ": " + err.Error()
		}
	}
	msg := err.Error()
	if token != "" {
		msg = strings.ReplaceAll(msg, token, tokenMask)
	}
	slog.Error("push", "run", runID, "repo", slug, "error", redact.String(msg))
	return slug + ": push failed"
}

// commitMessage is "<ticket_ref>: <ticket_title>", first line, ≤ 72 runes.
func commitMessage(a *workerv1.RunAssignment) string {
	title, _, _ := strings.Cut(a.GetTicketTitle(), "\n")
	msg := strings.TrimSpace(a.GetTicketRef() + ": " + strings.TrimSpace(title))
	msg = strings.TrimSuffix(msg, ":")
	if msg == "" {
		msg = "Orchivio change"
	}
	if runes := []rune(msg); len(runes) > maxSubjectRunes {
		msg = string(runes[:maxSubjectRunes])
	}
	return msg
}

func failed(text string) *workerv1.CompleteRunRequest {
	return &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_FAILED, Text: text}
}

func answered(text string) *workerv1.CompleteRunRequest {
	return &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_ANSWERED, Text: text}
}

// errTimeout marks a run that hit max_duration, whichever side noticed first.
var errTimeout = errors.New("runner: run exceeded max duration")

// sandbox runs the agent and returns what it reported. A nil *observed
// means the sandbox could not be set up at all.
func (r *Runner) sandbox(ctx context.Context, a *workerv1.RunAssignment, dir string, repos []preparedRepo) (*observed, error) {
	spec := r.spec(a, repos)
	stdin, err := protojson.Marshal(spec.runSpec)
	if err != nil {
		return nil, err
	}
	spec.sandbox.Stdin = stdin
	rawLog, err := os.OpenFile(filepath.Join(dir, "raw.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer rawLog.Close()
	stream, err := r.srv.ReportEvents(ctx)
	if err != nil {
		return nil, err
	}

	ev := &observed{}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { r.forward(stream, a.GetRunId(), outR, ev) })
	wg.Go(func() { writeRawLog(rawLog, a.GetRunId(), errR) })

	runCtx, cancel := context.WithTimeout(ctx, spec.sandbox.Timeout)
	defer cancel()
	sbErr := r.sb.Run(runCtx, spec.sandbox, outW, errW)
	outW.Close()
	errW.Close()
	wg.Wait()
	if errors.Is(sbErr, sandbox.ErrTimeout) || (ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded)) {
		sbErr = errTimeout
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		slog.Error("report events", "run", a.GetRunId(), "error", err)
	}
	return ev, sbErr
}

type runSpecs struct {
	runSpec *pluginv1.RunSpec
	sandbox sandbox.Spec
}

func (r *Runner) spec(a *workerv1.RunAssignment, repos []preparedRepo) runSpecs {
	rs := &pluginv1.RunSpec{}
	if a.GetSpec() != nil {
		rs = proto.Clone(a.GetSpec()).(*pluginv1.RunSpec)
	}
	rs.RunId = a.GetRunId()
	rs.Workdirs = nil
	var mounts []sandbox.Mount
	for _, repo := range repos {
		ctr := workdirPrefix + repo.name
		rs.Workdirs = append(rs.Workdirs, ctr)
		mounts = append(mounts, sandbox.Mount{HostPath: repo.agent, ContainerPath: ctr, ReadOnly: a.GetKind() == kindQuestion})
		if repo.file.Instructions != "" {
			rs.Instructions += fmt.Sprintf("\n\nInstructions for repository %s:\n%s", repo.slug, repo.file.Instructions)
		}
	}
	timeout := a.GetMaxDuration().AsDuration()
	if timeout <= 0 {
		timeout = defaultMaxDuration
	}
	return runSpecs{runSpec: rs, sandbox: sandbox.Spec{
		Name: sandbox.ContainerPrefix + a.GetRunId(), Image: r.cfg.Image, Network: r.cfg.Network, Mounts: mounts,
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": r.cfg.ProxyURL,
			"ANTHROPIC_API_KEY":  apiKeyPlaceholder,
			"HTTPS_PROXY":        r.cfg.ProxyURL,
			"HTTP_PROXY":         r.cfg.ProxyURL,
			"NO_PROXY":           proxyHost,
		},
		Timeout: timeout,
	}}
}

// observed is what the runner remembers from the event stream.
type observed struct {
	sessionID, question string
	finished            *pluginv1.AgentEvent
}

func (o *observed) note(e *pluginv1.AgentEvent) {
	switch e.GetType() {
	case eventStarted:
		o.sessionID = e.GetSessionId()
	case eventQuestion:
		o.question = e.GetText()
	case eventFinished:
		o.finished = e
	}
}

// forward sends every parsable stdout line as an event. After a failed
// Send it keeps draining rd, so a broken stream never blocks the container.
// ponytail: Send is synchronous, so a slow server backpressures the
// container's stdout; upgrade to a bounded channel plus a stream ctx
// cancelled at the run timeout if that shows up.
func (r *Runner) forward(stream workerv1.WorkerService_ReportEventsClient, runID string, rd io.Reader, ev *observed) {
	defer io.Copy(io.Discard, rd)
	sc := bufio.NewScanner(rd)
	sc.Buffer(nil, maxLineBytes)
	sending := true
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		e := &pluginv1.AgentEvent{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(line, e); err != nil {
			slog.Warn("skip unparsable agent event", "run", runID, "line", snippet(line))
			continue
		}
		ev.note(e)
		if sending {
			if err := stream.Send(&workerv1.ReportEventsRequest{RunId: runID, WorkerId: r.cfg.WorkerID, Event: e}); err != nil {
				slog.Error("send agent event", "run", runID, "error", err)
				sending = false
			}
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("agent output unreadable", "run", runID, "error", err)
	}
}

// writeRawLog stores the adapter's stderr line by line with secrets masked.
func writeRawLog(w io.Writer, runID string, rd io.Reader) {
	defer io.Copy(io.Discard, rd)
	sc := bufio.NewScanner(rd)
	sc.Buffer(nil, maxLineBytes)
	for sc.Scan() {
		if _, err := io.WriteString(w, redact.String(sc.Text())+"\n"); err != nil {
			slog.Error("write raw log", "run", runID, "error", err)
			return
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("raw log line dropped", "run", runID, "error", err)
	}
}

func snippet(line []byte) string {
	// Redact first: a cut through a token would leave its prefix unmatched.
	s := redact.String(string(line))
	if len(s) > logSnippetBytes {
		s = s[:logSnippetBytes]
	}
	return s
}
