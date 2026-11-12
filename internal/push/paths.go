package push

import (
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/orchivio/orchestrator/contract/redact"
)

// quotePath makes an agent-chosen file name safe for error messages:
// secrets masked, newlines and control characters escaped.
func quotePath(p string) string {
	return strconv.Quote(redact.String(p))
}

func symlinkEscapes(link, target string) bool {
	if path.IsAbs(target) {
		return true
	}
	resolved := path.Join(path.Dir(link), target)
	return resolved == ".." || strings.HasPrefix(resolved, "../")
}

// pathAllowed reports whether a changed path is well-formed, matches an
// allowed prefix and no forbidden prefix.
func pathAllowed(p string, allowed, forbidden []string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || strings.EqualFold(seg, ".git") {
			return false
		}
	}
	return matchesAny(p, allowed, false) && !matchesAny(p, forbidden, true)
}

// matchesAny matches whole path segments: prefix "src" covers "src" and
// "src/…", never "srcevil". fold compares segments case-insensitively
// (used for ForbiddenPaths, so a case-insensitive filesystem cannot dodge it).
func matchesAny(p string, prefixes []string, fold bool) bool {
	segs := strings.Split(p, "/")
	for _, prefix := range prefixes {
		pre := strings.Trim(path.Clean("/"+prefix), "/")
		if pre == "" {
			return true
		}
		preSegs := strings.Split(pre, "/")
		if len(preSegs) > len(segs) {
			continue
		}
		if slices.EqualFunc(segs[:len(preSegs)], preSegs, func(a, b string) bool {
			return a == b || fold && strings.EqualFold(a, b)
		}) {
			return true
		}
	}
	return false
}
