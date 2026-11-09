package codexreview

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// errLinkLoop reports a path whose links do not resolve.
var errLinkLoop = errors.New("codexreview: too many links while resolving a path")

// isLink reports whether fi is a symbolic link or, on Windows, a junction
// or other reparse point (os.Lstat reports junctions as ModeIrregular
// since Go 1.23, and filepath.EvalSymlinks no longer follows them).
func isLink(fi fs.FileInfo) bool {
	return fi.Mode()&fs.ModeSymlink != 0 || (runtime.GOOS == "windows" && fi.Mode()&fs.ModeIrregular != 0)
}

// resolveLinks returns p with every symbolic link and junction among its
// existing components replaced by the link target, recursively. Components
// that do not exist are appended unresolved. A reparse point whose target
// cannot be read is an error (callers fail closed).
func resolveLinks(p string) (string, error) { return resolveDepth(filepath.Clean(p), 0) }

func resolveDepth(p string, depth int) (string, error) {
	if depth > 40 {
		return "", errLinkLoop
	}
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	sep := string(filepath.Separator)
	cur := vol
	if strings.HasPrefix(rest, sep) || strings.HasPrefix(rest, "/") {
		cur += sep
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == filepath.Separator || r == '/' })
	for i, part := range parts {
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(next)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.Join(append([]string{next}, parts[i+1:]...)...), nil
			}
			return "", err
		}
		if isLink(fi) {
			t, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			t = strings.TrimPrefix(t, `\\?\`)
			if !filepath.IsAbs(t) {
				t = filepath.Join(cur, t)
			}
			return resolveDepth(filepath.Join(append([]string{t}, parts[i+1:]...)...), depth+1)
		}
		cur = next
	}
	return filepath.Clean(cur), nil
}

// samePath compares cleaned paths (case-insensitively on Windows).
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// lexWithin reports whether p is root or below it, by name.
func lexWithin(root, p string) bool {
	if samePath(root, p) {
		return true
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// identityWithin reports whether an existing ancestor of p (p included) is
// the same directory as root by filesystem identity, which also matches
// different spellings of one directory (8.3 names on Windows, aliases).
func identityWithin(root, p string) bool {
	rfi, err := os.Stat(root)
	if err != nil {
		return false
	}
	for d := filepath.Clean(p); ; {
		if fi, err := os.Stat(d); err == nil && os.SameFile(rfi, fi) {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// overlaps reports whether two directories are the same or one contains
// the other, after resolving links and junctions, by name or identity.
// An unresolvable path is an error.
func overlaps(a, b string) (bool, error) {
	ra, err := resolveLinks(a)
	if err != nil {
		return false, err
	}
	rb, err := resolveLinks(b)
	if err != nil {
		return false, err
	}
	for _, x := range [][2]string{{a, b}, {ra, rb}, {a, rb}, {ra, b}} {
		if lexWithin(x[0], x[1]) || lexWithin(x[1], x[0]) {
			return true, nil
		}
	}
	return identityWithin(ra, rb) || identityWithin(rb, ra), nil
}
