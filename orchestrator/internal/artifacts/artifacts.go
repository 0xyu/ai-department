// Package artifacts is the run artifact store: prompts, transcripts, diffs,
// gate logs, reviews, and reports of a run, under one directory per run.
//
// Every write is atomic (temporary file in the same directory, fsync,
// rename) and masked with the store's secure.Redactor, so an artifact is
// either absent or complete and never holds a registered secret or a known
// credential pattern. Paths are confined to the run directory: they are
// relative, use a small character set, and no component may be a symbolic
// link, junction, or other non-regular entry.
package artifacts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/0xyu/aco/internal/secure"
)

// MaxSize is the largest artifact accepted (64 MiB). Larger outputs (gate
// logs) are trimmed by their producer first.
const MaxSize = 64 << 20

// Errors.
var (
	ErrBadPath     = errors.New("artifacts: invalid artifact path")
	ErrNotRegular  = errors.New("artifacts: path component is a link, junction, or non-regular entry")
	ErrTooLarge    = errors.New("artifacts: artifact too large")
	ErrWriterState = errors.New("artifacts: writer already committed or aborted")
)

// Store holds the run directories under root (<root>/runs/<run-id>/).
type Store struct {
	root string
	red  *secure.Redactor
}

// New returns a Store rooted at root (for example ~/.aco). red may be nil
// (credential patterns are still masked).
func New(root string, red *secure.Redactor) *Store { return &Store{root: root, red: red} }

var (
	runIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
	segmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// reserved are Windows device names, invalid as file names on every OS we
// support so a layout works everywhere.
var reserved = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

func init() {
	for i := 1; i <= 9; i++ {
		reserved[fmt.Sprintf("com%d", i)] = true
		reserved[fmt.Sprintf("lpt%d", i)] = true
	}
}

// CheckPath validates an artifact path: slash-separated, relative, 1-16
// segments of [A-Za-z0-9_.-] that do not start with "." or "-", are not "."
// or "..", and are not Windows device names.
func CheckPath(rel string) error {
	if rel == "" || path.Clean(rel) != rel || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("%w: %q", ErrBadPath, rel)
	}
	parts := strings.Split(rel, "/")
	if len(parts) > 16 {
		return fmt.Errorf("%w: too deep", ErrBadPath)
	}
	for _, p := range parts {
		base := strings.ToLower(strings.SplitN(p, ".", 2)[0])
		if !segmentRE.MatchString(p) || reserved[base] || strings.HasSuffix(p, ".") {
			return fmt.Errorf("%w: %q", ErrBadPath, rel)
		}
	}
	return nil
}

// RunDir is one run's artifact directory.
type RunDir struct {
	s     *Store
	runID string
	dir   string
}

// Run returns (creating if needed) the artifact directory of runID.
func (s *Store) Run(runID string) (*RunDir, error) {
	if !runIDRE.MatchString(runID) {
		return nil, fmt.Errorf("%w: run ID", ErrBadPath)
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return nil, fmt.Errorf("artifacts: %w", err)
	}
	if err := ensureDirs(s.root, "runs/"+runID); err != nil {
		return nil, err
	}
	return &RunDir{s: s, runID: runID, dir: filepath.Join(s.root, "runs", runID)}, nil
}

// Path returns the run directory on disk.
func (d *RunDir) Path() string { return d.dir }

// Abs returns the file path of an artifact (for handing to a CLI that reads
// it, such as Codex --output-schema).
func (d *RunDir) Abs(rel string) (string, error) {
	if err := CheckPath(rel); err != nil {
		return "", err
	}
	return filepath.Join(d.dir, filepath.FromSlash(rel)), nil
}

// realDir reports whether fi is a plain directory. On Windows, junctions
// and other reparse points report a non-directory mode type in Go.
func realDir(fi fs.FileInfo) bool { return fi.Mode().Type() == fs.ModeDir }

// ensureDirs creates each component of rel under base as a plain directory
// (0700), refusing any existing component that is not one.
func ensureDirs(base, rel string) error {
	cur := base
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("artifacts: %w", err)
			}
			if fi, err = os.Lstat(cur); err != nil {
				return fmt.Errorf("artifacts: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("artifacts: %w", err)
		}
		if !realDir(fi) {
			return fmt.Errorf("%w: %s", ErrNotRegular, part)
		}
	}
	return nil
}

// Ref identifies a written artifact.
type Ref struct {
	RunID  string
	Path   string // relative to the run directory
	Size   int64
	SHA256 string // of the stored (masked) content
}

// Write masks data and stores it at rel, atomically replacing any previous
// version.
func (d *RunDir) Write(rel string, data []byte) (Ref, error) {
	if err := CheckPath(rel); err != nil {
		return Ref{}, err
	}
	if len(data) > MaxSize {
		return Ref{}, ErrTooLarge
	}
	masked := []byte(d.s.red.String(string(data)))
	if dir := path.Dir(rel); dir != "." {
		if err := ensureDirs(d.dir, dir); err != nil {
			return Ref{}, err
		}
	}
	final := filepath.Join(d.dir, filepath.FromSlash(rel))
	if fi, err := os.Lstat(final); err == nil && fi.Mode().Type() != 0 {
		return Ref{}, fmt.Errorf("%w: %s", ErrNotRegular, rel)
	}
	if err := atomicWrite(final, masked); err != nil {
		return Ref{}, err
	}
	sum := sha256.Sum256(masked)
	return Ref{RunID: d.runID, Path: rel, Size: int64(len(masked)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func atomicWrite(final string, data []byte) error {
	dir := filepath.Dir(final)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	if err := os.Rename(name, final); err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	ok = true
	syncDir(dir)
	return nil
}

// syncDir makes the rename durable where the OS supports syncing a
// directory (not Windows).
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if f, err := os.Open(dir); err == nil {
		f.Sync()
		f.Close()
	}
}

// Read returns an artifact's content.
func (d *RunDir) Read(rel string) ([]byte, error) {
	if err := CheckPath(rel); err != nil {
		return nil, err
	}
	cur := d.dir
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, fmt.Errorf("artifacts: %w", err)
		}
		last := i == len(parts)-1
		if (last && fi.Mode().Type() != 0) || (!last && !realDir(fi)) {
			return nil, fmt.Errorf("%w: %s", ErrNotRegular, rel)
		}
	}
	return os.ReadFile(cur)
}

// Writer buffers an artifact written in pieces (a streamed transcript) and
// stores it on Commit. Masking runs over the whole content, so a secret
// split across two writes is still masked.
type Writer struct {
	d    *RunDir
	rel  string
	buf  bytes.Buffer
	done bool
}

// Create returns a Writer for rel.
func (d *RunDir) Create(rel string) (*Writer, error) {
	if err := CheckPath(rel); err != nil {
		return nil, err
	}
	return &Writer{d: d, rel: rel}, nil
}

// Write appends p.
func (w *Writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, ErrWriterState
	}
	if w.buf.Len()+len(p) > MaxSize {
		return 0, ErrTooLarge
	}
	return w.buf.Write(p)
}

// Commit stores the buffered content atomically.
func (w *Writer) Commit() (Ref, error) {
	if w.done {
		return Ref{}, ErrWriterState
	}
	w.done = true
	ref, err := w.d.Write(w.rel, w.buf.Bytes())
	w.buf.Reset()
	return ref, err
}

// Abort discards the buffered content.
func (w *Writer) Abort() {
	w.done = true
	w.buf.Reset()
}
