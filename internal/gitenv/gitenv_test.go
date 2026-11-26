package gitenv

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestEnvPassesGitSSLCAInfo(t *testing.T) {
	t.Setenv("GIT_SSL_CAINFO", "/etc/ca/corp.pem")
	t.Setenv("GIT_SSL_CAPATH", "/etc/ca")

	env := Env("/h")

	for _, want := range []string{"GIT_SSL_CAINFO=/etc/ca/corp.pem", "GIT_SSL_CAPATH=/etc/ca"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %q", want)
		}
	}
}

func TestEnvOmitsCASettingsWhenUnset(t *testing.T) {
	// t.Setenv registers restore; Unsetenv-after-Setenv leaves them unset.
	t.Setenv("GIT_SSL_CAINFO", "")
	t.Setenv("GIT_SSL_CAPATH", "")
	unset(t, "GIT_SSL_CAINFO", "GIT_SSL_CAPATH")

	for _, kv := range Env("/h") {
		if strings.HasPrefix(kv, "GIT_SSL_CA") {
			t.Errorf("unexpected %q", kv)
		}
	}
}

func TestEnvStillDropsOtherGitVars(t *testing.T) {
	t.Setenv("GIT_DIR", "/evil")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.x=y'")
	t.Setenv("GIT_ASKPASS", "/evil/askpass")

	env := Env("/h")

	for _, bad := range []string{"GIT_DIR=/evil", "GIT_CONFIG_PARAMETERS='core.x=y'", "GIT_ASKPASS=/evil/askpass"} {
		if slices.Contains(env, bad) {
			t.Errorf("env leaks %q", bad)
		}
	}
}

func TestEnvKeepsProxyVars(t *testing.T) {
	t.Setenv("https_proxy", "http://proxy:3128")
	t.Setenv("NO_PROXY", "localhost")

	env := Env("/h")

	for _, want := range []string{"https_proxy=http://proxy:3128", "NO_PROXY=localhost"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %q", want)
		}
	}
}

func unset(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}
