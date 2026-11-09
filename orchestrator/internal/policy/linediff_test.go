package policy

import (
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/gitops"
)

func texts(ls []Line) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Text)
	}
	return out
}

func TestDiffLines(t *testing.T) {
	cases := []struct {
		name, old, new string
		added, removed []Line
	}{
		{"new file", "", "a\nb\n", []Line{{1, "a"}, {2, "b"}}, nil},
		{"deleted content", "a\nb\n", "", nil, []Line{{1, "a"}, {2, "b"}}},
		{"unchanged", "a\nb\n", "a\nb\n", nil, nil},
		{"insert middle", "a\nc\n", "a\nb\nc\n", []Line{{2, "b"}}, nil},
		{"replace", "a\nb\nc\n", "a\nB\nc\n", []Line{{2, "B"}}, []Line{{2, "b"}}},
		{"append no newline", "a\n", "a\nb", []Line{{2, "b"}}, nil},
		{"crlf", "a\r\n", "a\r\nt.Skip()\r\n", []Line{{2, "t.Skip()"}}, nil},
		{"moved block", "x\ny\nz\n", "z\nx\ny\n", []Line{{1, "z"}}, []Line{{3, "z"}}},
		{"duplicate lines", "a\na\n", "a\na\na\n", []Line{{3, "a"}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			added, removed := DiffLines([]byte(c.old), []byte(c.new))
			if !slices.Equal(added, c.added) || !slices.Equal(removed, c.removed) {
				t.Fatalf("added %v removed %v", added, removed)
			}
		})
	}
}

// TestDiffLinesIsComplete checks on random edits that applying the result
// to the old lines gives exactly the new lines: no added line is dropped.
func TestDiffLinesIsComplete(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		var a []string
		for i := 0; i < rng.Intn(40); i++ {
			a = append(a, string(rune('a'+rng.Intn(5))))
		}
		b := slices.Clone(a)
		for e := 0; e < rng.Intn(15); e++ {
			pos := 0
			if len(b) > 0 {
				pos = rng.Intn(len(b))
			}
			if rng.Intn(2) == 0 && len(b) > 0 {
				b = slices.Delete(b, pos, pos+1)
			} else {
				b = slices.Insert(b, pos, string(rune('a'+rng.Intn(6))))
			}
		}
		join := func(s []string) []byte {
			if len(s) == 0 {
				return nil
			}
			return []byte(strings.Join(s, "\n") + "\n")
		}
		added, removed := DiffLines(join(a), join(b))
		del := map[int]bool{}
		for _, r := range removed {
			if a[r.No-1] != r.Text {
				t.Fatal("removed line numbering")
			}
			del[r.No-1] = true
		}
		ins := map[int]string{}
		for _, l := range added {
			if b[l.No-1] != l.Text {
				t.Fatal("added line numbering")
			}
			ins[l.No-1] = l.Text
		}
		// Kept old lines, in order, must fill exactly the non-added slots of b.
		var kept []string
		for i, x := range a {
			if !del[i] {
				kept = append(kept, x)
			}
		}
		var rebuilt []string
		k := 0
		for i := range b {
			if txt, ok := ins[i]; ok {
				rebuilt = append(rebuilt, txt)
			} else if k < len(kept) {
				rebuilt = append(rebuilt, kept[k])
				k++
			}
		}
		if k != len(kept) || !slices.Equal(rebuilt, b) {
			t.Fatalf("edit script does not rebuild the new version\na=%v\nb=%v\nadded=%v removed=%v", a, b, texts(added), texts(removed))
		}
	}
}

// TestDiffLinesBeyondEditLimit: past maxEdits every changed line counts as
// added, so nothing is hidden.
func TestDiffLinesBeyondEditLimit(t *testing.T) {
	var a, b []string
	for i := 0; i < 2*maxEdits; i++ {
		a = append(a, "old line "+string(rune('a'+i%26)))
		b = append(b, "new line "+string(rune('a'+i%26)))
	}
	b[len(b)/2] = `key := "AKIAIOSFODNN7EXAMPLE"`
	added, _ := DiffLines([]byte(strings.Join(a, "\n")), []byte(strings.Join(b, "\n")))
	if len(added) != len(b) {
		t.Fatalf("got %d added lines, want %d", len(added), len(b))
	}
	r := Check(strictConfig(), []File{{Status: gitops.Modified, Path: "internal/gitops/big.go", Added: added}, testFile})
	requireFinding(t, r, CodeSecret, "internal/gitops/big.go", true)
}

func TestLongLineIsScanned(t *testing.T) {
	long := strings.Repeat("x", 1<<20) // far beyond bufio.Scanner's default 64 KiB
	newContent := "package p\n// " + long + "\nfunc f(t *T) { t.Skip() }\nconst k = \"AKIAIOSFODNN7EXAMPLE\"\n"
	added, _ := DiffLines([]byte("package p\n"), []byte(newContent))
	if len(added) != 3 || added[1].No != 3 || added[2].No != 4 {
		t.Fatalf("added lines %d", len(added))
	}
}

func TestUnscannedFileBlocks(t *testing.T) {
	r := Check(strictConfig(), []File{{Status: gitops.Modified, Path: "internal/gitops/huge.txt", Unscanned: true}, testFile})
	requireFinding(t, r, CodeScanIncomplete, "internal/gitops/huge.txt", true)
}
