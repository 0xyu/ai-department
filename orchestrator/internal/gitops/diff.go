package gitops

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Change statuses.
const (
	Added    = "A"
	Modified = "M"
	Deleted  = "D"
	Renamed  = "R"
	Copied   = "C"
	TypeChg  = "T"
)

// Change is one changed path between two trees.
type Change struct {
	Status  string // A, M, D, R, C, T
	Path    string // new path (the deleted path for D); slash-separated
	OldPath string // previous path for R and C
	OldMode string
	NewMode string
	OldBlob string
	NewBlob string
	Size    int64 // size of the new blob (0 for D)
	OldSize int64 // size of the old blob (0 for A)
	// Added, Deleted, and Binary come from git's numstat and therefore follow
	// .gitattributes (a "-diff" path is reported binary with no line counts).
	// They are for reports only; the diff policy reads blob contents itself.
	Added   int
	Deleted int
	Binary  bool
}

// Paths returns every path a change touches (old and new).
func (c Change) Paths() []string {
	if c.OldPath != "" && c.OldPath != c.Path {
		return []string{c.OldPath, c.Path}
	}
	return []string{c.Path}
}

// Changes lists the changes from tree-ish `from` to tree-ish `to`, with rename
// detection, blob sizes, line counts, and binary flags. Use WorkingTree to
// diff against the current working state: phase diff = phase base → working
// tree; incremental diff = last reviewed tree → working tree; full run diff =
// base commit → HEAD.
func (r *Repo) Changes(ctx context.Context, from, to string) ([]Change, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := r.run(ctx, call{args: []string{"diff-tree", "-r", "-z", "-M", "--raw", "--no-commit-id", "--end-of-options", from, to}})
	if err != nil {
		return nil, err
	}
	changes, err := parseRaw(raw)
	if err != nil {
		return nil, err
	}
	num, err := r.run(ctx, call{args: []string{"diff-tree", "-r", "-z", "-M", "--numstat", "--no-commit-id", "--end-of-options", from, to}})
	if err != nil {
		return nil, err
	}
	stats, err := parseNumstat(num)
	if err != nil {
		return nil, err
	}
	var blobs []string
	for i := range changes {
		c := &changes[i]
		if s, ok := stats[c.Path]; ok {
			c.Added, c.Deleted, c.Binary = s.added, s.deleted, s.binary
		}
		if c.Status != Deleted && !isZeroOID(c.NewBlob) && !strings.HasPrefix(c.NewMode, "160") {
			blobs = append(blobs, c.NewBlob)
		}
		if !isZeroOID(c.OldBlob) && !strings.HasPrefix(c.OldMode, "160") {
			blobs = append(blobs, c.OldBlob)
		}
	}
	sizes, err := r.blobSizes(ctx, blobs)
	if err != nil {
		return nil, err
	}
	for i := range changes {
		c := &changes[i]
		if c.Status != Deleted {
			c.Size = sizes[c.NewBlob]
		}
		if !isZeroOID(c.OldBlob) {
			c.OldSize = sizes[c.OldBlob]
		}
	}
	return changes, nil
}

func isZeroOID(s string) bool { return strings.Trim(s, "0") == "" }

// parseRaw parses `diff-tree -r -z --raw` output.
func parseRaw(b []byte) ([]Change, error) {
	f := splitNUL(b)
	var out []Change
	for i := 0; i < len(f); {
		meta := f[i]
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("gitops: unexpected raw diff record")
		}
		parts := strings.Fields(meta[1:])
		if len(parts) != 5 || i+1 >= len(f) {
			return nil, fmt.Errorf("gitops: malformed raw diff record")
		}
		c := Change{OldMode: parts[0], NewMode: parts[1], OldBlob: parts[2], NewBlob: parts[3], Status: parts[4][:1]}
		i++
		if c.Status == Renamed || c.Status == Copied {
			if i+1 >= len(f) {
				return nil, fmt.Errorf("gitops: malformed rename record")
			}
			c.OldPath, c.Path = f[i], f[i+1]
			i += 2
		} else {
			c.Path = f[i]
			i++
		}
		out = append(out, c)
	}
	return out, nil
}

