package push

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/soemii/orchivio-server/contract/redact"
)

const orchivioIdent = "Orchivio"
const orchivioEmail = "orchivio@localhost"

// hardening is passed as -c to every git call. Hooks, fsmonitor and
// signing are off; only https (code host) and file (tests, local
// remotes) transports are allowed, so a crafted URL like ext:: cannot run.
var hardening = []string{
	"core.hooksPath=/dev/null",
	"core.fsmonitor=false",
	"safe.directory=*",
	"core.symlinks=true",
	"core.autocrlf=false",
	"commit.gpgsign=false",
	"protocol.allow=never",
	"protocol.https.allow=always",
	"protocol.file.allow=always",
}

// git runs the system git against our own pristine git dir, with the
// agent directory only as work tree. The agent's .git is never read.
type git struct {
	gitDir, workTree string
	remoteURL        string
	env              []string
	attrSource       string // tree whose .gitattributes apply; agent's are ignored
}

func newGit(gitDir, workTree, remoteURL, tmp string) (*git, error) {
	home := filepath.Join(tmp, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, fmt.Errorf("push: create temp home: %w", err)
	}
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=0",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=" + home,
		"GIT_INDEX_FILE=" + filepath.Join(tmp, "index"),
		"GIT_AUTHOR_NAME=" + orchivioIdent, "GIT_AUTHOR_EMAIL=" + orchivioEmail,
		"GIT_COMMITTER_NAME=" + orchivioIdent, "GIT_COMMITTER_EMAIL=" + orchivioEmail,
	}
	for _, kv := range os.Environ() {
		// Drop every inherited GIT_* (GIT_DIR, GIT_CONFIG_PARAMETERS, GIT_SSH_COMMAND, ...)
		// and config-locating variables.
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return &git{gitDir: gitDir, workTree: workTree, remoteURL: remoteURL, env: env}, nil
}

// run executes git and returns stdout. Errors carry sanitized stderr only,
// never the arguments, because those may contain the remote URL.
func (g *git) run(ctx context.Context, stdin string, args ...string) (string, error) {
	full := []string{"--git-dir=" + g.gitDir, "--work-tree=" + g.workTree}
	if g.attrSource != "" {
		full = append(full, "--attr-source="+g.attrSource)
	}
	for _, c := range hardening {
		full = append(full, "-c", c)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = os.TempDir() // never inside the agent dir, so no repo discovery there
	cmd.Env = g.env
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("push: git %s: %w: %s", args[0], err, g.sanitize(stderr.String()))
	}
	return stdout.String(), nil
}

// sanitize strips the remote URL (it holds a token) and known secrets.
func (g *git) sanitize(s string) string {
	if g.remoteURL != "" {
		s = strings.ReplaceAll(s, g.remoteURL, "<remote>")
	}
	return strings.TrimSpace(redact.String(s))
}
