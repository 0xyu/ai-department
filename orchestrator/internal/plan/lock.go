package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// LockFormat identifies the freeze manifest format.
const LockFormat = "aco.plan.lock/v1"

// Lock is the freeze manifest .aco/plan/plan.lock.json (§10.5): the hash of
// every frozen plan file and AGENTS.md, plus review metadata.
type Lock struct {
	Format        string       `json:"format"`
	PlanVersion   string       `json:"plan_version"`
	PlanHash      string       `json:"plan_hash"` // combined hash of Files
	RunID         string       `json:"run_id,omitempty"`
	ReviewerModel string       `json:"reviewer_model,omitempty"`
	ReviewRounds  int          `json:"review_rounds"`
	Verdict       string       `json:"verdict,omitempty"`
	FrozenAt      string       `json:"frozen_at,omitempty"` // RFC 3339, UTC
	Files         []LockedFile `json:"files"`               // sorted by Path
}

// LockedFile is one frozen file. Path is slash-separated, relative to the
// repository root.
type LockedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"` // HashText of the file
}

// LockMeta is the review metadata recorded at freeze time.
type LockMeta struct {
	PlanVersion   string // "v1"
	RunID         string
	ReviewerModel string
	ReviewRounds  int
	Verdict       string
	FrozenAt      time.Time
}

// ErrNotRegular reports a frozen path that is a symlink, device, or other
// non-regular file.
var ErrNotRegular = errors.New("not a regular file")

// LockPath returns the lock file path under repoRoot.
func LockPath(repoRoot string) string {
	return filepath.Join(repoRoot, filepath.FromSlash(PackageDir), LockFileName)
}

// frozenFiles lists the files a lock covers: every regular file under
// .aco/plan/ except plan.lock.json, and AGENTS.md if present. Non-regular
// files are returned separately.
func frozenFiles(repoRoot string) (files []string, irregular []string, err error) {
	// Refuse a linked .aco, .aco/plan, or plan.lock.json before walking.
	if _, err := checkManifest(repoRoot); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = nil // no package directory yet
		}
		return nil, nil, err
	}
	root := filepath.Join(repoRoot, filepath.FromSlash(PackageDir))
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == root {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(repoRoot, p)
		rel = filepath.ToSlash(rel)
		switch {
		case e.Type() == fs.ModeDir:
			return nil
		case rel == PackageDir+"/"+LockFileName:
			return nil
		case e.Type() != 0:
			irregular = append(irregular, rel) // never followed
			if e.IsDir() {
				return filepath.SkipDir
			}
		default:
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	switch name, err := agentsMDName(repoRoot); {
	case errors.Is(err, ErrNotRegular):
		irregular = append(irregular, name)
	case err != nil:
		return nil, nil, err
	case name != "":
		files = append(files, name)
	}
	sort.Strings(files)
	sort.Strings(irregular)
	return files, irregular, nil
}

// BuildLock hashes the frozen files under repoRoot.
func BuildLock(repoRoot string, meta LockMeta) (*Lock, error) {
	names, irregular, err := frozenFiles(repoRoot)
	if err != nil {
		return nil, err
	}
	if len(irregular) > 0 {
		return nil, fmt.Errorf("%s: %w", strings.Join(irregular, ", "), ErrNotRegular)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no plan files under %s", PackageDir)
	}
	l := &Lock{Format: LockFormat, PlanVersion: meta.PlanVersion, RunID: meta.RunID, ReviewerModel: meta.ReviewerModel,
		ReviewRounds: meta.ReviewRounds, Verdict: meta.Verdict}
	if l.PlanVersion == "" {
		l.PlanVersion = "v1"
	}
	if !meta.FrozenAt.IsZero() {
		l.FrozenAt = meta.FrozenAt.UTC().Format(time.RFC3339)
	}
	for _, n := range names {
		raw, err := readRegular(filepath.Join(repoRoot, filepath.FromSlash(n)), n)
		if err != nil {
			return nil, err
		}
		l.Files = append(l.Files, LockedFile{Path: n, SHA256: HashText(raw)})
	}
	l.PlanHash = combinedHash(l.Files)
	return l, nil
}

// Marshal renders the lock as indented JSON with a trailing newline.
func (l *Lock) Marshal() []byte {
	b, _ := json.MarshalIndent(l, "", "  ")
	return append(b, '\n')
}

// WriteLock writes .aco/plan/plan.lock.json under repoRoot. It refuses a
// linked .aco, .aco/plan, or existing plan.lock.json without writing.
func WriteLock(repoRoot string, l *Lock) error {
	if _, err := checkManifest(repoRoot); err != nil {
		return err
	}
	return writeFileAtomic(LockPath(repoRoot), l.Marshal())
}

var hashRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ParseLock decodes and validates a lock: known format, safe relative paths
// limited to .aco/plan/** and AGENTS.md, well-formed hashes, and a plan hash
// that matches the file list.
func ParseLock(b []byte) (*Lock, error) {
	var l Lock
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("plan.lock.json: %w", err)
	}
	if l.Format != LockFormat {
		return nil, fmt.Errorf("plan.lock.json: unsupported format %q", clip(l.Format))
	}
	seen := map[string]bool{}
	for _, f := range l.Files {
		if !safeLockPath(f.Path) {
			return nil, fmt.Errorf("plan.lock.json: path %q is not allowed", clip(f.Path))
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("plan.lock.json: duplicate path %q", f.Path)
		}
		seen[f.Path] = true
		if !hashRE.MatchString(f.SHA256) {
			return nil, fmt.Errorf("plan.lock.json: bad hash for %q", f.Path)
		}
	}
	if !hashRE.MatchString(l.PlanHash) || combinedHash(l.Files) != l.PlanHash {
		return nil, errors.New("plan.lock.json: plan_hash does not match the file list")
	}
	return &l, nil
}