type numstat struct {
	added, deleted int
	binary         bool
}

// parseNumstat parses `--numstat -z` output, keyed by new path.
func parseNumstat(b []byte) (map[string]numstat, error) {
	f := splitNUL(b)
	out := map[string]numstat{}
	for i := 0; i < len(f); i++ {
		cols := strings.SplitN(f[i], "\t", 3)
		if len(cols) != 3 {
			return nil, fmt.Errorf("gitops: malformed numstat record")
		}
		var s numstat
		if cols[0] == "-" && cols[1] == "-" {
			s.binary = true
		} else {
			s.added, _ = strconv.Atoi(cols[0])
			s.deleted, _ = strconv.Atoi(cols[1])
		}
		path := cols[2]
		if path == "" { // rename: old and new path follow as separate fields
			if i+2 >= len(f) {
				return nil, fmt.Errorf("gitops: malformed numstat rename")
			}
			path = f[i+2]
			i += 2
		}
		out[path] = s
	}
	return out, nil
}

func splitNUL(b []byte) []string {
	b = bytes.TrimSuffix(b, []byte{0})
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

// blobSizes returns the size of each blob.
func (r *Repo) blobSizes(ctx context.Context, blobs []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(blobs) == 0 {
		return out, nil
	}
	in := strings.Join(blobs, "\n") + "\n"
	b, err := r.run(ctx, call{args: []string{"cat-file", "--batch-check=%(objectname) %(objectsize)"}, stdin: strings.NewReader(in)})
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) == 2 {
			n, err := strconv.ParseInt(parts[1], 10, 64)
			if err == nil {
				out[parts[0]] = n
			}
		}
	}
	return out, sc.Err()
}

// Patch returns the unified diff from `from` to `to` with `context` lines of
// context (rename detection on, external diff and textconv off, fixed a/ b/
// prefixes). Binary files appear as "Binary files … differ".
func (r *Repo) Patch(ctx context.Context, from, to string, context int) ([]byte, error) {
	if context < 0 {
		context = 3
	}
	return r.run(ctx, call{args: []string{
		"diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M",
		"--src-prefix=a/", "--dst-prefix=b/", "-U" + strconv.Itoa(context),
		"--end-of-options", from, to,
	}})
}

// DiffStat returns the human-readable `git diff --stat` summary.
func (r *Repo) DiffStat(ctx context.Context, from, to string) (string, error) {
	b, err := r.run(ctx, call{args: []string{"diff", "--no-ext-diff", "--no-color", "-M", "--stat=200", "--end-of-options", from, to}})
	return string(b), err
}

// ChangedPaths returns the sorted set of paths (old and new) touched between
// two tree-ishes.
func (r *Repo) ChangedPaths(ctx context.Context, from, to string) ([]string, error) {
	b, err := r.run(ctx, call{args: []string{"diff-tree", "-r", "-z", "--no-renames", "--name-only", "--no-commit-id", "--end-of-options", from, to}})
	if err != nil {
		return nil, err
	}
	return splitNUL(b), nil
}

// IsSubmodule reports whether a mode is a gitlink (submodule).
func IsSubmodule(mode string) bool { return strings.HasPrefix(mode, "160") }

// ReadBlob returns the raw content of a blob exactly as stored: no
// .gitattributes conversion, filter, or textconv is applied.
func (r *Repo) ReadBlob(ctx context.Context, blob string) ([]byte, error) {
	if isZeroOID(blob) || strings.HasPrefix(blob, "-") {
		return nil, fmt.Errorf("gitops: invalid blob id")
	}
	return r.run(ctx, call{args: []string{"cat-file", "blob", blob}})
}
