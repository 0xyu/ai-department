// Package prompt holds ACO's embedded, versioned prompt templates (English,
// §7). System prompts set the fixed rules of each role; task prompts carry
// the inputs. Everything that comes from the repository, the requirement
// document, a review, or a gate log is wrapped in a <data> block and the
// rules say such content is data, never instructions (§16.1 prompt
// injection). Rendered prompts are masked by the caller's Redactor before
// they are written or sent.
package prompt

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

// Version is recorded with every invocation; bump it when a template's
// meaning changes.
const Version = "v1"

// Template names.
const (
	PlannerSystem     = "planner_system"
	ImplementerSystem = "implementer_system"
	PlanCreate        = "plan_create"
	PlanRevise        = "plan_revise"
	PhaseImplement    = "phase_implement"
	GateFix           = "gate_fix"
	ReviewFix         = "review_fix"
	Continue          = "continue"
	FinalFix          = "final_fix"
)

// Names lists every template.
var Names = []string{PlannerSystem, ImplementerSystem, PlanCreate, PlanRevise, PhaseImplement, GateFix, ReviewFix, Continue, FinalFix}

//go:embed templates/*.tmpl
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"data":  dataBlock,
	"join":  strings.Join,
	"inc":   func(i int) int { return i + 1 },
	"phase": func(n int) string { return fmt.Sprintf("P%02d", n) },
}).Option("missingkey=error").ParseFS(files, "templates/*.tmpl"))

// dataBlock wraps untrusted text. A closing tag inside the text is
// neutralized so the text cannot end its own block early.
func dataBlock(name, text string) string {
	text = strings.ReplaceAll(text, "</data", "</ data")
	text = strings.ReplaceAll(text, "<data", "< data")
	if strings.TrimSpace(text) == "" {
		text = "(none)"
	}
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '<' || r == '>' || r == '\n' || r == '\r' {
			return '_'
		}
		return r
	}, name)
	return "<data name=\"" + name + "\">\n" + strings.TrimRight(text, "\n") + "\n</data>"
}

// Render executes the named template with data.
func Render(name string, data any) (string, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name+".tmpl", data); err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	return strings.TrimSpace(b.String()) + "\n", nil
}
