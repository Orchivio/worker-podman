package push

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/orchivio/orchestrator/contract/redact"
	"github.com/orchivio/worker-podman/internal/gitenv"
)

const orchivioIdent = "Orchivio"
const orchivioEmail = "orchivio@localhost"

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
	env := append(gitenv.Env(home),
		"GIT_INDEX_FILE="+filepath.Join(tmp, "index"),
		"GIT_AUTHOR_NAME="+orchivioIdent, "GIT_AUTHOR_EMAIL="+orchivioEmail,
		"GIT_COMMITTER_NAME="+orchivioIdent, "GIT_COMMITTER_EMAIL="+orchivioEmail,
	)
	return &git{gitDir: gitDir, workTree: workTree, remoteURL: remoteURL, env: env}, nil
}

// run executes git and returns stdout. Errors carry sanitized stderr only,
// never the arguments, because those may contain the remote URL.
func (g *git) run(ctx context.Context, stdin string, args ...string) (string, error) {
	full := []string{"--git-dir=" + g.gitDir, "--work-tree=" + g.workTree}
	if g.attrSource != "" {
		full = append(full, "--attr-source="+g.attrSource)
	}
	full = append(full, gitenv.Args()...)
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
