package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// bom is the UTF-8 byte order mark (written as bytes so no editor turns it
// into a literal character).
const bom = "\xef\xbb\xbf"

// NormalizeText strips a leading UTF-8 BOM and turns CRLF and lone CR line
// endings into LF, so the same text hashes the same on every OS.
func NormalizeText(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte(bom))
	if bytes.IndexByte(b, '\r') < 0 {
		return b
	}
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
}

// HashText returns "sha256:<hex>" of the normalized text.
func HashText(b []byte) string {
	sum := sha256.Sum256(NormalizeText(b))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// combinedHash hashes a file list in the sha256sum layout
// ("<hash>  <path>\n", sorted by path).
func combinedHash(files []LockedFile) string {
	sorted := append([]LockedFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		h.Write([]byte(f.SHA256 + "  " + f.Path + "\n"))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// srcLine is one source line.
type srcLine struct {
	text  string // without the line break (BOM stripped on line 1)
	start int    // byte offset of the line start
	end   int    // byte offset after the line break
	num   int    // 1-based line number
}

// splitLines splits src on LF, CRLF, and CR, keeping byte offsets so raw
// sections can be sliced out of the original bytes.
func splitLines(src []byte) []srcLine {
	var out []srcLine
	for i, num := 0, 1; i < len(src); num++ {
		j := i
		for j < len(src) && src[j] != '\n' && src[j] != '\r' {
			j++
		}
		end := j
		if j < len(src) {
			end = j + 1
			if src[j] == '\r' && j+1 < len(src) && src[j+1] == '\n' {
				end = j + 2
			}
		}
		text := string(src[i:j])
		if num == 1 {
			text = strings.TrimPrefix(text, bom)
		}
		out = append(out, srcLine{text: text, start: i, end: end, num: num})
		i = end
	}
	return out
}

// mdDoc is a Markdown file split into lines, with fenced code marked.
type mdDoc struct {
	src      []byte
	lines    []srcLine
	inFence  []bool // line is a fence delimiter or inside a fenced block
	headings []heading
}

type heading struct {
	level int
	text  string
	idx   int // line index
}

var (
	fenceRE   = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	headingRE = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)
	closingRE = regexp.MustCompile(`[ \t]+#+$`)
)

func parseMarkdown(src []byte) *mdDoc {
	d := &mdDoc{src: src, lines: splitLines(src)}
	d.inFence = make([]bool, len(d.lines))
	var fenceChar byte
	fenceLen := 0
	for i, l := range d.lines {
		if fenceLen > 0 {
			d.inFence[i] = true
			if m := fenceRE.FindStringSubmatch(l.text); m != nil && m[1][0] == fenceChar && len(m[1]) >= fenceLen && strings.TrimSpace(m[2]) == "" {
				fenceLen = 0
			}
			continue
		}
		if m := fenceRE.FindStringSubmatch(l.text); m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`")) {
			fenceChar, fenceLen = m[1][0], len(m[1])
			d.inFence[i] = true
			continue
		}
		if m := headingRE.FindStringSubmatch(l.text); m != nil {
			text := closingRE.ReplaceAllString(m[2], "")
			if strings.Trim(text, "#") == "" {
				text = ""
			}
			d.headings = append(d.headings, heading{level: len(m[1]), text: strings.TrimSpace(text), idx: i})
		}
	}
	return d
}

// headingAt returns the heading on line idx, if any.
func (d *mdDoc) headingAt(idx int) (heading, bool) {
	i := sort.Search(len(d.headings), func(k int) bool { return d.headings[k].idx >= idx })
	if i < len(d.headings) && d.headings[i].idx == idx {
		return d.headings[i], true
	}
	return heading{}, false
}

// sectionEnd returns the line index where the section opened by heading h
// ends: the next heading of the same or a higher level, or limit.
func (d *mdDoc) sectionEnd(h heading, limit int) int {
	for _, o := range d.headings {
		if o.idx > h.idx && o.idx < limit && o.level <= h.level {
			return o.idx
		}
	}
	return limit
}

// trimEnd moves end back over blank lines and thematic breaks.
func (d *mdDoc) trimEnd(from, end int) int {
	for end > from {
		t := strings.TrimSpace(d.lines[end-1].text)
		if t == "" || (!d.inFence[end-1] && isThematicBreak(t)) {
			end--
			continue
		}
		break
	}
	return end
}

func isThematicBreak(t string) bool {
	s := strings.ReplaceAll(t, " ", "")
	if len(s) < 3 {
		return false
	}
	return strings.Trim(s, "-") == "" || strings.Trim(s, "*") == "" || strings.Trim(s, "_") == ""
}

// raw returns the original bytes of lines [from, to).
func (d *mdDoc) raw(from, to int) string {
	if from >= to {
		return ""
	}
	return string(d.src[d.lines[from].start:d.lines[to-1].end])
}

// text returns lines [from, to) joined with LF, right-trimmed, with leading
// and trailing blank lines removed.
func (d *mdDoc) text(from, to int) string {
	var parts []string
	for i := from; i < to; i++ {
		parts = append(parts, strings.TrimRight(d.lines[i].text, " \t"))
	}
	return strings.Trim(strings.Join(parts, "\n"), "\n")
}

// item is one top-level list item or paragraph of a section.
type item struct {
	text string
	line int
}

var listRE = regexp.MustCompile(`^([ \t]*)([-*+]|\d{1,9}[.)])([ \t]+|$)(.*)$`)

func indentWidth(s string) int {
	w := 0
	for _, r := range s {
		if r == '\t' {
			w += 4 - w%4
		} else {
			w++
		}
	}
	return w
}

func leadingWS(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

// dedent removes up to n columns of leading whitespace.
func dedent(s string, n int) string {
	w := 0
	for i, r := range s {
		if w >= n || (r != ' ' && r != '\t') {
			return s[i:]
		}
		if r == '\t' {
			w += 4 - w%4
		} else {
			w++
		}
	}
	return ""
}

// blocks splits lines [from, to) into top-level list items and, if the
// section has no list, paragraphs. Nested lists and continuation lines
// belong to the enclosing item; their indentation relative to the item's
// content is kept. Heading lines inside the range are skipped.
func (d *mdDoc) blocks(from, to int) (items, paras []item) {
	var cur []string
	curLine, contentIndent, topIndent := 0, 0, -1
	inItem, blankBefore := false, false
	var para []string
	paraLine := 0

	flushItem := func() {
		if inItem {
			items = append(items, item{text: strings.Trim(strings.Join(cur, "\n"), "\n"), line: curLine})
		}
		cur, inItem = nil, false
	}
	flushPara := func() {
		if len(para) > 0 {
			paras = append(paras, item{text: strings.Join(para, "\n"), line: paraLine})
		}
		para = nil
	}

	for i := from; i < to; i++ {
		l := d.lines[i]
		text := strings.TrimRight(l.text, " \t")
		if d.inFence[i] {
			if inItem {
				cur = append(cur, dedent(text, contentIndent))
			} else {
				if len(para) == 0 {
					paraLine = l.num
				}
				para = append(para, text)
			}
			blankBefore = false
			continue
		}
		if _, isHeading := d.headingAt(i); isHeading {
			flushItem()
			flushPara()
			blankBefore = false
			continue
		}
		if strings.TrimSpace(text) == "" {
			if inItem {
				cur = append(cur, "")
			} else {
				flushPara()
			}
			blankBefore = true
			continue
		}
		if m := listRE.FindStringSubmatch(text); m != nil {
			ind := indentWidth(m[1])
			if topIndent < 0 || ind <= topIndent+1 || !inItem && ind < contentIndent {
				flushItem()
				flushPara()
				if topIndent < 0 || ind < topIndent {
					topIndent = ind
				}
				spaces := len(m[3])
				if spaces == 0 || spaces > 4 {
					spaces = 1
				}
				contentIndent = ind + len(m[2]) + spaces
				cur, curLine, inItem = []string{m[4]}, l.num, true
				blankBefore = false
				continue
			}
		}
		ind := indentWidth(leadingWS(text))
		switch {
		case inItem && blankBefore && ind < contentIndent:
			flushItem()
			paraLine = l.num
			para = []string{strings.TrimSpace(text)}
		case inItem:
			cur = append(cur, dedent(text, contentIndent))
		default:
			if len(para) == 0 {
				paraLine = l.num
			}
			para = append(para, strings.TrimSpace(text))
		}
		blankBefore = false
	}
	flushItem()
	flushPara()
	return items, paras
}

// entries returns the list items of a section, or its paragraphs when it
// has no list.
func (d *mdDoc) entries(from, to int) []item {
	items, paras := d.blocks(from, to)
	if len(items) > 0 {
		return items
	}
	return paras
}

// splitFrontMatter returns the YAML front matter of a Markdown file (between
// a first line "---" and a closing "---" or "..."), the line its content
// starts on, and the index of the first line after it.
func splitFrontMatter(d *mdDoc) (fm []byte, firstLine, bodyStart int, ok bool) {
	if len(d.lines) == 0 || strings.TrimRight(d.lines[0].text, " \t") != "---" {
		return nil, 0, 0, false
	}
	for i := 1; i < len(d.lines); i++ {
		t := strings.TrimRight(d.lines[i].text, " \t")
		if t == "---" || t == "..." {
			var b strings.Builder
			for _, l := range d.lines[1:i] {
				b.WriteString(l.text + "\n")
			}
			return []byte(b.String()), 2, i + 1, true
		}
	}
	return nil, 0, 0, false
}

// codeSpans extracts inline code spans and returns them with the remaining
// text (spans replaced by a space).
func codeSpans(s string) (spans []string, rest string) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '`' {
			b.WriteByte(s[i])
			i++
			continue
		}
		n := 0
		for i+n < len(s) && s[i+n] == '`' {
			n++
		}
		closeAt := -1
		for j := i + n; j < len(s); {
			if s[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(s) && s[j+m] == '`' {
				m++
			}
			if m == n {
				closeAt = j
				break
			}
			j += m
		}
		if closeAt < 0 {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		c := s[i+n : closeAt]
		if len(c) >= 2 && c[0] == ' ' && c[len(c)-1] == ' ' && strings.TrimSpace(c) != "" {
			c = c[1 : len(c)-1]
		}
		spans = append(spans, c)
		b.WriteByte(' ')
		i = closeAt + n
	}
	return spans, b.String()
}

// codeSpan renders s as an inline code span.
func codeSpan(s string) string {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") || (strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") && strings.TrimSpace(s) != "") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

// stripEmphasis removes surrounding ** / __ / * / ` markers from a token.
func stripEmphasis(s string) string {
	for {
		t := strings.TrimSpace(s)
		switch {
		case len(t) >= 4 && (strings.HasPrefix(t, "**") && strings.HasSuffix(t, "**") || strings.HasPrefix(t, "__") && strings.HasSuffix(t, "__")):
			s = t[2 : len(t)-2]
		case len(t) >= 2 && (t[0] == '*' && t[len(t)-1] == '*' || t[0] == '`' && t[len(t)-1] == '`' || t[0] == '_' && t[len(t)-1] == '_'):
			s = t[1 : len(t)-1]
		default:
			return t
		}
	}
}
