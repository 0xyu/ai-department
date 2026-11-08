package logging

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// The byte scan looks for secrets hidden in byte data that text masking
// cannot see once it is formatted: %+v and encoding/json print byte arrays,
// integer slices, and slices of named byte types as decimal lists. It walks
// values by reflection only, so it never runs a custom method.
//
// It fails closed: a value too deep or too large to inspect completely is
// reported as incomplete, and callers replace it with a placeholder.

// scanMode picks which printer's rules the scan follows.
type scanMode int

const (
	scanText scanMode = iota // fmt %+v (slog's text handler)
	scanJSON                 // encoding/json (slog's JSON handler)
)

const (
	maxScanDepth = 32
	maxScanNodes = 10000
)

// UninspectablePlaceholder replaces a value too deep or too large for the
// byte scan to inspect completely.
const UninspectablePlaceholder = "[REDACTED: value too deep or too large to inspect]"

// BinarySecretPlaceholder replaces a value whose nested byte data holds a
// secret that the selected output form cannot mask.
const BinarySecretPlaceholder = "[REDACTED: value holds a secret in binary data]"

// scanResult reports what the scan found.
type scanResult struct {
	dirty      bool // some byte data holds a secret
	unmaskable bool // ...and the printer would write it in a form masking cannot reach
	incomplete bool // depth or node budget ran out
}

var (
	formatterType = reflect.TypeOf((*fmt.Formatter)(nil)).Elem()
	stringerType  = reflect.TypeOf((*fmt.Stringer)(nil)).Elem()
	errorType     = reflect.TypeOf((*error)(nil)).Elem()
	jsonMarshType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

func implementsJSONConv(t reflect.Type) bool {
	return t.Implements(jsonMarshType) || t.Implements(textMarshType) ||
		reflect.PointerTo(t).Implements(jsonMarshType) || reflect.PointerTo(t).Implements(textMarshType)
}

// stops reports whether the printer renders v through a method (so its
// contents are never printed raw). fmt calls methods only on values it can
// reach (exported); encoding/json also skips unexported fields entirely.
func (m scanMode) stops(v reflect.Value, unexported bool) bool {
	t := v.Type()
	if m == scanJSON {
		if unexported || t.Implements(jsonMarshType) || t.Implements(textMarshType) {
			return true
		}
		// encoding/json uses pointer-receiver methods only on addressable values.
		p := reflect.PointerTo(t)
		return v.CanAddr() && (p.Implements(jsonMarshType) || p.Implements(textMarshType))
	}
	if !v.CanInterface() {
		return false
	}
	return t.Implements(formatterType) || t.Implements(stringerType) || t.Implements(errorType)
}

// jsonMaskable reports whether encoding/json writes a dirty byte sequence of
// type t as Base64 (which sanitizeJSON decodes and masks). That holds only
// for slices of a uint8 kind whose element has no JSON or text marshaler;
// arrays, other integer types, and marshaling elements become number lists.
func jsonMaskable(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 && !implementsJSONConv(t.Elem())
}

func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return false
}

func intOf(v reflect.Value) (int64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := v.Uint()
		if u > 255 {
			return 256, true
		}
		return int64(u), true
	}
	return 0, false
}

// mayHoldBytes reports whether a value of type t can contain byte data the
// scan cares about. Types that cannot (strings, numbers, []string, ...) are
// skipped without spending the node budget.
var mayHoldCache sync.Map // reflect.Type -> bool

func mayHoldBytes(t reflect.Type) bool {
	if v, ok := mayHoldCache.Load(t); ok {
		return v.(bool)
	}
	r := mayHold(t, map[reflect.Type]bool{})
	mayHoldCache.Store(t, r)
	return r
}

