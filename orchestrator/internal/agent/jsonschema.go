package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/0xyu/aco/schemas"
)

// A small JSON Schema validator for the keywords ACO's review schemas use
// (type, properties, required, additionalProperties: false, items, enum,
// plus the annotations $schema, $id, title, description). A schema with
// any other keyword is rejected when it is compiled, so a schema change
// can never silently skip validation. Reviews are validated against the
// whole embedded schema before the request-specific checks: a missing
// nested field or a null where a value is required is a schema error, not
// a zero value.

type schemaNode struct {
	Type       string
	Properties map[string]*schemaNode
	Required   []string
	Closed     bool // additionalProperties: false
	Items      *schemaNode
	Enum       []string
}

var annotationKeys = map[string]bool{"$schema": true, "$id": true, "title": true, "description": true}

func compileSchema(raw json.RawMessage, path string) (*schemaNode, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	n := &schemaNode{}
	for k, v := range m {
		switch k {
		case "type":
			if err := json.Unmarshal(v, &n.Type); err != nil {
				return nil, fmt.Errorf("%s.type: only a single type name is supported", path)
			}
		case "required":
			if err := json.Unmarshal(v, &n.Required); err != nil {
				return nil, fmt.Errorf("%s.required: %w", path, err)
			}
		case "enum":
			if err := json.Unmarshal(v, &n.Enum); err != nil {
				return nil, fmt.Errorf("%s.enum: only string enums are supported", path)
			}
		case "additionalProperties":
			var b bool
			if err := json.Unmarshal(v, &b); err != nil || b {
				return nil, fmt.Errorf("%s.additionalProperties: only false is supported", path)
			}
			n.Closed = true
		case "properties":
			var props map[string]json.RawMessage
			if err := json.Unmarshal(v, &props); err != nil {
				return nil, fmt.Errorf("%s.properties: %w", path, err)
			}
			n.Properties = map[string]*schemaNode{}
			for name, pv := range props {
				c, err := compileSchema(pv, path+"."+name)
				if err != nil {
					return nil, err
				}
				n.Properties[name] = c
			}
		case "items":
			c, err := compileSchema(v, path+"[]")
			if err != nil {
				return nil, err
			}
			n.Items = c
		default:
			if !annotationKeys[k] {
				return nil, fmt.Errorf("%s: unsupported schema keyword %q", path, k)
			}
		}
	}
	switch n.Type {
	case "object", "array", "string", "boolean", "integer", "number":
	default:
		return nil, fmt.Errorf("%s: unsupported or missing type %q", path, n.Type)
	}
	return n, nil
}

// validate appends every violation of v (decoded with UseNumber) to probs.
func (n *schemaNode) validate(v any, path string, probs *[]string) {
	add := func(format string, a ...any) { *probs = append(*probs, path+": "+fmt.Sprintf(format, a...)) }
	if v == nil {
		add("must be %s, not null", n.Type)
		return
	}
	switch n.Type {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			add("must be an object")
			return
		}
		for _, r := range n.Required {
			if _, ok := obj[r]; !ok {
				*probs = append(*probs, path+"."+r+": missing required field")
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c, ok := n.Properties[k]
			if !ok {
				if n.Closed {
					*probs = append(*probs, path+"."+k+": unknown field")
				}
				continue
			}
			c.validate(obj[k], path+"."+k, probs)
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			add("must be an array")
			return
		}
		if n.Items != nil {
			for i, e := range arr {
				n.Items.validate(e, fmt.Sprintf("%s[%d]", path, i), probs)
			}
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			add("must be a string")
			return
		}
		if len(n.Enum) > 0 && !slices.Contains(n.Enum, s) {
			add("%q is not one of %s", s, strings.Join(n.Enum, "|"))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			add("must be a boolean")
		}
	case "integer", "number":
		num, ok := v.(json.Number)
		if !ok {
			add("must be a number")
			return
		}
		if n.Type == "integer" {
			if _, err := num.Int64(); err != nil {
				add("must be an integer")
			}
		}
	}
}

var (
	reviewSchemasOnce              sync.Once
	planReviewSchema, codeReviewSc *schemaNode
	reviewSchemaErr                error
)

func reviewSchemas() (*schemaNode, *schemaNode, error) {
	reviewSchemasOnce.Do(func() {
		planReviewSchema, reviewSchemaErr = compileSchema(schemas.PlanReview, "plan-review")
		if reviewSchemaErr == nil {
			codeReviewSc, reviewSchemaErr = compileSchema(schemas.CodeReview, "code-review")
		}
	})
	return planReviewSchema, codeReviewSc, reviewSchemaErr
}

// ValidateSchema checks the JSON document raw against schema (restricted to
// the keywords listed above). It returns every violation; an error means
// the schema itself is not supported.
func ValidateSchema(schema, raw []byte) ([]string, error) {
	n, err := compileSchema(schema, "$schema")
	if err != nil {
		return nil, err
	}
	return validateJSON(n, raw), nil
}

// validateJSON checks raw against a compiled schema. The answer must be a
// single JSON value.
func validateJSON(n *schemaNode, raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []string{"the review is not valid JSON: " + err.Error()}
	}
	if dec.More() {
		return []string{"the review has data after the JSON object"}
	}
	var probs []string
	n.validate(v, "$", &probs)
	return probs
}
