// Package gitenv builds the hardened environment and arguments for every
// git call the worker makes on the host: no hooks, no fsmonitor, no
// global or system config, only https and file transports.
package gitenv

import (
	"os"
	"strings"
)

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
	"core.protectHFS=true",
	"core.protectNTFS=true",
	"maintenance.auto=false",
	"gc.auto=0",
}

// Args returns the hardening settings as "-c key=value" pairs; they go
// before the git subcommand.
func Args() []string {
	args := make([]string, 0, 2*len(hardening))
	for _, c := range hardening {
		args = append(args, "-c", c)
	}
	return args
}

// Env returns the process environment without any inherited GIT_*
// (GIT_DIR, GIT_CONFIG_PARAMETERS, GIT_SSH_COMMAND, ...) or
// config-locating variable, with system/global config off and HOME set
// to home, an empty directory owned by the caller.
func Env(home string) []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=0",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=" + home,
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
