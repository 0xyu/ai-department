package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/0xyu/aco/internal/secure"
)

// Event is one persisted run event. Seq is assigned by the Log: it starts at
// 1 for each run and increases by one per event, with no gaps.
type Event struct {
	RunID   string
	Seq     int64
	Type    Type
	Time    time.Time
	Stage   string // planning | plan_review | execution | final_verification, or ""
	Phase   int    // phase number, or 0 for run-level events
	Payload Payload
	// Message is the event rendered in the operator language for display.
	// Payload fields are the machine-readable data; Message is not.
	Message string
}

// kind is the JSON type of a payload field.
type kind uint8

const (
	kindString kind = iota + 1
	kindInt
	kindBool
	kindStrings
)

// Field is one typed payload entry. Build fields with Str, Int, Bool, Strs.
type Field struct {
	Key  string
	kind kind
	s    string
	i    int64
	b    bool
	ss   []string
}

// Str is a string field.
func Str(key, v string) Field { return Field{Key: key, kind: kindString, s: v} }

// Int is an integer field.
func Int(key string, v int64) Field { return Field{Key: key, kind: kindInt, i: v} }

// Bool is a boolean field.
func Bool(key string, v bool) Field { return Field{Key: key, kind: kindBool, b: v} }

// Strs is a string-list field.
func Strs(key string, v ...string) Field {
	return Field{Key: key, kind: kindStrings, ss: append([]string{}, v...)}
}

// String returns a string field's value ("" for other kinds).
func (f Field) String() string { return f.s }

// IntValue returns an integer field's value (0 for other kinds).
func (f Field) IntValue() int64 { return f.i }

// BoolValue returns a boolean field's value (false for other kinds).
func (f Field) BoolValue() bool { return f.b }

// Strings returns a string-list field's value (nil for other kinds).
func (f Field) Strings() []string { return append([]string(nil), f.ss...) }

// Payload is an ordered set of typed fields, encoded as a JSON object.
type Payload []Field

// Get returns the field named key.
func (p Payload) Get(key string) (Field, bool) {
	for _, f := range p {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

var keyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Validate checks field keys (lower snake case, unique) and kinds.
func (p Payload) Validate() error {
	seen := map[string]bool{}
	for i, f := range p {
		if !keyRE.MatchString(f.Key) {
			return fmt.Errorf("payload field #%d: key must be lower snake case", i+1)
		}
		if seen[f.Key] {
			return fmt.Errorf("payload field %s: duplicate key", f.Key)
		}
		seen[f.Key] = true
		if f.kind < kindString || f.kind > kindStrings {
			return fmt.Errorf("payload field %s: no value (build fields with Str, Int, Bool, Strs)", f.Key)
		}
	}
	return nil
}

// Redact returns a copy with every string masked by r.
func (p Payload) Redact(r *secure.Redactor) Payload {
	out := make(Payload, len(p))
	for i, f := range p {
		f.s = r.String(f.s)
		if f.ss != nil {
			ss := make([]string, len(f.ss))
			for j, s := range f.ss {
				ss[j] = r.String(s)
			}
			f.ss = ss
		}
		// Keys are lower-snake-case identifiers (Validate), set by code.
		out[i] = f
	}
	return out
}

func quote(s string) []byte {
	b, _ := json.Marshal(s) // a string always marshals
	return b
}

// MarshalJSON encodes the fields as a JSON object in their order.
func (p Payload) MarshalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range p {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(quote(f.Key))
		b.WriteByte(':')
		switch f.kind {
		case kindString:
			b.Write(quote(f.s))
		case kindInt:
			b.WriteString(strconv.FormatInt(f.i, 10))
		case kindBool:
			b.WriteString(strconv.FormatBool(f.b))
		case kindStrings:
			b.WriteByte('[')
			for j, s := range f.ss {
				if j > 0 {
					b.WriteByte(',')
				}
				b.Write(quote(s))
			}
			b.WriteByte(']')
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON decodes an object written by MarshalJSON, keeping field
// order. Only strings, integers, booleans, and string lists are accepted.
func (p *Payload) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("payload: want a JSON object")
	}
	var out Payload
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := kt.(string)
		f, err := decodeValue(dec, key)
		if err != nil {
			return err
		}
		out = append(out, f)
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("payload: trailing data")
	}
	if err := out.Validate(); err != nil {
		return err
	}
	*p = out
	return nil
}

func decodeValue(dec *json.Decoder, key string) (Field, error) {
	tok, err := dec.Token()
	if err != nil {
		return Field{}, err
	}
	switch v := tok.(type) {
	case string:
		return Str(key, v), nil
	case bool:
		return Bool(key, v), nil
	case json.Number:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return Field{}, fmt.Errorf("payload field %s: not an integer", key)
		}
		return Int(key, n), nil
	case json.Delim:
		if v != '[' {
			break
		}
		ss := []string{}
		for dec.More() {
			t, err := dec.Token()
			if err != nil {
				return Field{}, err
			}
			s, ok := t.(string)
			if !ok {
				return Field{}, fmt.Errorf("payload field %s: list items must be strings", key)
			}
			ss = append(ss, s)
		}
		if _, err := dec.Token(); err != nil {
			return Field{}, err
		}
		return Field{Key: key, kind: kindStrings, ss: ss}, nil
	}
	return Field{}, fmt.Errorf("payload field %s: unsupported value", key)
}

// MarshalJSON encodes the event in the aco.event/v1 form
// (schemas/event.schema.json).
func (e Event) MarshalJSON() ([]byte, error) {
	payload := e.Payload
	if payload == nil {
		payload = Payload{}
	}
	pj, err := payload.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(`{"schema":`)
	b.Write(quote(Schema))
	b.WriteString(`,"run_id":`)
	b.Write(quote(e.RunID))
	b.WriteString(`,"seq":` + strconv.FormatInt(e.Seq, 10))
	b.WriteString(`,"type":`)
	b.Write(quote(string(e.Type)))
	b.WriteString(`,"time":`)
	b.Write(quote(e.Time.UTC().Format(time.RFC3339Nano)))
	b.WriteString(`,"stage":`)
	b.Write(quote(e.Stage))
	b.WriteString(`,"phase":` + strconv.Itoa(e.Phase))
	b.WriteString(`,"payload":`)
	b.Write(pj)
	b.WriteString(`,"message":`)
	b.Write(quote(e.Message))
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON decodes an aco.event/v1 event.
func (e *Event) UnmarshalJSON(data []byte) error {
	var w struct {
		Schema  string          `json:"schema"`
		RunID   string          `json:"run_id"`
		Seq     int64           `json:"seq"`
		Type    Type            `json:"type"`
		Time    time.Time       `json:"time"`
		Stage   string          `json:"stage"`
		Phase   int             `json:"phase"`
		Payload json.RawMessage `json:"payload"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.Schema != Schema {
		return fmt.Errorf("event: unsupported schema %q", w.Schema)
	}
	var p Payload
	if len(w.Payload) > 0 {
		if err := p.UnmarshalJSON(w.Payload); err != nil {
			return err
		}
	}
	*e = Event{RunID: w.RunID, Seq: w.Seq, Type: w.Type, Time: w.Time, Stage: w.Stage, Phase: w.Phase, Payload: p, Message: w.Message}
	return nil
}
