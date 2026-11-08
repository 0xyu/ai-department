package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Layer names, lowest to highest precedence.
const (
	LayerDefaults = "defaults"
	LayerUser     = "user"
	LayerRepo     = "repo"
	LayerFile     = "file"
	LayerPlan     = "plan"
	LayerFlags    = "flags"
)

// RepoConfigName is the per-repository config file name.
const RepoConfigName = ".aco.yaml"

// LoadOptions selects the layers to load.
type LoadOptions struct {
	UserConfigPath  string   // ~/.aco/config.yaml; skipped if empty or missing
	RepoDir         string   // directory holding .aco.yaml; skipped if empty or file missing
	ConfigFiles     []string // --config files; each must exist
	PlanFrontMatter []byte   // YAML from the plan's front matter; skipped if empty
	PlanSource      string   // label for the plan layer (plan file path)
	Overrides       []string // --set key.path=value, applied last
}

// LayerInfo describes one applied layer.
type LayerInfo struct {
	Name   string
	Source string
}

// Loaded is the result of Load.
type Loaded struct {
	Config *Config
	Layers []LayerInfo // layers actually applied, defaults first
	origin map[string]LayerInfo
}

// Origin returns the highest layer that set the field at path (dotted, e.g.
// "auth.mode"). It returns false when the value is the built-in default.
func (l *Loaded) Origin(path string) (LayerInfo, bool) {
	if l == nil {
		return LayerInfo{}, false
	}
	li, ok := l.origin[path]
	return li, ok
}

type layer struct {
	name   string
	source string
	node   *yaml.Node // document content (mapping) or nil
}

// Load builds the effective config from defaults and the selected layers.
// On failure the error is of type Errors with field-level entries.
func Load(opts LoadOptions) (*Loaded, error) {
	var errs Errors
	var layers []layer

	addFile := func(name, path string, mustExist bool) {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && !mustExist {
				return
			}
			errs = append(errs, FieldError{Layer: name, Source: path, Code: ErrFileNotFound})
			return
		}
		n, perr := parseYAML(data)
		if perr != nil {
			perr.Layer, perr.Source = name, path
			errs = append(errs, *perr)
			return
		}
		layers = append(layers, layer{name: name, source: path, node: n})
	}

	if opts.UserConfigPath != "" {
		addFile(LayerUser, opts.UserConfigPath, false)
	}
	if opts.RepoDir != "" {
		addFile(LayerRepo, filepath.Join(opts.RepoDir, RepoConfigName), false)
	}
	for _, f := range opts.ConfigFiles {
		addFile(LayerFile, f, true)
	}
	if len(bytes.TrimSpace(opts.PlanFrontMatter)) > 0 {
		n, perr := parseYAML(opts.PlanFrontMatter)
		if perr != nil {
			perr.Layer, perr.Source = LayerPlan, opts.PlanSource
			errs = append(errs, *perr)
		} else {
			layers = append(layers, layer{name: LayerPlan, source: opts.PlanSource, node: n})
		}
	}
	for i, ov := range opts.Overrides {
		n, src, ferr := overrideNode(i, ov)
		if ferr != nil {
			errs = append(errs, *ferr)
			continue
		}
		layers = append(layers, layer{name: LayerFlags, source: src, node: n})
	}

	cfg := Default()
	applied := []LayerInfo{{Name: LayerDefaults}}
	reported := map[string]bool{}
	origin := map[string]LayerInfo{}

	for _, l := range layers {
		if l.node == nil {
			applied = append(applied, LayerInfo{Name: l.name, Source: l.source})
			continue
		}
		w := walker{layer: l.name, source: l.source, paths: map[string]int{}}
		w.walk(l.node, reflect.TypeOf(Config{}), "")
		// Decode even when the walker found problems, so value errors in the
		// same layer are reported too (the result is discarded on any error).
		errs = append(errs, w.errs...)
		if err := l.node.Decode(cfg); err != nil {
			if len(w.errs) == 0 {
				errs = append(errs, decodeError(l, err))
			}
			continue
		}
		info := LayerInfo{Name: l.name, Source: l.source}
		applied = append(applied, info)
		for p := range w.paths {
			origin[p] = info
		}
		for _, ve := range Validate(cfg) {
			line, set := w.paths[ve.Field]
			if !set || reported[ve.Field] {
				continue
			}
			reported[ve.Field] = true
			if l.name == LayerFlags {
				line = 0 // a --set value has no meaningful line
			}
			ve.Layer, ve.Source, ve.Line = l.name, l.source, line
			errs = append(errs, ve)
		}
	}

	// Anything still invalid that no layer set explicitly (should not happen
	// with sane defaults, but keep the merged config honest).
	for _, ve := range Validate(cfg) {
		if !reported[ve.Field] {
			ve.Layer = LayerDefaults
			errs = append(errs, ve)
		}
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return &Loaded{Config: cfg, Layers: applied, origin: origin}, nil
}

