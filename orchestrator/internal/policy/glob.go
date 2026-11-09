package policy

import (
	"path"
	"strings"
)

// Match reports whether a slash-separated, repository-relative path matches
// a path pattern. The rules follow .gitignore closely:
//
//   - A pattern without "/" (such as "AGENTS.md", "*.pem", ".env*") matches
//     any path segment at any depth, so it covers a file of that name and
//     everything inside a directory of that name.
//   - A pattern with "/" is anchored at the repository root (a leading "/"
//     is ignored). "**" matches zero or more whole segments; "*", "?", and
//     "[...]" match within one segment.
//   - A pattern that matches a directory also matches everything inside it:
//     "internal/gitops" covers "internal/gitops/x.go".
//
// fold makes the match case-insensitive (used for protected paths, so that a
// case variant on a case-insensitive file system cannot slip through).
func Match(pattern, p string, fold bool) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if fold {
		pattern, p = strings.ToLower(pattern), strings.ToLower(p)
	}
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/")
	segs := strings.Split(p, "/")
	if !strings.Contains(pattern, "/") {
		for _, s := range segs {
			if ok, _ := path.Match(pattern, s); ok {
				return true
			}
		}
		return false
	}
	pat := strings.Split(pattern, "/")
	// The path or any ancestor directory may match.
	for n := len(segs); n >= 1; n-- {
		if matchSegs(pat, segs[:n]) {
			return true
		}
	}
	return false
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(segs); i++ {
				if matchSegs(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// MatchAny reports whether p matches any of patterns.
func MatchAny(patterns []string, p string, fold bool) bool {
	for _, pat := range patterns {
		if Match(pat, p, fold) {
			return true
		}
	}
	return false
}
