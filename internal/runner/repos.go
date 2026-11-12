package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/worker/v1"
	"github.com/orchivio/orchestrator/contract/redact"
	"github.com/orchivio/orchestrator/contract/repofile"
	"github.com/orchivio/worker-podman/internal/gitenv"
)

const (
	repoFileName    = ".orchivio.yaml"
	defaultUsername = "x-access-token"
	tokenMask       = "****"
)

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// errRepoFile marks an invalid .orchivio.yaml; its message is safe to show.
var errRepoFile = errors.New("invalid " + repoFileName)

// preparedRepo is one repository cloned for a run.
type preparedRepo struct {
	slug, name, base string
	pristine         string // bare clone, never mounted, no remote
	agent            string // agent copy, mounted at /work/<name>
	file             repofile.RepoFile
	allowedPaths     []string // from the server config of this repo only
}

// hostGit runs git on the host with the hardened env. Errors carry
// stderr with the token masked, never the arguments (they hold URLs).
type hostGit struct {
	env   []string
	dir   string
	token string
}

func (g hostGit) run(ctx context.Context, gitDir string, args ...string) (string, error) {
	var full []string
	if gitDir != "" {
		full = append(full, "--git-dir="+gitDir)
	}
	full = append(append(full, gitenv.Args()...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = g.dir
	cmd.Env = g.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, g.sanitize(stderr.String()))
	}
	return stdout.String(), nil
}

func (g hostGit) sanitize(s string) string {
	if g.token != "" {
		s = strings.ReplaceAll(s, g.token, tokenMask)
	}
	return strings.TrimSpace(redact.String(s))
}

// prepare clones every repo of the run: a bare pristine clone with the
// token (remote removed afterwards) and a token-free agent copy of it.
func prepare(ctx context.Context, a *workerv1.RunAssignment, dir string, creds *pluginv1.RepoCredentials) ([]preparedRepo, error) {
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create git home: %w", err)
	}
	g := hostGit{env: gitenv.Env(home), dir: dir, token: creds.GetToken()}
	var repos []preparedRepo
	seen := map[string]bool{}
	for _, r := range a.GetRepos() {
		name := path.Base(r.GetSlug())
		if !repoNamePattern.MatchString(name) || name == "." || name == ".." || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate repo name in slug %q", r.GetSlug())
		}
		seen[name] = true
		p, err := prepareRepo(ctx, g, a, dir, r, name, creds)
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", r.GetSlug(), err)
		}
		repos = append(repos, p)
	}
	return repos, nil
}

func prepareRepo(ctx context.Context, g hostGit, a *workerv1.RunAssignment, dir string, r *workerv1.Repo, name string, creds *pluginv1.RepoCredentials) (preparedRepo, error) {
	p := preparedRepo{slug: r.GetSlug(), name: name, base: r.GetBaseBranch(), allowedPaths: r.GetAllowedPaths(),
		pristine: filepath.Join(dir, "pristine", name+".git"), agent: filepath.Join(dir, "repos", name)}
	remote, err := cloneURL(creds.GetCloneUrls()[p.slug], creds)
	if err != nil {
		return p, err
	}
	if _, err := g.run(ctx, "", "clone", "--bare", "--quiet", "--", remote, p.pristine); err != nil {
		return p, err
	}
	if _, err := g.run(ctx, p.pristine, "remote", "remove", "origin"); err != nil {
		return p, err
	}
	if p.file, err = readRepoFile(ctx, g, p.pristine, p.base); err != nil {
		return p, err
	}
	checkout := p.base
	if a.GetBranch() != "" {
		if _, err := g.run(ctx, p.pristine, "rev-parse", "--verify", "--quiet", "refs/heads/"+a.GetBranch()); err == nil {
			checkout = a.GetBranch() // follow-up run continues on its branch
		}
	}
	// --no-hardlinks: the agent may rewrite files in its copy; shared
	// inodes would let it corrupt the pristine objects push relies on.
	if _, err := g.run(ctx, "", "clone", "--quiet", "--no-hardlinks", "--branch", checkout, "--", p.pristine, p.agent); err != nil {
		return p, err
	}
	// The origin would name the host path of the pristine clone.
	_, err = g.run(ctx, filepath.Join(p.agent, ".git"), "remote", "remove", "origin")
	return p, err
}

// readRepoFile reads .orchivio.yaml from the base branch of the pristine
// clone, never from the agent copy. A missing file is no restriction.
func readRepoFile(ctx context.Context, g hostGit, pristine, base string) (repofile.RepoFile, error) {
	out, err := g.run(ctx, pristine, "ls-tree", "-z", "refs/heads/"+base, "--", repoFileName)
	if err != nil {
		return repofile.RepoFile{}, err
	}
	if out == "" {
		return repofile.RepoFile{}, nil
	}
	meta, _, _ := strings.Cut(out, "\t")
	fields := strings.Fields(meta) // mode type sha
	if len(fields) != 3 || fields[1] != "blob" {
		return repofile.RepoFile{}, fmt.Errorf("%w: not a regular file", errRepoFile)
	}
	data, err := g.run(ctx, pristine, "cat-file", "blob", fields[2])
	if err != nil {
		return repofile.RepoFile{}, err
	}
	f, err := repofile.Parse([]byte(data))
	if err != nil {
		return repofile.RepoFile{}, fmt.Errorf("%w: %v", errRepoFile, err)
	}
	return f, nil
}

// cloneURL adds the token to http(s) URLs; file URLs (tests, local
// remotes) are used as they are. Other transports are refused.
func cloneURL(raw string, creds *pluginv1.RepoCredentials) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return "", errors.New("missing or invalid clone url")
	}
	switch u.Scheme {
	case "file":
		return raw, nil
	case "https", "http":
		if creds.GetToken() == "" {
			return raw, nil
		}
		user := creds.GetUsername()
		if user == "" {
			user = defaultUsername
		}
		u.User = url.UserPassword(user, creds.GetToken())
		return u.String(), nil
	default:
		return "", fmt.Errorf("unsupported clone url scheme %q", u.Scheme)
	}
}

// forceRemove deletes dir even if the agent left directories without
// write or search permission behind.
func forceRemove(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
