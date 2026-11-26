package runner

import (
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
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

func TestPushFailureRedactsPerRepoToken(t *testing.T) {
	repoCreds := forRepo(perRepoCreds(), "g/a")
	msg := pushFailure("run-1", "g/a", repoCreds.GetToken(), errors.New("remote https://bot-a:ta@gl.example failed"))
	if strings.Contains(msg, "bot-a:ta") || msg != "g/a: push failed" {
		t.Fatalf("message = %q", msg)
	}
	if repoCreds.GetToken() != "ta" {
		t.Fatalf("token = %q", repoCreds.GetToken())
	}
}
