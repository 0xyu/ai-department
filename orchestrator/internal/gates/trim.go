package gates

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxLogBytes is the default size of a command's log in a result.
const MaxLogBytes = 1 << 20

// failureLineRE marks lines that start a failure in common tool output.
var failureLineRE = regexp.MustCompile(`(?i)^\s*(?:--- FAIL|FAIL\b|panic:|fatal error:|FAILED\b|ERROR\b|error(?:\[[A-Z0-9]+\])?:|Error:|[EF]\s{2,}|Traceback \(most recent call last\)|AssertionError|✕|✗|×|npm ERR!|.*\bfailed\b.*:)|^\S+\.[A-Za-z0-9]+:\d+(?::\d+)?:\s|^\s*\S+\(\d+,\d+\): error`)

// FirstFailure returns the byte offset of the first line that looks like a
// failure, or -1.
func FirstFailure(log []byte) int {
	off := 0
	for off < len(log) {
		end := bytes.IndexByte(log[off:], '\n')
		line := log[off:]
		if end >= 0 {
			line = log[off : off+end]
		}
		if len(line) <= 4096 && failureLineRE.Match(line) {
			return off
		}
		if end < 0 {
			break
		}
		off += end + 1
	}
	return -1
}

// Trim bounds log to max bytes. It keeps the beginning, a block around the
// first failure (a few lines of context before it and as much as fits
// after), and the tail, with a marker for each cut. Cuts are moved to line
// boundaries where one is near, and the result is valid UTF-8.
func Trim(log []byte, max int) ([]byte, bool) {
	if max <= 0 {
		max = MaxLogBytes
	}
	if len(log) <= max {
		return log, false
	}
	const reserve = 512 // markers
	budget := max - reserve
	if budget < 64 {
		budget = max
	}
	head := budget / 8
	fail := budget / 2
	tail := budget - head - fail

	type seg struct{ a, b int }
	segs := []seg{{0, lineEnd(log, head)}}
	if f := FirstFailure(log); f >= 0 {
		start := backLines(log, f, 10)
		if f-start > fail/4 {
			start = lineStart(log, f-fail/4)
		}
		segs = append(segs, seg{start, lineEnd(log, start+fail)})
	} else {
		tail += fail
	}
	ts := lineStart(log, len(log)-tail)
	segs = append(segs, seg{ts, len(log)})

	// Merge overlapping segments (they are in order).
	merged := []seg{segs[0]}
	for _, s := range segs[1:] {
		last := &merged[len(merged)-1]
		if s.a <= last.b {
			if s.b > last.b {
				last.b = s.b
			}
			continue
		}
		merged = append(merged, s)
	}
	var out bytes.Buffer
	marker := func(n int) { out.WriteString("\n[... aco: " + strconv.Itoa(n) + " bytes trimmed ...]\n") }
	prev := 0
	for _, s := range merged {
		if s.a > prev {
			marker(s.a - prev)
		}
		out.Write(log[s.a:s.b])
		prev = s.b
	}
	if prev < len(log) {
		marker(len(log) - prev)
	}
	b := out.Bytes()
	if len(b) > max {
		b = b[:max]
	}
	return bytes.ToValidUTF8(b, []byte("�")), true
}

// lineEnd moves i forward to just after the next newline within 4 KiB.
func lineEnd(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	if i < 0 {
		return 0
	}
	lim := min(len(b), i+4096)
	if j := bytes.IndexByte(b[i:lim], '\n'); j >= 0 {
		return i + j + 1
	}
	return i
}

// lineStart moves i forward to the start of the next line within 4 KiB.
func lineStart(b []byte, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(b) {
		return len(b)
	}
	if b[i-1] == '\n' {
		return i
	}
	return lineEnd(b, i)
}

// backLines returns the start of the line n lines before the line at off.
func backLines(b []byte, off, n int) int {
	i := off
	for k := 0; k < n && i > 0; k++ {
		j := bytes.LastIndexByte(b[:i-1], '\n')
		if j < 0 {
			return 0
		}
		i = j + 1
	}
	return i
}

// --- signatures --------------------------------------------------------------

// MaxSignatures bounds the signatures kept per command.
const MaxSignatures = 200

var (
	sigNumRE   = regexp.MustCompile(`:\d+(?::\d+)?`)
	sigParenRE = regexp.MustCompile(`\(\d+,\d+\)`)
	sigTimeRE  = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ms|s|m)\b`)
	sigHexRE   = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	sigSpaceRE = regexp.MustCompile(`\s+`)
)

// Signatures extracts normalized failure lines from unstructured output:
// line and column numbers, durations, and addresses are removed so the same
// pre-existing error matches its baseline after unrelated edits.
func Signatures(log []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(log), "\n") {
		if len(line) > 4096 || !failureLineRE.MatchString(line) {
			continue
		}
		s := sigNumRE.ReplaceAllString(line, ":N")
		s = sigParenRE.ReplaceAllString(s, "(N)")
		s = sigTimeRE.ReplaceAllString(s, "T")
		s = sigHexRE.ReplaceAllString(s, "0xN")
		s = strings.TrimSpace(sigSpaceRE.ReplaceAllString(s, " "))
		if len(s) > 300 {
			s = s[:300]
			for !utf8.ValidString(s) {
				s = s[:len(s)-1]
			}
		}
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
			if len(out) >= MaxSignatures {
				break
			}
		}
	}
	return out
}

// --- environment failures ----------------------------------------------------

var (
	// notFoundRE matches a shell that could not find the program.
	notFoundRE = regexp.MustCompile(`(?m)(?:^(?:\S*/)?(?:sh|bash|dash|zsh)(?:\.exe)?: (?:\d+: |line \d+: )?[^:\s]+: (?:command not found|not found|No such file or directory)\r?$|is not recognized as an internal or external command|cannot execute: required file not found)`)
	// unreachableRE matches a service the gate needs that is not reachable.
	unreachableRE = regexp.MustCompile(`(?i)connection refused|ECONNREFUSED|could not connect to (?:server|database)|Cannot connect to the Docker daemon|is the docker daemon running|getaddrinfo (?:ENOTFOUND|EAI_AGAIN)|no such host|Temporary failure in name resolution|connect: network is unreachable`)
)

// environmentReason classifies a failed command's output. It returns "" for
// an ordinary failure.
func environmentReason(exit int, log []byte, testsFailed bool) string {
	if exit == 127 {
		return "command not found"
	}
	head := log
	if len(head) > 256<<10 {
		head = head[:256<<10]
	}
	if notFoundRE.Match(head) && !testsFailed {
		return "command not found"
	}
	if !testsFailed && unreachableRE.Match(log) {
		return "required service unreachable"
	}
	return ""
}
