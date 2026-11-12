// Package push turns the changes an untrusted agent made in its working
// copy into one commit on an orchivio/… branch and pushes it, but only if
// path boundaries, diff size and the secret scan pass (anti-goals A2, A5).
package push

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/orchivio/orchestrator/contract/redact"
)

const (
	branchPrefix = "orchivio/"
	modeSymlink  = "120000"
	modeGitlink  = "160000"
	modeMissing  = "000000"

	// MaxChangedBytes caps the summed size of all added/modified blobs. It
	// bounds memory for the secret scan and catches binary files, which
	// count 0 lines against MaxDiffLines.
	MaxChangedBytes = 20 << 20
)

// maxChangedBytes is MaxChangedBytes; tests lower it.
var maxChangedBytes = MaxChangedBytes

// Request describes one push. RemoteURL holds a token and never appears in errors.
type Request struct {
	PristineGitDir string // own bare repo, cloned by the runner, never mounted
	AgentDir       string // work tree the agent modified
	BaseBranch     string
	Branch         string
	// AllowedPaths and ForbiddenPaths are path prefixes (directory or exact
	// file); "" means everything. An empty AllowedPaths allows nothing.
	AllowedPaths, ForbiddenPaths []string
	MaxDiffLines                 int
	CommitMessage                string
	RemoteURL                    string
}

// Result reports whether a commit was pushed and which paths it touched (sorted).
type Result struct {
	Pushed       bool
	ChangedFiles []string
}

var (
	ErrPathNotAllowed = errors.New("push: change outside allowed paths")
	ErrDiffTooLarge   = errors.New("push: diff too large")
	ErrSecretInDiff   = errors.New("push: secret in diff")
	ErrBadBranch      = errors.New("push: invalid branch")
)

// change is one entry of `git diff-index --raw --no-renames`.
type change struct {
	newMode, newSHA, path string
}

// Push commits the agent's changes on top of BaseBranch (or the existing
// remote Branch) and pushes them. No changes → Pushed=false, nil error.
func Push(ctx context.Context, req Request) (Result, error) {
	if err := validate(req); err != nil {
		return Result{}, err
	}
	tmp, err := os.MkdirTemp("", "orchivio-push-")
	if err != nil {
		return Result{}, fmt.Errorf("push: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	g, err := newGit(req.PristineGitDir, req.AgentDir, req.RemoteURL, tmp)
	if err != nil {
		return Result{}, err
	}
	if _, err := g.run(ctx, "", "check-ref-format", "refs/heads/"+req.Branch); err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrBadBranch, req.Branch)
	}
	parent, err := resolveParent(ctx, g, req)
	if err != nil {
		return Result{}, err
	}
	g.attrSource = parent
	files, err := stageAndCheck(ctx, g, req, parent)
	if err != nil || len(files) == 0 {
		return Result{}, err
	}
	if err := commitAndPush(ctx, g, req, parent); err != nil {
		return Result{}, err
	}
	return Result{Pushed: true, ChangedFiles: files}, nil
}

func validate(req Request) error {
	if !strings.HasPrefix(req.Branch, branchPrefix) {
		return fmt.Errorf("%w: %s", ErrBadBranch, req.Branch)
	}
	if req.MaxDiffLines <= 0 {
		return fmt.Errorf("push: MaxDiffLines must be positive, got %d", req.MaxDiffLines)
	}
	if req.PristineGitDir == "" || req.AgentDir == "" || req.BaseBranch == "" || req.CommitMessage == "" {
		return errors.New("push: PristineGitDir, AgentDir, BaseBranch and CommitMessage are required")
	}
	if !filepath.IsAbs(req.PristineGitDir) || !filepath.IsAbs(req.AgentDir) {
		return errors.New("push: PristineGitDir and AgentDir must be absolute")
	}
	if req.RemoteURL == "" || strings.HasPrefix(req.RemoteURL, "-") {
		return errors.New("push: invalid RemoteURL")
	}
	return nil
}

// resolveParent returns the remote Branch head if it exists (follow-up
// run), fetched into the pristine repo, else the BaseBranch commit.
func resolveParent(ctx context.Context, g *git, req Request) (string, error) {
	ref := "refs/heads/" + req.Branch
	out, err := g.run(ctx, "", "ls-remote", req.RemoteURL, ref)
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(out) {
		sha, name, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if name != ref {
			continue
		}
		if _, err := g.run(ctx, "", "fetch", "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no", req.RemoteURL, ref); err != nil {
			return "", err
		}
		return verifyCommit(ctx, g, sha)
	}
	return verifyCommit(ctx, g, "refs/heads/"+req.BaseBranch)
}

func verifyCommit(ctx context.Context, g *git, rev string) (string, error) {
	out, err := g.run(ctx, "", "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	return strings.TrimSpace(out), err
}

// stageAndCheck builds the private index from parent plus the agent's work
// tree and enforces all boundaries. It returns the sorted changed paths.
func stageAndCheck(ctx context.Context, g *git, req Request, parent string) ([]string, error) {
	if _, err := g.run(ctx, "", "read-tree", parent); err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, "", "add", "-A"); err != nil {
		return nil, err
	}
	changes, err := diffChanges(ctx, g, parent)
	if err != nil || len(changes) == 0 {
		return nil, err
	}
	files := make([]string, 0, len(changes))
	for _, c := range changes {
		if err := checkChange(ctx, g, c, req); err != nil {
			return nil, err
		}
		files = append(files, c.path)
	}
	slices.Sort(files)
	files = slices.Compact(files)
	if err := checkSize(ctx, g, parent, req.MaxDiffLines); err != nil {
		return nil, err
	}
	if err := checkBytes(ctx, g, changes); err != nil {
		return nil, err
	}
	return files, checkSecrets(ctx, g, parent, req.CommitMessage, files)
}