var yamlLineRE = regexp.MustCompile(`line (\d+)`)

func lineOf(msg string) int {
	if m := yamlLineRE.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// errMultipleDocs is the parse detail for input holding more than one document.
const errMultipleDocs = "more than one YAML document; a config holds exactly one"

// parseYAML parses exactly one YAML document into its root content node.
// Empty input (or a document with no content) yields a nil node. A trailing
// document, or malformed YAML after the first document, is a parse error.
func parseYAML(data []byte) (*yaml.Node, *FieldError) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	parseErr := func(err error) *FieldError {
		return &FieldError{Code: ErrParse, Line: lineOf(err.Error()), Value: strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, parseErr(err)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, parseErr(err)
	default:
		return nil, &FieldError{Code: ErrParse, Line: docLine(&extra, data), Value: errMultipleDocs}
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return doc.Content[0], nil
}

// docLine returns the line where a trailing document starts: its first
// content node, or else the last "---" marker line (an empty document).
func docLine(doc *yaml.Node, data []byte) int {
	if len(doc.Content) > 0 && doc.Content[0].Line > 0 {
		return doc.Content[0].Line
	}
	last := 0
	for i, ln := range strings.Split(string(data), "\n") {
		if ln == "---" || strings.HasPrefix(ln, "--- ") || strings.HasPrefix(ln, "---\t") {
			last = i + 1
		}
	}
	return last
}

func decodeError(l layer, err error) FieldError {
	msg := err.Error()
	var te *yaml.TypeError
	if errors.As(err, &te) && len(te.Errors) > 0 {
		msg = te.Errors[0]
	}
	return FieldError{Layer: l.name, Source: l.source, Line: lineOf(msg), Code: ErrInvalidType, Value: msg}
}

// overrideNode turns "a.b.c=value" into the YAML mapping {a: {b: {c: value}}}.
// The value is parsed as YAML, so "true", "5", and "[x, y]" get their types.
//
// The returned source names the option and key path ("--set auth.mode") but
// never the value, which may be a credential. A malformed override, or one
// whose key is not a trusted schema path, is named by its position
// ("--set #2") because the key itself may be a secret.
func overrideNode(index int, kv string) (*yaml.Node, string, *FieldError) {
	ordinal := "#" + strconv.Itoa(index+1)
	key, val, ok := strings.Cut(kv, "=")
	key = strings.TrimSpace(key)
	segs := strings.Split(key, ".")
	valid := ok
	for _, s := range segs {
		if s == "" {
			valid = false
		}
	}
	if !valid {
		return nil, "", &FieldError{Layer: LayerFlags, Source: "--set " + ordinal, Code: ErrInvalidOverride, Value: ordinal, Allowed: "key.path=value"}
	}
	source := overrideSource(ordinal, key)
	leaf, perr := parseYAML([]byte(val))
	if perr != nil {
		// perr.Value is the YAML parser's message, not the raw value.
		perr.Layer, perr.Source, perr.Field = LayerFlags, source, key
		return nil, "", perr
	}
	if leaf == nil {
		leaf = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""}
	}
	n := leaf
	for i := len(segs) - 1; i >= 0; i-- {
		n = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: segs[i]}, n,
		}}
	}
	return n, source, nil
}

