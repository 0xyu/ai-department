// Package agents loads employee definitions and runs each employee as its own Claude agent loop.
package agents

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Def is one employee, parsed from agents/<id>.md (YAML front matter + Markdown body).
type Def struct {
	ID       string   `yaml:"id" json:"id"`
	Name     string   `yaml:"name" json:"name"`
	Title    string   `yaml:"title" json:"title"`
	Color    string   `yaml:"color" json:"color"`
	Tools    []string `yaml:"tools" json:"tools"`
	MaxTurns int      `yaml:"max_turns" json:"max_turns"`
	Body     string   `yaml:"-" json:"body"` // role, boundaries, delivery standard, escalation rules
}

func (d *Def) HasTool(name string) bool {
	for _, t := range d.Tools {
		if t == name {
			return true
		}
	}
	return false
}

// Order is the fixed desk order in the office.
var Order = []string{"max", "dev", "ops", "sec", "qa", "doc"}

func LoadDefs(dir string) (map[string]*Def, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	defs := map[string]*Def{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		d, err := parseDef(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, t := range d.Tools {
			if _, ok := toolSpecs[t]; !ok {
				return nil, fmt.Errorf("%s: unknown tool %q", f, t)
			}
		}
		defs[d.ID] = d
	}
	for _, id := range Order {
		if defs[id] == nil {
			return nil, fmt.Errorf("missing agent definition agents/%s.md", id)
		}
	}
	return defs, nil
}

func parseDef(raw []byte) (*Def, error) {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	s := string(raw)
	if !strings.HasPrefix(s, "---\n") {
		return nil, fmt.Errorf("missing front matter")
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return nil, fmt.Errorf("unterminated front matter")
	}
	var d Def
	if err := yaml.Unmarshal([]byte(s[4:4+end]), &d); err != nil {
		return nil, err
	}
	d.Body = strings.TrimSpace(s[4+end+4:])
	if d.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	if d.MaxTurns == 0 {
		d.MaxTurns = 40
	}
	sort.Strings(d.Tools)
	return &d, nil
}
