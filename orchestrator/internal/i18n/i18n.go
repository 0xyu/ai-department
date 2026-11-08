// Package i18n holds the message catalog for all operator-facing text.
//
// Every string shown to the operator (CLI output, doctor results, error
// explanations) must be rendered through a catalog Key. Documents, prompts,
// logs, and machine fields stay in English and do not use this package.
package i18n

import (
	"fmt"
	"io"
	"strings"
)

// Lang is an operator language tag.
type Lang string

// Supported operator languages.
const (
	ZhCN Lang = "zh-CN"
	EN   Lang = "en"

	// Default is the operator language when nothing is configured.
	Default = ZhCN
)

// Langs lists all supported operator languages.
var Langs = []Lang{ZhCN, EN}

// ParseLang returns the Lang for s, or false if s is not supported.
func ParseLang(s string) (Lang, bool) {
	for _, l := range Langs {
		if strings.EqualFold(string(l), s) {
			return l, true
		}
	}
	return "", false
}

// Key identifies a catalog message. Keys are declared in keys.go.
type Key string

type message struct {
	zh string
	en string
}

func (m message) in(l Lang) string {
	if l == EN {
		return m.en
	}
	return m.zh
}

// Has reports whether key exists in the catalog.
func Has(key Key) bool {
	_, ok := catalog[key]
	return ok
}

// Msg is a message that is rendered later, in the reader's language.
// A Msg may be passed as an argument to another message.
type Msg struct {
	Key  Key
	Args []any
}

// M builds a Msg.
func M(key Key, args ...any) Msg { return Msg{Key: key, Args: args} }

// T renders key in lang. Args are applied with fmt.Sprintf semantics;
// Msg arguments are rendered in the same language first.
// A missing key renders as "!(key)" so gaps are visible in tests.
func T(lang Lang, key Key, args ...any) string {
	m, ok := catalog[key]
	if !ok {
		return "!(" + string(key) + ")"
	}
	tmpl := m.in(lang)
	if len(args) == 0 {
		return tmpl
	}
	resolved := make([]any, len(args))
	for i, a := range args {
		if sub, ok := a.(Msg); ok {
			resolved[i] = T(lang, sub.Key, sub.Args...)
		} else {
			resolved[i] = a
		}
	}
	return fmt.Sprintf(tmpl, resolved...)
}

// Printer writes catalog messages to an output stream.
// It is the only sanctioned way to write operator-facing text.
type Printer struct {
	w      io.Writer
	lang   Lang
	filter func(string) string
}

// NewPrinter returns a Printer writing to w in lang.
func NewPrinter(w io.Writer, lang Lang) *Printer {
	if _, ok := ParseLang(string(lang)); !ok {
		lang = Default
	}
	return &Printer{w: w, lang: lang}
}

// SetFilter installs a function applied to every line before it is written
// (the CLI uses it to mask secrets). It returns p for chaining.
func (p *Printer) SetFilter(f func(string) string) *Printer {
	p.filter = f
	return p
}

// Lang returns the printer's language.
func (p *Printer) Lang() Lang { return p.lang }

// Say writes the rendered message followed by a newline.
func (p *Printer) Say(key Key, args ...any) {
	s := T(p.lang, key, args...)
	if p.filter != nil {
		s = p.filter(s)
	}
	_, _ = io.WriteString(p.w, s+"\n")
}

// T renders key in the printer's language without writing it.
func (p *Printer) T(key Key, args ...any) string {
	return T(p.lang, key, args...)
}

// Blank writes an empty line.
func (p *Printer) Blank() {
	_, _ = io.WriteString(p.w, "\n")
}
