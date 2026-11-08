// Package logging sets up log/slog with a handler that redacts secrets from
// every message and attribute before it is written.
//
// Logs are machine-oriented and written in English; operator-facing text goes
// through internal/i18n instead.
package logging

import (
	"bytes"
	"context"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/0xyu/aco/internal/redact"
)

// Options configure a logger.
type Options struct {
	Level    slog.Level
	JSON     bool
	Redactor *redact.Redactor
}

// New returns a logger writing to w with redaction.
func New(w io.Writer, o Options) *slog.Logger {
	ho := &slog.HandlerOptions{Level: o.Level}
	var inner slog.Handler
	if o.JSON {
		inner = slog.NewJSONHandler(w, ho)
	} else {
		inner = slog.NewTextHandler(w, ho)
	}
	return slog.New(NewRedactHandler(inner, o.Redactor))
}

// ParseLevel parses debug|info|warn|error (default warn).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// RedactHandler wraps a slog.Handler and masks secrets in the message,
// attribute keys and values, and group names.
//
// Values of kind Any are converted exactly once, the way the inner handler
// would convert them, and the redacted result is what gets emitted: the
// inner handler only ever sees plain strings or already-sanitized JSON, so
// no custom method (MarshalJSON, MarshalText, Error, Format, String) runs
// again on the original value after it was inspected.
type RedactHandler struct {
	inner slog.Handler
	r     *redact.Redactor
	json  bool // inner is a *slog.JSONHandler
}

// NewRedactHandler wraps inner. A nil redactor masks known patterns only.
func NewRedactHandler(inner slog.Handler, r *redact.Redactor) *RedactHandler {
	if r == nil {
		r = redact.New()
	}
	_, isJSON := inner.(*slog.JSONHandler)
	return &RedactHandler{inner: inner, r: r, json: isJSON}
}

// Enabled implements slog.Handler.
func (h *RedactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *RedactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h *RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = h.attr(a)
	}
	return &RedactHandler{inner: h.inner.WithAttrs(red), r: h.r, json: h.json}
}

// WithGroup implements slog.Handler.
func (h *RedactHandler) WithGroup(name string) slog.Handler {
	return &RedactHandler{inner: h.inner.WithGroup(h.r.String(name)), r: h.r, json: h.json}
}

func (h *RedactHandler) attr(a slog.Attr) slog.Attr {
	key := h.r.String(a.Key)
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(key, h.r.String(v.String()))
	case slog.KindGroup:
		g := v.Group()
		red := make([]any, len(g))
		for i, ga := range g {
			red[i] = h.attr(ga)
		}
		return slog.Group(key, red...)
	case slog.KindAny:
		return slog.Attr{Key: key, Value: h.anyValue(v.Any())}
	default:
		return slog.Attr{Key: key, Value: v}
	}
}

// anyValue converts x once, following the inner handler's own rules, and
// returns the redacted result.
//
//	JSON handler: Error() for errors that are not json.Marshalers, else json.Marshal.
//	Text handler: MarshalText for encoding.TextMarshalers, []byte as a string, else %+v.
//
// A panic in a custom conversion (Error, MarshalJSON, MarshalText, Format,
// String) is contained here: the value becomes a fixed placeholder that never
// includes the panic payload, and the rest of the record is logged normally.
func (h *RedactHandler) anyValue(x any) (out slog.Value) {
	defer func() {
		if r := recover(); r != nil {
			// Like slog: a method on a nil pointer is the usual cause.
			if rv := reflect.ValueOf(x); rv.Kind() == reflect.Pointer && rv.IsNil() {
				out = slog.StringValue("<nil>")
				return
			}
			out = slog.StringValue(PanicPlaceholder)
		}
	}()
	if h.json {
		_, jm := x.(json.Marshaler)
		_, tm := x.(encoding.TextMarshaler)
		if err, ok := x.(error); ok && !jm {
			return slog.StringValue(h.r.String(err.Error()))
		}
		// encoding/json writes byte slices (named ones too) as Base64, which
		// hides secrets from text masking. Mask the original bytes first; the
		// field stays a Base64 string. Marshalers keep precedence.
		if bs, ok := byteSlice(x); ok && !jm && !tm {
			if red := h.r.String(string(bs)); red != string(bs) {
				x = []byte(red)
			}
		}
		// Nested plain byte slices become Base64, which sanitizeJSON masks.
		// Byte data written as number lists (arrays, integer slices, slices
		// of marshaling byte types) cannot be masked after encoding.
		if !jm && !tm {
			switch r := h.scanBytes(x, scanJSON); {
			case r.incomplete:
				return slog.StringValue(UninspectablePlaceholder)
			case r.unmaskable:
				return slog.StringValue(BinarySecretPlaceholder)
			}
		}
		b, err := marshalJSON(x)
		if err != nil {
			return slog.StringValue("!ERROR:" + h.r.String(err.Error()))
		}
		clean, err := h.sanitizeJSON(b)
		if err != nil {
			return slog.StringValue("!ERROR:" + h.r.String(err.Error()))
		}
		return slog.AnyValue(json.RawMessage(clean))
	}
	if tm, ok := x.(encoding.TextMarshaler); ok {
		b, err := tm.MarshalText()
		if err != nil {
			return slog.StringValue("!ERROR:" + h.r.String(err.Error()))
		}
		return slog.StringValue(h.r.String(string(b)))
	}
	if b, ok := byteSlice(x); ok {
		return slog.StringValue(h.r.String(string(b)))
	}
	return slog.StringValue(h.textFallback(x))
}

