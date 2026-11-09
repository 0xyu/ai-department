package policy

import (
	"errors"
	"strings"
)

// A small POSIX-shell reader for the guard. It does not execute or expand
// anything; it splits a command line into simple commands (words plus
// redirections) and collects the text of command substitutions so they can
// be checked too. Anything it cannot read reliably (unbalanced quotes or
// parentheses, unterminated heredocs, too deep nesting) is an error, and the
// guard then denies the command.

var errUnparsable = errors.New("command cannot be parsed reliably")

// maxShellDepth bounds nested substitutions and "sh -c" scripts.
const maxShellDepth = 8

type redirect struct {
	op     string // ">", ">>", "<", "&>", ...
	target string
}

type simpleCmd struct {
	words     []string
	redirects []redirect
	pipeline  int // pipeline number within the script
	pipePos   int // position in its pipeline (0 = first)
}

type shellScript struct {
	cmds []simpleCmd
	subs []string // bodies of $(...), `...`, <(...), >(...)
}

type heredoc struct {
	delim     string
	stripTabs bool
}

type lexer struct {
	src      []rune
	i        int
	out      shellScript
	cur      simpleCmd
	word     strings.Builder
	inWord   bool
	pipeline int
	pipePos  int
	pending  []heredoc
}

func parseShell(src string) (*shellScript, error) {
	lx := &lexer{src: []rune(src)}
	if err := lx.run(); err != nil {
		return nil, err
	}
	return &lx.out, nil
}

func (lx *lexer) endWord() {
	if lx.inWord {
		lx.cur.words = append(lx.cur.words, lx.word.String())
		lx.word.Reset()
		lx.inWord = false
	}
}

// endCmd finishes the current simple command. newPipeline starts a new
// pipeline (";", "&&", "||", "&", newline); otherwise the next command is
// the next stage of the same pipeline ("|").
func (lx *lexer) endCmd(newPipeline bool) {
	lx.endWord()
	if len(lx.cur.words) > 0 || len(lx.cur.redirects) > 0 {
		lx.cur.pipeline, lx.cur.pipePos = lx.pipeline, lx.pipePos
		lx.out.cmds = append(lx.out.cmds, lx.cur)
		lx.pipePos++
	}
	lx.cur = simpleCmd{}
	if newPipeline {
		lx.pipeline++
		lx.pipePos = 0
	}
}

func (lx *lexer) peek(off int) rune {
	if lx.i+off < len(lx.src) {
		return lx.src[lx.i+off]
	}
	return 0
}