func safeLockPath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || path.IsAbs(p) || path.Clean(p) != p || strings.Contains(p, ":") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return false
		}
	}
	return strings.EqualFold(p, AgentsMDName) || strings.HasPrefix(p, PackageDir+"/")
}

// ReadLock reads and validates repoRoot's plan.lock.json.
func ReadLock(repoRoot string) (*Lock, error) {
	exists, err := checkManifest(repoRoot)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &fs.PathError{Op: "open", Path: LockPath(repoRoot), Err: fs.ErrNotExist}
	}
	b, err := readRegular(LockPath(repoRoot), PackageDir+"/"+LockFileName)
	if err != nil {
		return nil, err
	}
	return ParseLock(b)
}

// Mismatch problems.
const (
	ProblemModified   = "modified"
	ProblemMissing    = "missing"
	ProblemUnexpected = "unexpected" // a frozen-area file that the lock does not list
	ProblemNotRegular = "not_regular"
)

// Mismatch is one file that differs from the lock.
type Mismatch struct {
	Path    string
	Problem string
}

// MismatchError lists every mismatching file.
type MismatchError []Mismatch

func (e MismatchError) Error() string {
	parts := make([]string, len(e))
	for i, m := range e {
		parts[i] = m.Path + " (" + m.Problem + ")"
	}
	return "frozen plan changed: " + strings.Join(parts, ", ")
}

// VerifyLock compares the files under repoRoot with the lock. It returns nil
// when they match, a MismatchError naming each differing file, a
// *ContainmentError when .aco, .aco/plan, or plan.lock.json is a link or
// junction (nothing is read through it), or an I/O error.
func VerifyLock(repoRoot string, l *Lock) error {
	names, irregular, err := frozenFiles(repoRoot)
	if err != nil {
		return err
	}
	var bad MismatchError
	present := map[string]bool{}
	for _, n := range names {
		present[n] = true
	}
	locked := map[string]bool{}
	for _, f := range l.Files {
		locked[f.Path] = true
		if !present[f.Path] {
			if containsString(irregular, f.Path) {
				bad = append(bad, Mismatch{f.Path, ProblemNotRegular})
			} else {
				bad = append(bad, Mismatch{f.Path, ProblemMissing})
			}
			continue
		}
		raw, err := readRegular(filepath.Join(repoRoot, filepath.FromSlash(f.Path)), f.Path)
		if err != nil {
			return err
		}
		if HashText(raw) != f.SHA256 {
			bad = append(bad, Mismatch{f.Path, ProblemModified})
		}
	}
	for _, n := range names {
		if !locked[n] {
			bad = append(bad, Mismatch{n, ProblemUnexpected})
		}
	}
	for _, n := range irregular {
		if !locked[n] {
			bad = append(bad, Mismatch{n, ProblemNotRegular})
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].Path < bad[j].Path })
	return bad
}

// --- AGENTS.md ---

// agentsMDName finds AGENTS.md in repoRoot (exact name preferred, else a
// case-insensitive match). It returns "" when there is none and
// ErrNotRegular (with the name) when it is not a regular file.
func agentsMDName(repoRoot string) (string, error) {
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		return "", err
	}
	name := ""
	for _, e := range entries {
		if e.Name() == AgentsMDName {
			name = e.Name()
			break
		}
		if name == "" && strings.EqualFold(e.Name(), AgentsMDName) {
			name = e.Name()
		}
	}
	if name == "" {
		return "", nil
	}
	fi, err := os.Lstat(filepath.Join(repoRoot, name))
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return name, ErrNotRegular
	}
	return name, nil
}

// DiscoverAgentsMD reads repoRoot/AGENTS.md. It returns nil, nil when the
// repository has none, and ErrNotRegular when it is a symlink or other
// non-regular file (it is never followed).
func DiscoverAgentsMD(repoRoot string) (*File, error) {
	name, err := agentsMDName(repoRoot)
	if err != nil || name == "" {
		return nil, err
	}
	raw, err := readRegular(filepath.Join(repoRoot, name), name)
	if err != nil {
		return nil, err
	}
	return &File{Path: name, Raw: raw, Hash: HashText(raw)}, nil
}
