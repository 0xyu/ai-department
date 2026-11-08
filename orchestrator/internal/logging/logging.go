// Package logging sets up log/slog with a handler that masks secrets and only
// writes values of known-safe types (see internal/secure for the rules).
//
// Logs are machine-oriented and written in English; operator-facing text goes
// through internal/i18n instead.
package logging

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"

	"github.com/0xyu/aco/internal/secure"
)

// Options configure a logger.
type Options struct {
	Level    slog.Level
	JSON     bool
	Redactor *secure.Redactor
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

// Placeholders written instead of values the handler will not log.
const (
	// PanicPlaceholder replaces an error whose Error method panicked. The
	// panic payload is never written.
	PanicPlaceholder = "!PANIC: log value conversion failed"
	// TruncatedPlaceholder replaces a group or slog.LogValuer result beyond
	// the per-record expansion limits (maxValueDepth, maxResolves).
	TruncatedPlaceholder = "[TRUNCATED: log value expansion limit]"
	// unloggablePrefix starts the placeholder for a value of a type the
	// allowlist does not cover: "[UNLOGGABLE <type>]".
	unloggablePrefix = "[UNLOGGABLE "
)

// Unloggable returns the placeholder written for a value of type t.
func Unloggable(t reflect.Type) string {
	return unloggablePrefix + t.String() + "]"
}

// Expansion limits shared by all attributes of one record (or one WithAttrs
// call). Plain values cost nothing, so ordinary attributes are always
// written even after a runaway LogValuer has used the budget up.
const (
	maxValueDepth = 16
	maxResolves   = 256
)

// budget tracks LogValue calls across one record.
type budget struct{ resolves int }

// RedactHandler wraps a slog.Handler. It masks the message, attribute keys,
// group names, and string values, and writes only allowlisted value types:
//
//   - strings, numbers, booleans, durations, and times (including named
//     types with those underlying kinds, read without calling any method)
//   - []string
//   - errors, through a single, panic-guarded call to Error()
//   - groups, and slog.LogValuer results (resolved by slog), checked
//     recursively against the same rules, within one expansion budget per
//     record: at most maxValueDepth nested groups and maxResolves LogValue
//     calls in all. Anything beyond is written as TruncatedPlaceholder, so
//     a LogValuer that returns itself inside a group cannot recurse forever.
//   - secure.Secret, which logs as secure.Mask
//
// Any other value is written as "[UNLOGGABLE <type>]" without being inspected
// or converted: no MarshalJSON, MarshalText, String, or Format method of an
// arbitrary value ever runs. To log a structured value, give its type a
// LogValue method that returns a group of allowlisted values.
type RedactHandler struct {
	inner slog.Handler
	r     *secure.Redactor
}

// NewRedactHandler wraps inner. A nil redactor masks known patterns only.
func NewRedactHandler(inner slog.Handler, r *secure.Redactor) *RedactHandler {
	if r == nil {
		r = secure.NewRedactor()
	}
	return &RedactHandler{inner: inner, r: r}
}

// Enabled implements slog.Handler.
func (h *RedactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *RedactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.String(rec.Message), rec.PC)
	b := &budget{}
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a, b, 0))
		return true
	})
	return h.inner.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h *RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	b := &budget{}
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = h.attr(a, b, 0)
	}
	return &RedactHandler{inner: h.inner.WithAttrs(red), r: h.r}
}

// WithGroup implements slog.Handler.
func (h *RedactHandler) WithGroup(name string) slog.Handler {
	return &RedactHandler{inner: h.inner.WithGroup(h.r.String(name)), r: h.r}
}

// attr masks one attribute at group depth depth.
func (h *RedactHandler) attr(a slog.Attr, b *budget, depth int) slog.Attr {
	return slog.Attr{Key: h.r.String(a.Key), Value: h.value(a.Value, b, depth)}
}

func (h *RedactHandler) value(v slog.Value, b *budget, depth int) slog.Value {
	if v.Kind() == slog.KindLogValuer {
		if b.resolves >= maxResolves {
			return slog.StringValue(TruncatedPlaceholder)
		}
		b.resolves++
		v = v.Resolve() // slog bounds LogValuer chains and recovers panics
	}
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(h.r.String(v.String()))
	case slog.KindGroup:
		if depth >= maxValueDepth {
			return slog.StringValue(TruncatedPlaceholder)
		}
		g := v.Group()
		red := make([]slog.Attr, len(g))
		for i, ga := range g {
			red[i] = h.attr(ga, b, depth+1)
		}
		return slog.GroupValue(red...)
	case slog.KindAny:
		return h.anyValue(v.Any())
	default: // int, uint, float, bool, duration, time
		return v
	}
}

// anyValue applies the allowlist to a value slog could not classify.
func (h *RedactHandler) anyValue(x any) slog.Value {
	switch t := x.(type) {
	case nil:
		return slog.AnyValue(nil)
	case error:
		return slog.StringValue(h.errorText(t))
	case []string:
		red := make([]string, len(t))
		for i, s := range t {
			red[i] = h.r.String(s)
		}
		return slog.AnyValue(red)
	}
	// Named basic types (type Mode string, type Phase int, ...): read the
	// underlying value by reflection, which calls no method.
	rv := reflect.ValueOf(x)
	switch rv.Kind() {
	case reflect.String:
		return slog.StringValue(h.r.String(rv.String()))
	case reflect.Bool:
		return slog.BoolValue(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return slog.Int64Value(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return slog.Uint64Value(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return slog.Float64Value(rv.Float())
	}
	return slog.StringValue(Unloggable(rv.Type()))
}

// errorText calls Error() once. A panic is contained: a nil pointer receiver
// gives "<nil>" (as slog does), anything else the fixed PanicPlaceholder.
func (h *RedactHandler) errorText(err error) (s string) {
	defer func() {
		if r := recover(); r != nil {
			if rv := reflect.ValueOf(err); rv.Kind() == reflect.Pointer && rv.IsNil() {
				s = "<nil>"
				return
			}
			s = PanicPlaceholder
		}
	}()
	return h.r.String(err.Error())
}
