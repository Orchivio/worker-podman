package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/worker/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func perRepoCreds() *pluginv1.RepoCredentials {
	return &pluginv1.RepoCredentials{
		Username: "top-user", Token: "top-token",
		Tokens: map[string]*pluginv1.RepoToken{"g/a": {Username: "bot-a", Token: "ta"}},
	}
}

func TestCloneUsesPerRepoToken(t *testing.T) {
	creds := perRepoCreds()
	got, err := cloneURL("https://gl.example/g/a.git", forRepo(creds, "g/a"))
	if err != nil || got != "https://bot-a:ta@gl.example/g/a.git" {
		t.Fatalf("g/a = %q %v", got, err)
	}
	got, err = cloneURL("https://gl.example/g/b.git", forRepo(creds, "g/b"))
	if err != nil || got != "https://top-user:top-token@gl.example/g/b.git" {
		t.Fatalf("g/b = %q %v", got, err)
	}
}

func TestForRepoFallsBackOnEmptyToken(t *testing.T) {
	creds := perRepoCreds()
	creds.Tokens["g/a"].Token = ""
	if got := forRepo(creds, "g/a"); got.GetToken() != "top-token" || got.GetUsername() != "top-user" {
		t.Fatalf("got %s/%s", got.GetUsername(), got.GetToken())
	}
}

func TestForRepoUsesPerRepoExpiry(t *testing.T) {
	top, own := timestamppb.New(testNow.Add(time.Hour)), timestamppb.New(testNow.Add(time.Minute))
	creds := perRepoCreds()
	creds.ExpiresAt = top
	creds.Tokens["g/a"].ExpiresAt = own
	if got := forRepo(creds, "g/a").GetExpiresAt(); !got.AsTime().Equal(own.AsTime()) {
		t.Fatalf("g/a expiry = %v", got)
	}
	if got := forRepo(creds, "g/b").GetExpiresAt(); !got.AsTime().Equal(top.AsTime()) {
		t.Fatalf("g/b expiry = %v", got)
	}
}

func TestHostGitMasksPerRepoToken(t *testing.T) {
	creds := perRepoCreds()
	creds.Tokens["g/a"].Token = "secret-ta-42"
	g := hostGit{env: os.Environ(), dir: t.TempDir(), token: forRepo(creds, "g/a").GetToken()}
	// git names the missing path (which holds the token) in its stderr.
	_, err := g.run(context.Background(), "", "clone", "--", "/missing/secret-ta-42.git", filepath.Join(g.dir, "x"))
	if err == nil || strings.Contains(err.Error(), "secret-ta-42") || !strings.Contains(err.Error(), "/missing/****.git") {
		t.Fatalf("err = %v", err)
	}
}

func TestPushRefreshesWhenOnlyPerRepoTokenExpires(t *testing.T) {
	e := newEnv(t, changeFile("src/new.go", "package main\n"))
	e.srv.creds.ExpiresAt = nil
	e.srv.creds.Tokens = map[string]*pluginv1.RepoToken{
		testSlug: {Username: "bot", Token: "per-repo", ExpiresAt: timestamppb.New(testNow.Add(-time.Minute))}}
	got := execute(t, e, assignment("task"))
	if got.GetOutcome() != workerv1.CompleteRunRequest_PUSHED {
		t.Fatalf("complete = %v", got)
	}
	if !slices.Equal(e.srv.credWrites, []bool{true, true}) {
		t.Fatalf("credential writes = %v, want [true true]", e.srv.credWrites)
	}
}
