package plan

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNotContained reports a protected path component that is a symbolic
// link, a Windows junction or other reparse point, or another non-regular
// entry. Following it would let a repository point ACO at files outside the
// repository.
var ErrNotContained = errors.New("path leaves the repository (symbolic link, junction, or non-regular entry)")

// ContainmentError names the offending path (slash-separated, relative to the
// repository root).
type ContainmentError struct {
	Path string
}

func (e *ContainmentError) Error() string { return e.Path + ": " + ErrNotContained.Error() }

// Unwrap lets errors.Is(err, ErrNotContained) match.
func (e *ContainmentError) Unwrap() error { return ErrNotContained }

// realDir reports whether fi is a plain directory (no link or reparse bits).
func realDir(fi fs.FileInfo) bool { return fi.Mode().Type() == fs.ModeDir }

// realFile reports whether fi is a plain regular file.
func realFile(fi fs.FileInfo) bool { return fi.Mode().Type() == 0 }

// checkContained checks every component of rel (slash-separated, relative to
// root) with Lstat, without following anything: all but the last must be
// plain directories; the last must be a plain directory, or a plain file when
// lastIsFile. The root itself is trusted (it is what the caller named). A
// missing component returns an fs.ErrNotExist error.
func checkContained(root, rel string, lastIsFile bool) error {
	parts := strings.Split(rel, "/")
	cur := root
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		last := i == len(parts)-1
		ok := realDir(fi)
		if last && lastIsFile {
			ok = realFile(fi)
		}
		if !ok {
			return &ContainmentError{Path: strings.Join(parts[:i+1], "/")}
		}
	}
	return nil
}

// checkPackageDir checks <root>/.aco and <root>/.aco/plan.
func checkPackageDir(root string) error { return checkContained(root, PackageDir, false) }

// checkManifest checks the lock path: the package directory must be
// contained, and plan.lock.json, if present, must be a plain file. It returns
// whether the lock exists.
func checkManifest(root string) (bool, error) {
	if err := checkPackageDir(root); err != nil {
		return false, err
	}
	err := checkContained(root, PackageDir+"/"+LockFileName, true)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// readRegular reads p only if it is a plain regular file (it never follows a
// link). rel names p in a ContainmentError.
func readRegular(p, rel string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !realFile(fi) {
		return nil, &ContainmentError{Path: rel}
	}
	b, err := readFile(p)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// isPackageDir reports whether dir is <repoRoot>/.aco/plan by path (no link
// resolution, so a linked .aco cannot make an outside directory match).
func isPackageDir(dir, repoRoot string) bool {
	a, err1 := filepath.Abs(dir)
	b, err2 := filepath.Abs(filepath.Join(repoRoot, filepath.FromSlash(PackageDir)))
	if err1 != nil || err2 != nil {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func containmentDiag(err error) (Diagnostic, bool) {
	var ce *ContainmentError
	if !errors.As(err, &ce) {
		return Diagnostic{}, false
	}
	return NewDiagnostic(CodePathNotContained, SeverityError, Location{File: ce.Path, Ref: ce.Path}, ce.Path), true
}
