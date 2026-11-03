package push

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testBranch = "orchivio/orc-1-abcd1234"

// fakeToken matches the redact patterns, so it must never reach a commit.
var fakeToken = "ghp_" + strings.Repeat("a", 36)

// hostGit runs git for test setup, isolated from the developer's config.
func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "protocol.file.allow=always", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(testEnv(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type fixture struct {
	remote, pristine, agent string
}

// newFixture creates a bare remote with main (README.md, src/main.go),
// a pristine bare clone (as the runner does) and an agent working copy.
func newFixture(t *testing.T) fixture {
	t.Helper()
	return newFixtureAt(t, filepath.Join(t.TempDir(), "remote.git"))
}

func newFixtureAt(t *testing.T, remote string) fixture {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	hostGit(t, root, "init", "--bare", remote)
	hostGit(t, root, "init", seed)
	write(t, seed, "README.md", "hello\n")
	write(t, seed, "src/main.go", "package main\n")
	hostGit(t, seed, "add", "-A")
	hostGit(t, seed, "commit", "-m", "init")
	hostGit(t, seed, "push", remote, "HEAD:refs/heads/main")
	f := fixture{remote: remote, pristine: filepath.Join(root, "pristine.git"), agent: filepath.Join(root, "agent")}
	hostGit(t, root, "clone", "--bare", remote, f.pristine)
	hostGit(t, root, "clone", remote, f.agent)
	return f
}

func (f fixture) request() Request {
	return Request{
		PristineGitDir: f.pristine,
		AgentDir:       f.agent,
		BaseBranch:     "main",
		Branch:         testBranch,
		AllowedPaths:   []string{""},
		MaxDiffLines:   100,
		CommitMessage:  "orc-1: agent change",
		RemoteURL:      f.remote,
	}
}

func (f fixture) remoteHasBranch(t *testing.T) bool {
	t.Helper()
	return exec.Command("git", "--git-dir", f.remote, "rev-parse", "--verify", "-q", "refs/heads/"+testBranch).Run() == nil
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func expectRejected(t *testing.T, f fixture, req Request, want error) {
	t.Helper()
	res, err := Push(context.Background(), req)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if res.Pushed || f.remoteHasBranch(t) {
		t.Fatal("rejected change reached the remote")
	}
}

func TestPushCommitsAgentChangesToBranch(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "README.md", "hello agent\n")

	res, err := Push(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed || !slices.Equal(res.ChangedFiles, []string{"README.md"}) {
		t.Fatalf("result = %+v", res)
	}
	if got := hostGit(t, f.remote, "show", testBranch+":README.md"); got != "hello agent" {
		t.Fatalf("remote README = %q", got)
	}
	if got := hostGit(t, f.remote, "log", "-1", "--format=%an <%ae>|%cn <%ce>|%s", testBranch); got != "Orchivio <orchivio@localhost>|Orchivio <orchivio@localhost>|orc-1: agent change" {
		t.Fatalf("commit meta = %q", got)
	}
	if got, want := hostGit(t, f.remote, "rev-parse", testBranch+"^"), hostGit(t, f.remote, "rev-parse", "main"); got != want {
		t.Fatalf("parent = %s, want main %s", got, want)
	}
}

func TestPushIgnoresAgentGitDir(t *testing.T) {
	f := newFixture(t)
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	fsmonitorMarker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := "#!/bin/sh\ntouch " + hookMarker + "\n"
	for _, name := range []string{"pre-commit", "post-commit", "pre-push", "post-index-change", "reference-transaction"} {
		write(t, f.agent, ".git/hooks/"+name, hook)
	}
	hostGit(t, f.agent, "config", "core.fsmonitor", "touch "+fsmonitorMarker+" #")
	write(t, f.agent, "README.md", "changed\n")

	if _, err := Push(context.Background(), f.request()); err != nil {
		t.Fatal(err)
	}
	if exists(hookMarker) || exists(fsmonitorMarker) {
		t.Fatal("agent-controlled hook or fsmonitor ran on the host")
	}
	for _, p := range strings.Split(hostGit(t, f.remote, "ls-tree", "-r", "--name-only", testBranch), "\n") {
		if p == ".git" || strings.HasPrefix(p, ".git/") {
			t.Fatalf("agent .git committed: %s", p)
		}
	}
	// Control: the planted config and hooks are live when git uses the agent's .git.
	hostGit(t, f.agent, "commit", "-am", "control")
	if !exists(hookMarker) || !exists(fsmonitorMarker) {
		t.Fatal("control failed: planted hooks/fsmonitor did not fire, test is vacuous")
	}
}

func TestPushIgnoresAgentRemoteRewrite(t *testing.T) {
	f := newFixture(t)
	evil := filepath.Join(t.TempDir(), "evil.git")
	hostGit(t, f.agent, "init", "--bare", evil)
	hostGit(t, f.agent, "config", "url."+evil+".insteadOf", f.remote)
	write(t, f.agent, "README.md", "changed\n")

	if _, err := Push(context.Background(), f.request()); err != nil {
		t.Fatal(err)
	}
	if !f.remoteHasBranch(t) || hostGit(t, evil, "for-each-ref") != "" {
		t.Fatal("agent .git/config redirected the push")
	}
	// Control: git using the agent's .git does follow the rewrite.
	hostGit(t, f.agent, "push", f.remote, "HEAD:refs/heads/control")
	if hostGit(t, evil, "for-each-ref") == "" {
		t.Fatal("control failed: insteadOf did not redirect, test is vacuous")
	}
}

func TestPushRejectsPathOutsideAllowed(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "README.md", "changed\n")
	req := f.request()
	req.AllowedPaths = []string{"src"}
	expectRejected(t, f, req, ErrPathNotAllowed)
}

func TestPushRejectsForbiddenPath(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, ".github/workflows/ci.yml", "on: push\n")
	req := f.request()
	req.ForbiddenPaths = []string{".github/workflows"}
	expectRejected(t, f, req, ErrPathNotAllowed)
}

func TestPushRejectsLargeDiff(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "src/big.go", strings.Repeat("x\n", 10))
	req := f.request()
	req.MaxDiffLines = 5
	expectRejected(t, f, req, ErrDiffTooLarge)
}

