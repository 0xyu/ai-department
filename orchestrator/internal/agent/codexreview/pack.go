package codexreview

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/apidiff"
	"github.com/0xyu/aco/internal/prompt"
)

// Context packs (§10.3, §11.6). The pack is the reviewer's only input
// besides the read-only repository: rules, the material under review, and
// the orchestrator's authoritative results. It is capped (§16.3): what does
// not fit is written to the run's artifact directory, which the reviewer
// may read, and the pack names the file.

// Store writes a pack attachment (run-relative path) and returns its
// absolute path for the reviewer.
type Store func(rel string, data []byte) (string, error)

// Pack is a built context pack.
type Pack struct {
	Text     string
	Template string // template name
	// Attachments are the run-relative paths written for this pack.
	Attachments []string
}

// ErrPackTooLarge reports a pack that does not fit even after moving
// everything movable out of it.
var ErrPackTooLarge = errors.New("codexreview: the context pack exceeds the size cap even after moving content to files")

// BuildPlanPack builds a plan review pack. prefix is the review's
// artifact directory (run-relative), artifactDir its absolute run
// directory (the reviewer may read below it).
func BuildPlanPack(req agent.PlanReviewRequest, artifactDir, prefix string, maxBytes int, store Store) (*Pack, error) {
	d := prompt.PlanReviewPack{
		Round: req.Round, ArtifactDir: artifactDir, Requirements: req.Requirements,
		Files: append([]agent.PackFile{}, req.PackageFiles...), ReqIDs: req.ReqIDs,
		RepoOverview: req.RepoOverview, AgentsMD: req.AgentsMD, CheckResults: req.CheckResults, Previous: req.Previous,
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	name := prompt.PlanReview
	if req.Round > 1 && len(req.Previous) > 0 {
		name = prompt.PlanReReview
	}
	p := &Pack{Template: name}
	text, err := prompt.Render(name, d)
	if err != nil {
		return nil, err
	}
	// Too large: move the largest plan files (then the overview) to files.
	for len(text) > maxBytes {
		big := -1
		for i, f := range d.Files {
			if big < 0 || len(f.Content) > len(d.Files[big].Content) {
				big = i
			}
		}
		switch {
		case big >= 0 && len(d.Files[big].Content) > 256:
			f := d.Files[big]
			rel := prefix + "/package/" + safeRel(f.Path)
			abs, err := store(rel, []byte(f.Content))
			if err != nil {
				return nil, err
			}
			p.Attachments = append(p.Attachments, rel)
			d.Omitted = append(d.Omitted, prompt.OmittedFile{What: "plan file " + f.Path, Path: abs})
			d.Files = append(d.Files[:big], d.Files[big+1:]...)
		case len(d.RepoOverview) > 256:
			rel := prefix + "/repository-overview.txt"
			abs, err := store(rel, []byte(d.RepoOverview))
			if err != nil {
				return nil, err
			}
			p.Attachments = append(p.Attachments, rel)
			d.Omitted = append(d.Omitted, prompt.OmittedFile{What: "repository overview", Path: abs})
			d.RepoOverview = "(moved to a file; see the note in the plan package section)"
		default:
			return nil, ErrPackTooLarge
		}
		if text, err = prompt.Render(name, d); err != nil {
			return nil, err
		}
	}
	p.Text = text
	return p, nil
}

var unsafeRel = regexp.MustCompile(`[^A-Za-z0-9._/-]`)

func safeRel(p string) string {
	p = unsafeRel.ReplaceAllString(strings.ReplaceAll(p, `\`, "/"), "_")
	p = strings.ReplaceAll(p, "..", "_")
	return strings.TrimLeft(p, "/")
}

// codeTemplate picks the template of a code review.
func codeTemplate(req agent.CodeReviewRequest) string {
	switch {
	case req.Kind == agent.ReviewFinal:
		return prompt.FinalReview
	case req.Round > 1 && len(req.Ledger) > 0:
		return prompt.CodeReReview
	}
	return prompt.CodeReview
}

// movable are sections that may be shortened (largest first) when the
// pack is still too large after the diffs; the plan, the phase
// specification, and AGENTS.md always stay verbatim.
type movable struct {
	what string
	file string
	ptr  *string
}

// BuildCodePack builds a phase, re-review, or final review pack.
func BuildCodePack(req agent.CodeReviewRequest, artifactDir, prefix string, maxBytes int, store Store) (*Pack, error) {
	files := apidiff.Parse(req.Diff)
	changes := apidiff.Extract(files)
	d := prompt.CodeReviewPack{
		Phase: req.Phase, Attempt: req.Attempt, Round: req.Round, ArtifactDir: artifactDir,
		PlanOverview: req.PlanOverview, Phases: req.Phases, PhaseSpec: req.PhaseSpec, Requirements: req.Requirements,
		AgentsMD: req.AgentsMD, ACIDs: req.ACIDs,
		DiffStat: req.DiffStat, Diff: req.Diff, IncrementalDiff: req.IncrementalDiff,
		GateResults: req.GateResults, Baseline: req.Baseline, TestChanges: req.TestChanges, Regressions: req.Regressions,
		Security: req.Security, ChangedAPIs: apidiff.Render(changes), Importers: req.Importers, ImplReport: req.ImplReport,
		Ledger: req.Ledger,
	}
	if d.DiffStat == "" {
		d.DiffStat = diffStat(files)
	}
	name := codeTemplate(req)
	p := &Pack{Template: name}
	render := func() (string, error) { return prompt.Render(name, d) }
	text, err := render()
	if err != nil {
		return nil, err
	}
	if len(text) <= maxBytes {
		p.Text = text
		return p, nil
	}
	put := func(rel string, data string) (string, error) {
		abs, err := store(rel, []byte(data))
		if err == nil {
			p.Attachments = append(p.Attachments, rel)
		}
		return abs, err
	}
	// 1. The full patch goes to a file; the pack keeps the highest-risk hunks.
	if d.FullPatchPath, err = put(prefix+"/full.patch", req.Diff); err != nil {
		return nil, err
	}
	d.DiffPartial, d.Diff = true, ""
	if d.IncrementalDiff != "" {
		if d.IncrementalPath, err = put(prefix+"/incremental.patch", req.IncrementalDiff); err != nil {
			return nil, err
		}
		d.IncrementalDiff = ""
	}
	// 2. Shorten the largest other sections until the rest fits in half the cap.
	sections := []movable{
		{"gate results", "gate-results.txt", &d.GateResults}, {"implementation report", "implementation-report.txt", &d.ImplReport},
		{"security data", "security.txt", &d.Security}, {"baseline comparison", "baseline.txt", &d.Baseline},
		{"test changes", "test-changes.txt", &d.TestChanges}, {"importers", "importers.txt", &d.Importers},
		{"regressions", "regressions.txt", &d.Regressions}, {"changed public APIs", "changed-apis.txt", &d.ChangedAPIs},
		{"requirements", "requirements.txt", &d.Requirements},
	}
	const keep = 8 << 10
	for {
		if text, err = render(); err != nil {
			return nil, err
		}
		if len(text) <= maxBytes/2 {
			break
		}
		big := -1
		for i, s := range sections {
			if len(*s.ptr) > keep && (big < 0 || len(*s.ptr) > len(*sections[big].ptr)) {
				big = i
			}
		}
		if big < 0 {
			break
		}
		s := sections[big]
		abs, err := put(prefix+"/context/"+s.file, *s.ptr)
		if err != nil {
			return nil, err
		}
		*s.ptr = (*s.ptr)[:keep] + "\n… (shortened)"
		d.Omitted = append(d.Omitted, prompt.OmittedFile{What: s.what, Path: abs})
	}
	// 3. Fill the remaining room with hunks, highest risk first.
	budget := maxBytes - len(text) - 1024
	d.Diff = selectHunks(files, changes, budget)
	if text, err = render(); err != nil {
		return nil, err
	}
	if len(text) > maxBytes {
		return nil, ErrPackTooLarge
	}
	p.Text = text
	return p, nil
}

// diffStat is a simple stat when the engine gave none.
func diffStat(files []apidiff.File) string {
	var b strings.Builder
	add, del := 0, 0
	for _, f := range files {
		a, r := 0, 0
		for _, h := range f.Hunks {
			a += h.Added()
			r += h.Removed()
		}
		add, del = add+a, del+r
		if f.Binary {
			b.WriteString(fmt.Sprintf(" %s | binary\n", f.Path()))
			continue
		}
		b.WriteString(fmt.Sprintf(" %s | +%d -%d\n", f.Path(), a, r))
	}
	b.WriteString(fmt.Sprintf(" %d files changed, %d insertions(+), %d deletions(-)\n", len(files), add, del))
	return b.String()
}

var (
	riskyPath = regexp.MustCompile(`(?i)auth|crypt|secret|token|passw|credential|session|login|sql|db|database|query|exec|shell|cmd|command|http|handler|router|api|upload|download|path|file|perm|policy|sandbox|guard|security|sanitiz|escape|template|serializ|parse`)
	riskyCode = regexp.MustCompile(`(?i)exec\.Command|os/exec|subprocess|shell\s*=\s*True|\beval\(|\bexec\(|child_process|Query(Row|Context)?\(|Exec(Context)?\(|"(SELECT|INSERT|UPDATE|DELETE) |Sprintf\(\s*"(SELECT|INSERT|UPDATE|DELETE)|innerHTML|dangerouslySetInnerHTML|password|secret|token|filepath\.Join|os\.(Open|Create|Remove|WriteFile|ReadFile)|ioutil\.|unsafe\.|crypto/|math/rand|InsecureSkipVerify|http\.(Get|Post|ListenAndServe)|chmod|0o?777|pickle|yaml\.load\(|deserializ`)
	lowValue  = regexp.MustCompile(`(?i)(^|/)(go\.sum|package-lock\.json|yarn\.lock|pnpm-lock\.yaml|poetry\.lock|Cargo\.lock|composer\.lock)$|(^|/)vendor/|(^|/)node_modules/|\.pb\.go$|_gen\.go$|\.min\.(js|css)$|\.snap$`)
	testFile  = regexp.MustCompile(`(?i)_test\.go$|(^|/)tests?/|\.test\.|\.spec\.|(^|/)test_[^/]*\.py$|_test\.py$`)
	docFile   = regexp.MustCompile(`(?i)\.(md|txt|rst|adoc)$`)
	assertRE  = regexp.MustCompile(`(?i)assert|expect\(|t\.(Error|Fatal)|require\.|should`)
)

type scoredHunk struct {
	file  int
	hunk  int
	score int
	size  int
}

// hunkScore rates a hunk's review risk.
func hunkScore(f *apidiff.File, h *apidiff.Hunk, apiFiles map[string]bool) int {
	p := f.Path()
	s := 0
	switch {
	case lowValue.MatchString(p):
		s -= 10
	case docFile.MatchString(p):
		s += 0
	case testFile.MatchString(p):
		s += 2
		for _, l := range h.Lines {
			if strings.HasPrefix(l, "-") && assertRE.MatchString(l) {
				s += 4 // removed or weakened assertions (anti-gaming)
				break
			}
		}
	default:
		s += 4
	}
	if riskyPath.MatchString(path.Base(p)) || riskyPath.MatchString(path.Dir(p)) {
		s += 3
	}
	hits := 0
	for _, l := range h.Lines {
		if strings.HasPrefix(l, "+") && riskyCode.MatchString(l) {
			hits++
		}
	}
	s += min(hits, 3) * 2
	if apiFiles[p] {
		s += 2
	}
	if f.OldPath == "/dev/null" || f.NewPath == "/dev/null" {
		s++
	}
	return s
}

// selectHunks returns the highest-risk hunks that fit budget bytes, in
// diff order, each under its file header.
func selectHunks(files []apidiff.File, changes []apidiff.Change, budget int) string {
	apiFiles := map[string]bool{}
	for _, c := range changes {
		apiFiles[c.File] = true
	}
	var all []scoredHunk
	for fi := range files {
		for hi := range files[fi].Hunks {
			h := &files[fi].Hunks[hi]
			all = append(all, scoredHunk{file: fi, hunk: hi, score: hunkScore(&files[fi], h, apiFiles), size: len(h.Text())})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].size < all[j].size
	})
	chosen := map[[2]int]bool{}
	headerCost := map[int]bool{}
	used := 0
	for _, c := range all {
		cost := c.size
		if !headerCost[c.file] {
			cost += len(files[c.file].Header) + 1
		}
		if used+cost > budget {
			continue
		}
		used += cost
		headerCost[c.file] = true
		chosen[[2]int{c.file, c.hunk}] = true
	}
	var b strings.Builder
	omitted := 0
	for fi := range files {
		wrote := false
		for hi := range files[fi].Hunks {
			if !chosen[[2]int{fi, hi}] {
				omitted++
				continue
			}
			if !wrote {
				b.WriteString(files[fi].Header + "\n")
				wrote = true
			}
			b.WriteString(files[fi].Hunks[hi].Text())
		}
	}
	if omitted > 0 {
		b.WriteString(fmt.Sprintf("\n(%d lower-risk hunks omitted; read the full patch)\n", omitted))
	}
	return b.String()
}