func mayHold(t reflect.Type, visiting map[reflect.Type]bool) bool {
	if visiting[t] {
		return true // recursive type: assume it can
	}
	visiting[t] = true
	defer delete(visiting, t)
	switch t.Kind() {
	case reflect.Interface:
		return true
	case reflect.Slice, reflect.Array:
		return isIntKind(t.Elem().Kind()) || mayHold(t.Elem(), visiting)
	case reflect.Pointer:
		return mayHold(t.Elem(), visiting)
	case reflect.Map:
		return mayHold(t.Key(), visiting) || mayHold(t.Elem(), visiting)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if mayHold(t.Field(i).Type, visiting) {
				return true
			}
		}
	}
	return false
}

// viewKey identifies a visited pointer, slice view, or map. Slices sharing a
// start address but differing in length are different views (s[:1] vs s),
// and a pointer to a struct differs from a pointer to its first field.
type viewKey struct {
	ptr uintptr
	typ reflect.Type
	n   int
}

type byteScan struct {
	h       *RedactHandler
	mode    scanMode
	seen    map[viewKey]bool
	visited int
	res     scanResult
}

// scanBytes scans x under mode's printing rules.
func (h *RedactHandler) scanBytes(x any, mode scanMode) scanResult {
	s := byteScan{h: h, mode: mode, seen: map[viewKey]bool{}}
	s.walk(reflect.ValueOf(x), 0, false)
	return s.res
}

func (s *byteScan) done() bool { return s.res.incomplete || s.res.unmaskable }

func (s *byteScan) once(k viewKey) bool {
	if s.seen[k] {
		return false
	}
	s.seen[k] = true
	return true
}

func (s *byteScan) walk(v reflect.Value, depth int, unexported bool) {
	if !v.IsValid() || s.done() || !mayHoldBytes(v.Type()) {
		return
	}
	if depth > maxScanDepth || s.visited >= maxScanNodes {
		s.res.incomplete = true
		return
	}
	s.visited++
	if depth > 0 && s.mode.stops(v, unexported) {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() && s.once(viewKey{v.Pointer(), v.Type(), 0}) {
			s.walk(v.Elem(), depth+1, unexported)
		}
	case reflect.Interface:
		if !v.IsNil() {
			s.walk(v.Elem(), depth+1, unexported)
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice {
			if v.IsNil() || !s.once(viewKey{v.Pointer(), v.Type(), v.Len()}) {
				return
			}
		}
		if s.byteSequence(v) {
			return
		}
		for i := 0; i < v.Len() && !s.done(); i++ {
			s.walk(v.Index(i), depth+1, unexported)
		}
	case reflect.Map:
		if v.IsNil() || !s.once(viewKey{v.Pointer(), v.Type(), -1}) {
			return
		}
		iter := v.MapRange()
		for iter.Next() && !s.done() {
			s.walk(iter.Key(), depth+1, unexported)
			s.walk(iter.Value(), depth+1, unexported)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField() && !s.done(); i++ {
			s.walk(v.Field(i), depth+1, unexported || !t.Field(i).IsExported())
		}
	}
}

// byteSequence checks a slice or array whose elements are all integers in
// 0..255 (a statically integer element type, or interface elements holding
// integers). It returns true when v was such a sequence.
func (s *byteScan) byteSequence(v reflect.Value) bool {
	n := v.Len()
	if n == 0 {
		return isIntKind(v.Type().Elem().Kind())
	}
	static := isIntKind(v.Type().Elem().Kind())
	if !static && v.Type().Elem().Kind() != reflect.Interface {
		return false
	}
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		e := v.Index(i)
		if !static {
			if e.IsNil() {
				return false
			}
			e = e.Elem()
		}
		x, ok := intOf(e)
		if !ok {
			return false
		}
		if x < 0 || x > 255 {
			return true // integers, but not bytes: nothing to check
		}
		b[i] = byte(x)
	}
	if s.h.r.String(string(b)) == string(b) {
		return true
	}
	s.res.dirty = true
	if s.mode == scanText || !static || !jsonMaskable(v.Type()) {
		s.res.unmaskable = true
	}
	return true
}
