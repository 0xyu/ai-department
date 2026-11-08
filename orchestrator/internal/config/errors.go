package config

import (
	"strconv"
	"strings"
)

// ErrCode classifies a config error. The CLI renders each code through the
// i18n catalog (key "config.err.<code>").
type ErrCode string

// Error codes.
const (
	ErrParse              ErrCode = "parse"               // YAML syntax error
	ErrUnknownField       ErrCode = "unknown_field"       // key not in the schema
	ErrUnknownGateType    ErrCode = "unknown_gate_type"   // gates.<x> where x is not one of the six types
	ErrSecretField        ErrCode = "secret_field"        // credential-like key (api_key, token, …) in a config file
	ErrInvalidType        ErrCode = "invalid_type"        // wrong YAML kind/type for the field
	ErrInvalidValue       ErrCode = "invalid_value"       // value not in the allowed set
	ErrOutOfRange         ErrCode = "out_of_range"        // numeric value outside its range
	ErrInvalidDuration    ErrCode = "invalid_duration"    // not a Go duration string
	ErrUnsupportedVersion ErrCode = "unsupported_version" // version != CurrentVersion
	ErrRequired           ErrCode = "required"            // empty value where one is required
	ErrFileNotFound       ErrCode = "file_not_found"      // explicitly requested file missing
	ErrInvalidOverride    ErrCode = "invalid_override"    // malformed --set key=value
)

// FieldError is one config problem, located by layer and field path.
type FieldError struct {
	Layer   string  // e.g. "repo", "user", "flags"
	Source  string  // file path or flag text, if any
	Line    int     // 1-based line in Source, 0 if unknown
	Field   string  // dotted path, e.g. "planner.effort"
	Code    ErrCode //
	Value   string  // offending value (never a secret: secret fields carry no value)
	Allowed string  // allowed values / range, e.g. "low|medium|high|max"
}

func (e FieldError) Error() string {
	var b strings.Builder
	if e.Source != "" {
		b.WriteString(e.Source)
		if e.Line > 0 {
			b.WriteString(":" + strconv.Itoa(e.Line))
		}
		b.WriteString(": ")
	}
	if e.Field != "" {
		b.WriteString(e.Field)
		b.WriteString(": ")
	}
	b.WriteString(string(e.Code))
	if e.Value != "" {
		b.WriteString(" " + strconv.Quote(e.Value))
	}
	if e.Allowed != "" {
		b.WriteString(" (allowed: " + e.Allowed + ")")
	}
	return b.String()
}

// Errors is a list of config problems. It implements error.
type Errors []FieldError

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Has reports whether any error has the given field and code.
func (es Errors) Has(field string, code ErrCode) bool {
	for _, e := range es {
		if e.Field == field && e.Code == code {
			return true
		}
	}
	return false
}