// secretKeyRE matches key names that look like credentials. Such keys are
// never valid in config: secrets come only from env, env file, or keychain.
var secretKeyRE = regexp.MustCompile(`(?i)(api[_-]?key|apikey|secret|password|passwd|token|credential|private[_-]?key)`)

var durationType = reflect.TypeOf(Duration(0))

// walker checks a YAML node tree against the Config struct before decoding:
// unknown keys, wrong kinds, and bad durations become field-level errors.
// It also records every path the layer sets (with its line).
type walker struct {
	layer  string
	source string
	paths  map[string]int
	errs   Errors
}

func (w *walker) fail(n *yaml.Node, path string, code ErrCode, value, allowed string) {
	line := n.Line
	if w.layer == LayerFlags {
		line = 0
	}
	w.errs = append(w.errs, FieldError{Layer: w.layer, Source: w.source, Line: line, Field: path, Code: code, Value: value, Allowed: allowed})
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (w *walker) walk(n *yaml.Node, t reflect.Type, path string) {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if path != "" {
		w.paths[path] = n.Line
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return // null leaves the lower layer's value in place (or clears lists)
	}
	if t == durationType {
		if n.Kind != yaml.ScalarNode {
			w.fail(n, path, ErrInvalidDuration, "", "e.g. 60m, 4h")
			return
		}
		if _, err := time.ParseDuration(n.Value); err != nil {
			w.fail(n, path, ErrInvalidDuration, n.Value, "e.g. 60m, 4h")
		}
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			w.fail(n, path, ErrInvalidType, n.Value, "mapping")
			return
		}
		fields := yamlFields(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			ft, ok := fields[k.Value]
			if !ok {
				p := join(path, k.Value)
				switch {
				case secretKeyRE.MatchString(k.Value):
					w.fail(k, p, ErrSecretField, "", "")
				case path == "gates":
					w.fail(k, p, ErrUnknownGateType, k.Value, strings.Join(GateTypes, "|")+"|auto_detect")
				default:
					w.fail(k, p, ErrUnknownField, k.Value, "")
				}
				continue
			}
			w.walk(v, ft, join(path, k.Value))
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			w.fail(n, path, ErrInvalidType, n.Value, "mapping")
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			w.walk(n.Content[i+1], t.Elem(), join(path, n.Content[i].Value))
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			w.fail(n, path, ErrInvalidType, n.Value, "list")
			return
		}
		for i, item := range n.Content {
			w.walk(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.String:
		if n.Kind != yaml.ScalarNode {
			w.fail(n, path, ErrInvalidType, "", "string")
		}
	case reflect.Bool:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
			w.fail(n, path, ErrInvalidType, n.Value, "true|false")
		}
	case reflect.Int:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!int" {
			w.fail(n, path, ErrInvalidType, n.Value, "integer")
		}
	case reflect.Float64:
		if n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!float") {
			w.fail(n, path, ErrInvalidType, n.Value, "number")
		}
	}
}

func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = f.Type
	}
	return out
}

// ExtractFrontMatter returns the YAML front matter of a Markdown document
// ("---" line, YAML, "---" line at the top), or false if there is none.
func ExtractFrontMatter(md []byte) ([]byte, bool) {
	s := strings.ReplaceAll(string(md), "\r\n", "\n")
	s = strings.TrimPrefix(s, string(rune(0xFEFF))) // UTF-8 BOM
	if !strings.HasPrefix(s, "---\n") {
		return nil, false
	}
	rest := s[len("---\n"):]
	if strings.HasPrefix(rest, "---\n") || rest == "---" {
		return []byte{}, true
	}
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - len("\n---")
		} else {
			return nil, false
		}
	}
	return []byte(rest[:end+1]), true
}