func TestPushRejectsLargeDiffMarkedBinaryByAgent(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, ".gitattributes", "* -diff\n")
	write(t, f.agent, "src/big.go", strings.Repeat("x\n", 10))
	req := f.request()
	req.MaxDiffLines = 5
	expectRejected(t, f, req, ErrDiffTooLarge)
}

func TestPushRejectsSecret(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "src/config.go", "package main\nconst token = \""+fakeToken+"\"\n")
	expectRejected(t, f, f.request(), ErrSecretInDiff)
}

func TestPushRejectsNonOrchivioBranch(t *testing.T) {
	for _, branch := range []string{"feature/x", "main", "orchivio/bad..name", "orchivio/x.lock", "orchivio/"} {
		t.Run(branch, func(t *testing.T) {
			f := newFixture(t)
			write(t, f.agent, "README.md", "changed\n")
			req := f.request()
			req.Branch = branch
			if _, err := Push(context.Background(), req); !errors.Is(err, ErrBadBranch) {
				t.Fatalf("err = %v, want ErrBadBranch", err)
			}
		})
	}
}

func TestPushNoChanges(t *testing.T) {
	f := newFixture(t)
	res, err := Push(context.Background(), f.request())
	if err != nil || res.Pushed || len(res.ChangedFiles) != 0 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if f.remoteHasBranch(t) {
		t.Fatal("branch created without changes")
	}
}

func TestPushEmptyAllowedPathsRejectsEverything(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "src/main.go", "package main\n// x\n")
	req := f.request()
	req.AllowedPaths = nil
	expectRejected(t, f, req, ErrPathNotAllowed)
}

func TestPushPrefixIsSegmentAware(t *testing.T) {
	t.Run("sibling with same prefix is rejected", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.agent, "srcevil/x.go", "package x\n")
		req := f.request()
		req.AllowedPaths = []string{"src"}
		expectRejected(t, f, req, ErrPathNotAllowed)
	})
	t.Run("directory and exact file are allowed", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.agent, "src/new.go", "package main\n")
		write(t, f.agent, "README.md", "changed\n")
		req := f.request()
		req.AllowedPaths = []string{"src/", "README.md"}
		res, err := Push(context.Background(), req)
		if err != nil || !slices.Equal(res.ChangedFiles, []string{"README.md", "src/new.go"}) {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	})
}

func TestPushRejectsDeletionOutsideAllowed(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(filepath.Join(f.agent, "README.md")); err != nil {
		t.Fatal(err)
	}
	req := f.request()
	req.AllowedPaths = []string{"src"}
	expectRejected(t, f, req, ErrPathNotAllowed)
}