func (lx *lexer) run() error {
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			lx.endWord()
			lx.i++
		case c == '\n':
			lx.endCmd(true)
			lx.i++
			if err := lx.skipHeredocs(); err != nil {
				return err
			}
		case c == '#' && !lx.inWord:
			for lx.i < len(lx.src) && lx.src[lx.i] != '\n' {
				lx.i++
			}
		case c == ';':
			lx.endCmd(true)
			lx.i++
		case c == '&':
			if lx.peek(1) == '>' { // &> and &>>
				lx.endWord()
				if err := lx.redirect(); err != nil {
					return err
				}
				continue
			}
			lx.endCmd(true)
			lx.i++
			if lx.peek(0) == '&' {
				lx.i++
			}
		case c == '|':
			if lx.peek(1) == '|' {
				lx.endCmd(true)
				lx.i += 2
				continue
			}
			lx.endCmd(false)
			lx.i++
			if lx.peek(0) == '&' {
				lx.i++
			}
		case c == '(' || c == ')':
			lx.endCmd(true)
			lx.i++
		case c == '<' || c == '>':
			if lx.peek(1) == '(' { // process substitution
				body, err := lx.balanced(lx.i + 2)
				if err != nil {
					return err
				}
				lx.out.subs = append(lx.out.subs, body)
				lx.word.WriteString("$()")
				lx.inWord = true
				continue
			}
			// "2>" and similar: a word of digits right before the operator is its fd.
			if lx.inWord && isDigits(lx.word.String()) {
				lx.word.Reset()
				lx.inWord = false
			}
			lx.endWord()
			if err := lx.redirect(); err != nil {
				return err
			}
		default:
			if err := lx.wordChar(); err != nil {
				return err
			}
		}
	}
	if len(lx.pending) > 0 {
		return errUnparsable // heredoc without body
	}
	lx.endCmd(true)
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// wordChar consumes one word element: a plain character, an escape, a quoted
// string, or a substitution.
func (lx *lexer) wordChar() error {
	c := lx.src[lx.i]
	lx.inWord = true
	switch c {
	case '\\':
		if lx.i+1 >= len(lx.src) {
			lx.i++
			return nil
		}
		if lx.src[lx.i+1] == '\n' { // line continuation
			lx.i += 2
			if lx.word.Len() == 0 {
				lx.inWord = false
			}
			return nil
		}
		lx.word.WriteRune(lx.src[lx.i+1])
		lx.i += 2
	case '\'':
		end := lx.i + 1
		for end < len(lx.src) && lx.src[end] != '\'' {
			end++
		}
		if end >= len(lx.src) {
			return errUnparsable
		}
		lx.word.WriteString(string(lx.src[lx.i+1 : end]))
		lx.i = end + 1
	case '"':
		return lx.doubleQuoted()
	case '$':
		switch lx.peek(1) {
		case '(':
			body, err := lx.balanced(lx.i + 2)
			if err != nil {
				return err
			}
			lx.out.subs = append(lx.out.subs, body)
			lx.word.WriteString("$()")
		case '\'': // $'...' ANSI-C quoting: keep the raw body
			end := lx.i + 2
			for end < len(lx.src) && lx.src[end] != '\'' {
				if lx.src[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(lx.src) {
				return errUnparsable
			}
			lx.word.WriteString(string(lx.src[lx.i+2 : end]))
			lx.i = end + 1
		default:
			lx.word.WriteRune(c)
			lx.i++
		}
	case '`':
		body, end, err := lx.backtick(lx.i + 1)
		if err != nil {
			return err
		}
		lx.out.subs = append(lx.out.subs, body)
		lx.word.WriteString("$()")
		lx.i = end
	default:
		lx.word.WriteRune(c)
		lx.i++
	}
	return nil
}

func (lx *lexer) doubleQuoted() error {
	lx.i++ // opening quote
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		switch c {
		case '"':
			lx.i++
			return nil
		case '\\':
			if lx.i+1 < len(lx.src) {
				n := lx.src[lx.i+1]
				if n == '"' || n == '\\' || n == '$' || n == '`' {
					lx.word.WriteRune(n)
				} else if n != '\n' {
					lx.word.WriteRune('\\')
					lx.word.WriteRune(n)
				}
				lx.i += 2
				continue
			}
			lx.i++
		case '$':
			if lx.peek(1) == '(' {
				body, err := lx.balanced(lx.i + 2)
				if err != nil {
					return err
				}
				lx.out.subs = append(lx.out.subs, body)
				lx.word.WriteString("$()")
				continue
			}
			lx.word.WriteRune(c)
			lx.i++
		case '`':
			body, end, err := lx.backtick(lx.i + 1)
			if err != nil {
				return err
			}
			lx.out.subs = append(lx.out.subs, body)
			lx.word.WriteString("$()")
			lx.i = end
		default:
			lx.word.WriteRune(c)
			lx.i++
		}
	}
	return errUnparsable
}

// balanced returns the text between an opening "(" (just before start) and
// its matching ")", honouring quotes, and moves past the ")".
func (lx *lexer) balanced(start int) (string, error) {
	depth := 1
	for j := start; j < len(lx.src); j++ {
		switch lx.src[j] {
		case '\\':
			j++
		case '\'':
			j++
			for j < len(lx.src) && lx.src[j] != '\'' {
				j++
			}
			if j >= len(lx.src) {
				return "", errUnparsable
			}
		case '"':
			j++
			for j < len(lx.src) && lx.src[j] != '"' {
				if lx.src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(lx.src) {
				return "", errUnparsable
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				lx.i = j + 1
				return string(lx.src[start:j]), nil
			}
		}
	}
	return "", errUnparsable
}

func (lx *lexer) backtick(start int) (string, int, error) {
	var b strings.Builder
	for j := start; j < len(lx.src); j++ {
		c := lx.src[j]
		if c == '\\' && j+1 < len(lx.src) {
			b.WriteRune(lx.src[j+1])
			j++
			continue
		}
		if c == '`' {
			return b.String(), j + 1, nil
		}
		b.WriteRune(c)
	}
	return "", 0, errUnparsable
}

// redirect reads a redirection operator at lx.i and its target word.
func (lx *lexer) redirect() error {
	ops := []string{"&>>", "&>", "<<<", "<<-", "<<", "<>", "<&", "<", ">>", ">|", ">&", ">"}
	rest := string(lx.src[lx.i:min(lx.i+3, len(lx.src))])
	op := ""
	for _, o := range ops {
		if strings.HasPrefix(rest, o) {
			op = o
			break
		}
	}
	lx.i += len([]rune(op))
	for lx.i < len(lx.src) && (lx.src[lx.i] == ' ' || lx.src[lx.i] == '\t') {
		lx.i++
	}
	target, err := lx.readWord()
	if err != nil {
		return err
	}
	if target == "" {
		return errUnparsable
	}
	switch op {
	case ">&", "<&":
		if isDigits(target) || target == "-" {
			return nil // fd duplication
		}
		op = ">"
	case "<<", "<<-":
		lx.pending = append(lx.pending, heredoc{delim: target, stripTabs: op == "<<-"})
		return nil
	case "<<<":
		return nil // here-string: data
	}
	lx.cur.redirects = append(lx.cur.redirects, redirect{op: op, target: target})
	return nil
}

// readWord reads one word (with quotes and substitutions) at lx.i.
func (lx *lexer) readWord() (string, error) {
	saved, savedIn := lx.word.String(), lx.inWord
	lx.word.Reset()
	lx.inWord = false
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		if strings.ContainsRune(" \t\r\n;&|()<>", c) {
			break
		}
		if err := lx.wordChar(); err != nil {
			return "", err
		}
	}
	w := lx.word.String()
	lx.word.Reset()
	lx.word.WriteString(saved)
	lx.inWord = savedIn
	return w, nil
}

// skipHeredocs skips the bodies of heredocs opened on the line just ended.
func (lx *lexer) skipHeredocs() error {
	for len(lx.pending) > 0 {
		h := lx.pending[0]
		lx.pending = lx.pending[1:]
		found := false
		for lx.i < len(lx.src) {
			end := lx.i
			for end < len(lx.src) && lx.src[end] != '\n' {
				end++
			}
			line := strings.TrimRight(string(lx.src[lx.i:end]), "\r")
			if h.stripTabs {
				line = strings.TrimLeft(line, "\t")
			}
			lx.i = min(end+1, len(lx.src))
			if line == h.delim {
				found = true
				break
			}
		}
		if !found {
			return errUnparsable
		}
	}
	return nil
}