// textFallback renders x as %+v, as slog's text handler would, with two
// guards fmt does not provide:
//
//   - The top-level Format/Error/String method is called here, inside the
//     caller's recover, instead of by fmt. fmt would catch a panic itself and
//     print its payload ("%!v(PANIC=Error method: ...)").
//   - %+v prints byte data nested in maps, slices, and struct fields as a
//     decimal list ("[115 107 ...]") that text masking cannot see. Such data
//     is checked first (by reflection, without calling any method), and a
//     value that holds a secret there is rendered in a masked form instead.
func (h *RedactHandler) textFallback(x any) string {
	switch t := x.(type) {
	case fmt.Formatter:
		var panicked bool
		s := fmt.Sprintf("%+v", guardedFormatter{t, &panicked})
		if panicked {
			return PanicPlaceholder
		}
		return h.r.String(s)
	case error:
		return h.r.String(t.Error())
	case fmt.Stringer:
		return h.r.String(t.String())
	}
	switch r := h.scanBytes(x, scanText); {
	case r.incomplete:
		return UninspectablePlaceholder
	case r.dirty:
		// fmt would print the secret bytes as a decimal list. Use the JSON
		// form only if it can mask every dirty sequence (scanned under JSON's
		// own rules); otherwise use a placeholder. fmt has not run, so every
		// custom conversion still runs at most once.
		if j := h.scanBytes(x, scanJSON); j.incomplete || j.unmaskable {
			return BinarySecretPlaceholder
		}
		b, err := marshalJSON(x)
		if err != nil {
			return BinarySecretPlaceholder
		}
		clean, err := h.sanitizeJSON(b)
		if err != nil {
			return BinarySecretPlaceholder
		}
		return string(clean)
	}
	s := fmt.Sprintf("%+v", x)
	// A method on a nested value can still panic inside fmt, which prints
	// "%!v(PANIC=<Method> method: <payload>)". Match that exact form only.
	if fmtPanicRE.MatchString(s) {
		return PanicPlaceholder
	}
	return h.r.String(s)
}

// fmtPanicRE matches the text fmt writes when a method panics.
var fmtPanicRE = regexp.MustCompile(`%!\w\(PANIC=(Format|Error|String|GoString) method: `)

// guardedFormatter runs a Formatter's Format and records a panic instead of
// letting fmt print the payload.
type guardedFormatter struct {
	f        fmt.Formatter
	panicked *bool
}

func (g guardedFormatter) Format(s fmt.State, verb rune) {
	defer func() {
		if recover() != nil {
			*g.panicked = true
		}
	}()
	g.f.Format(s, verb)
}

// PanicPlaceholder replaces a value whose custom conversion panicked.
const PanicPlaceholder = "!PANIC: log value conversion failed"

// byteSlice reports whether x is a []byte or a named byte-slice type, as
// slog's text handler and encoding/json treat both alike.
func byteSlice(x any) ([]byte, bool) {
	if bs, ok := x.([]byte); ok {
		return bs, true
	}
	t := reflect.TypeOf(x)
	if t != nil && t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return reflect.ValueOf(x).Bytes(), true
	}
	return nil, false
}

// unBase64 returns the bytes a string decodes to when it looks like the
// Base64 that encoding/json writes for byte slices nested in a value.
func unBase64(s string) ([]byte, bool) {
	if len(s) < 12 || len(s)%4 != 0 {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	return b, err == nil
}

// marshalJSON matches slog's JSON handler: no HTML escaping, no newline.
func marshalJSON(x any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(x); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sanitizeJSON re-encodes one JSON value with every string (object keys
// included) redacted. Key order, numbers, and structure are preserved, and
// the output is always valid JSON.
func (h *RedactHandler) sanitizeJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	type frame struct {
		obj bool
		n   int // tokens written in this container
	}
	var stack []frame
	var out bytes.Buffer
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			stack = stack[:len(stack)-1]
			out.WriteByte(byte(d))
			continue
		}
		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			switch {
			case top.obj && top.n%2 == 1:
				out.WriteByte(':')
			case top.n > 0:
				out.WriteByte(',')
			}
			top.n++
		}
		switch t := tok.(type) {
		case json.Delim:
			out.WriteByte(byte(t))
			stack = append(stack, frame{obj: t == '{'})
		case string:
			red := h.r.String(t)
			if red == t {
				// A byte slice nested in a struct or map arrives as Base64.
				if raw, ok := unBase64(t); ok {
					if rr := h.r.String(string(raw)); rr != string(raw) {
						red = base64.StdEncoding.EncodeToString([]byte(rr))
					}
				}
			}
			s, err := marshalJSON(red)
			if err != nil {
				return nil, err
			}
			out.Write(s)
		case json.Number:
			out.WriteString(t.String())
		case bool:
			out.WriteString(strconv.FormatBool(t))
		case nil:
			out.WriteString("null")
		}
	}
	return out.Bytes(), nil
}