func TestPushRejectsRenameFromOutsideAllowed(t *testing.T) {
	f := newFixture(t)
	if err := os.Rename(filepath.Join(f.agent, "README.md"), filepath.Join(f.agent, "src", "README.md")); err != nil {
		t.Fatal(err)
	}
	req := f.request()
	req.AllowedPaths = []string{"src"}
	expectRejected(t, f, req, ErrPathNotAllowed)
}

func TestPushRejectsEscapingSymlink(t *testing.T) {
	for _, target := range []string{"/etc/passwd", "../../etc/passwd", "../.."} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t)
			if err := os.Symlink(target, filepath.Join(f.agent, "src", "link")); err != nil {
				t.Fatal(err)
			}
			expectRejected(t, f, f.request(), ErrPathNotAllowed)
		})
	}
	t.Run("in-repo symlink is allowed", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Symlink("../README.md", filepath.Join(f.agent, "src", "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := Push(context.Background(), f.request()); err != nil {
			t.Fatal(err)
		}
		if got := hostGit(t, f.remote, "ls-tree", testBranch, "src/link"); !strings.HasPrefix(got, "120000") {
			t.Fatalf("symlink not stored as symlink: %q", got)
		}
	})
}

func TestPushRejectsSubmodules(t *testing.T) {
	t.Run(".gitmodules", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.agent, ".gitmodules", "[submodule \"x\"]\n\tpath = x\n\turl = https://evil.example/x.git\n")
		expectRejected(t, f, f.request(), ErrPathNotAllowed)
	})
	t.Run("gitlink", func(t *testing.T) {
		f := newFixture(t)
		sub := filepath.Join(f.agent, "src", "sub")
		hostGit(t, f.agent, "init", sub)
		write(t, sub, "x", "x\n")
		hostGit(t, sub, "add", "-A")
		hostGit(t, sub, "commit", "-m", "x")
		expectRejected(t, f, f.request(), ErrPathNotAllowed)
	})
}

func TestPushRejectsNonPositiveMaxDiffLines(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "README.md", "changed\n")
	req := f.request()
	req.MaxDiffLines = 0
	res, err := Push(context.Background(), req)
	if err == nil || res.Pushed || f.remoteHasBranch(t) {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestPushRejectsSecretInCommitMessage(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "README.md", "changed\n")
	req := f.request()
	req.CommitMessage = "fix: use " + fakeToken
	expectRejected(t, f, req, ErrSecretInDiff)
}

func TestPushRejectsHiddenSecrets(t *testing.T) {
	cases := map[string]map[string]string{
		"line starting with ++": {"src/a.txt": "++" + fakeToken + "\n"},
		"marked binary by agent gitattributes": {
			".gitattributes": "* -diff -text\n",
			"src/a.txt":      fakeToken + "\n",
		},
		"binary file":    {"src/a.bin": "\x00\x01" + fakeToken + "\x00"},
		"in file name":   {"src/" + fakeToken: "x\n"},
		"multi-line key": {"src/id": "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			for p, c := range files {
				write(t, f.agent, p, c)
			}
			expectRejected(t, f, f.request(), ErrSecretInDiff)
		})
	}
}

func TestPushAgentGitattributesFilterIsInert(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	hostGit(t, f.agent, "config", "filter.evil.clean", "touch "+marker+"; cat")
	hostGit(t, f.agent, "config", "filter.evil.smudge", "touch "+marker+"; cat")
	hostGit(t, f.agent, "config", "filter.evil.required", "true")
	write(t, f.agent, ".gitattributes", "* filter=evil\n")
	write(t, f.agent, "README.md", "changed\n")

	if _, err := Push(context.Background(), f.request()); err != nil {
		t.Fatal(err)
	}
	if exists(marker) {
		t.Fatal("agent filter driver ran on the host")
	}
	if got := hostGit(t, f.remote, "show", testBranch+":README.md"); got != "changed" {
		t.Fatalf("README = %q", got)
	}
}