// diffChanges lists index changes against parent. With --no-renames a
// rename is a deletion plus an addition, so both paths get checked.
func diffChanges(ctx context.Context, g *git, parent string) ([]change, error) {
	out, err := g.run(ctx, "", "diff-index", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev", parent)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var changes []change
	for i := 0; i+1 < len(fields); i += 2 {
		// ":oldmode newmode oldsha newsha status" then the path
		meta := strings.Fields(fields[i])
		if len(meta) != 5 {
			return nil, fmt.Errorf("push: unexpected diff-index line %q", fields[i])
		}
		changes = append(changes, change{newMode: meta[1], newSHA: meta[3], path: fields[i+1]})
	}
	return changes, nil
}

func checkChange(ctx context.Context, g *git, c change, req Request) error {
	if !pathAllowed(c.path, req.AllowedPaths, req.ForbiddenPaths) {
		return fmt.Errorf("%w: %s", ErrPathNotAllowed, quotePath(c.path))
	}
	if strings.EqualFold(path.Base(c.path), ".gitmodules") || c.newMode == modeGitlink {
		return fmt.Errorf("%w: submodule %s", ErrPathNotAllowed, quotePath(c.path))
	}
	if c.newMode != modeSymlink {
		return nil
	}
	target, err := g.run(ctx, "", "cat-file", "blob", c.newSHA)
	if err != nil {
		return err
	}
	if symlinkEscapes(c.path, target) {
		return fmt.Errorf("%w: symlink %s leaves the repository", ErrPathNotAllowed, quotePath(c.path))
	}
	return nil
}

// checkSize sums added+deleted lines; binary files count 0 lines.
func checkSize(ctx context.Context, g *git, parent string, maxLines int) error {
	out, err := g.run(ctx, "", "diff-index", "--cached", "--numstat", "-z", "--no-renames", parent)
	if err != nil {
		return err
	}
	total := 0
	for _, entry := range strings.Split(out, "\x00") {
		added, rest, _ := strings.Cut(entry, "\t")
		deleted, _, _ := strings.Cut(rest, "\t")
		a, _ := strconv.Atoi(added) // "-" (binary) → 0
		d, _ := strconv.Atoi(deleted)
		total += a + d
	}
	if total > maxLines {
		return fmt.Errorf("%w: %d lines, max %d", ErrDiffTooLarge, total, maxLines)
	}
	return nil
}

// checkBytes sums the blob sizes of all added/modified entries and runs
// before the secret scan, so the --text patch stays bounded in memory.
func checkBytes(ctx context.Context, g *git, changes []change) error {
	var shas strings.Builder
	for _, c := range changes {
		if c.newMode != modeMissing {
			shas.WriteString(c.newSHA + "\n")
		}
	}
	out, err := g.run(ctx, shas.String(), "cat-file", "--batch-check=%(objectsize)")
	if err != nil {
		return err
	}
	total := 0
	for line := range strings.Lines(out) {
		size, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return fmt.Errorf("push: unexpected cat-file output %q", line)
		}
		total += size
	}
	if total > maxChangedBytes {
		return fmt.Errorf("%w: %d bytes, max %d", ErrDiffTooLarge, total, maxChangedBytes)
	}
	return nil
}

// checkSecrets scans every added line (binary files included via --text),
// the commit message and the changed path names.
func checkSecrets(ctx context.Context, g *git, parent, message string, files []string) error {
	out, err := g.run(ctx, "", "diff-index", "--cached", "-p", "-U0", "--text", "--no-renames",
		"--no-ext-diff", "--no-textconv", "--no-color", parent)
	if err != nil {
		return err
	}
	scan := strings.Join(addedLines(out), "\n") + "\n" + message + "\n" + strings.Join(files, "\n")
	if redact.Contains(scan) {
		return ErrSecretInDiff
	}
	return nil
}

// addedLines returns the "+" lines inside hunks. Headers ("+++ b/…") only
// occur before a file's first "@@", so content starting with "++" is kept.
func addedLines(patch string) []string {
	var added []string
	inHunk := false
	for line := range strings.Lines(patch) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(line, "+"):
			added = append(added, line[1:])
		}
	}
	return added
}

// commitAndPush writes the index as a commit with parent and pushes that
// sha. HEAD and refs of the pristine repo are never moved; no --force.
func commitAndPush(ctx context.Context, g *git, req Request, parent string) error {
	tree, err := g.run(ctx, "", "write-tree")
	if err != nil {
		return err
	}
	commit, err := g.run(ctx, req.CommitMessage, "commit-tree", strings.TrimSpace(tree), "-p", parent, "-F", "-")
	if err != nil {
		return err
	}
	refspec := strings.TrimSpace(commit) + ":refs/heads/" + req.Branch
	_, err = g.run(ctx, "", "push", "--porcelain", "--no-verify", "--recurse-submodules=no", req.RemoteURL, refspec)
	return err
}