func TestPushSecondRunFastForwardsBranch(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "README.md", "v1\n")
	if _, err := Push(context.Background(), f.request()); err != nil {
		t.Fatal(err)
	}
	first := hostGit(t, f.remote, "rev-parse", testBranch)

	write(t, f.agent, "src/main.go", "package main\n// v2\n")
	req := f.request()
	req.AllowedPaths = []string{"src"} // README change is already in the parent
	res, err := Push(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.ChangedFiles, []string{"src/main.go"}) {
		t.Fatalf("changed = %v, want diff against previous branch head", res.ChangedFiles)
	}
	if got := hostGit(t, f.remote, "rev-parse", testBranch+"^"); got != first {
		t.Fatalf("parent = %s, want previous head %s", got, first)
	}
	if got := hostGit(t, f.remote, "show", testBranch+":README.md"); got != "v1" {
		t.Fatalf("README = %q", got)
	}
}

func TestPushErrorDoesNotLeakRemoteURL(t *testing.T) {
	const token = "s3cr3t-not-a-known-pattern"
	f := newFixtureAt(t, filepath.Join(t.TempDir(), "x-access-token:"+token+"@host", "repo.git"))
	objects := filepath.Join(f.remote, "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(objects, 0o755) })
	write(t, f.agent, "README.md", "changed\n")

	_, err := Push(context.Background(), f.request())
	if err == nil {
		t.Fatal("push into read-only remote succeeded")
	}
	if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "<remote>") {
		t.Fatalf("error leaks or lacks remote placeholder: %v", err)
	}
}

func TestPathRules(t *testing.T) {
	cases := []struct {
		path             string
		allowed, forbids []string
		ok               bool
	}{
		{"a/b", []string{""}, nil, true},
		{"a/b", nil, nil, false},
		{"a/b", []string{"a"}, nil, true},
		{"a/b", []string{"/a/"}, nil, true},
		{"ab/c", []string{"a"}, nil, false},
		{"a", []string{"a"}, nil, true},
		{"a/b", []string{""}, []string{"a"}, false},
		{"a/b", []string{""}, []string{"ab"}, true},
		{"/etc/passwd", []string{""}, nil, false},
		{"a/../b", []string{""}, nil, false},
		{"../b", []string{""}, nil, false},
		{"a//b", []string{""}, nil, false},
		{"x/.git/config", []string{""}, nil, false},
		{"x/.GIT/config", []string{""}, nil, false},
	}
	for _, c := range cases {
		if got := pathAllowed(c.path, c.allowed, c.forbids); got != c.ok {
			t.Errorf("pathAllowed(%q, %q, %q) = %v, want %v", c.path, c.allowed, c.forbids, got, c.ok)
		}
	}
}

// testEnv drops inherited git variables so tests are hermetic.
func testEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

func TestPushNULPrefixedFileCountsZeroLines(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, "src/data.bin", "\x00"+strings.Repeat("line\n", 100))
	req := f.request()
	req.MaxDiffLines = 5
	if _, err := Push(context.Background(), req); err != nil {
		t.Fatalf("binary under byte limit rejected: %v", err)
	}
}

func TestPushRejectsTooManyChangedBytes(t *testing.T) {
	old := maxChangedBytes
	maxChangedBytes = 64
	t.Cleanup(func() { maxChangedBytes = old })
	f := newFixture(t)
	write(t, f.agent, "src/data.bin", "\x00"+strings.Repeat("x", 100))
	expectRejected(t, f, f.request(), ErrDiffTooLarge)
}

func TestPushRejectedPathIsRedactedInError(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, fakeToken, "x\n")
	req := f.request()
	req.AllowedPaths = []string{"src"}
	_, err := Push(context.Background(), req)
	if !errors.Is(err, ErrPathNotAllowed) || strings.Contains(err.Error(), fakeToken) || !strings.Contains(err.Error(), "****") {
		t.Fatalf("err = %v", err)
	}
}

func TestPushForbiddenPathIsCaseInsensitive(t *testing.T) {
	f := newFixture(t)
	write(t, f.agent, ".Orchivio.yaml", "x: 1\n")
	req := f.request()
	req.ForbiddenPaths = []string{".orchivio.yaml"}
	expectRejected(t, f, req, ErrPathNotAllowed)
}

func TestMatchCaseRules(t *testing.T) {
	if pathAllowed("SRC/a", []string{"src"}, nil) {
		t.Error("AllowedPaths must be case-sensitive")
	}
	if pathAllowed("Secrets/Key", []string{""}, []string{"secrets/key"}) {
		t.Error("ForbiddenPaths must be case-insensitive")
	}
	if !pathAllowed("secretsx/key", []string{""}, []string{"secrets"}) {
		t.Error("case-insensitive forbidden match must stay segment-aware")
	}
}
