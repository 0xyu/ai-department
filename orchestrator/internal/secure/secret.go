// Package secure is ACO's security utility layer: one place for how secrets
// are held, masked, and kept out of every output.
//
// The rules it enforces (with guard tests in this package):
//
//  1. Secrets are typed at the source. Credentials read from the environment
//     or the env file become a Secret, which prints, marshals, and logs as
//     Mask in every form. The raw value is reached only by Reveal, which
//     may be called only from the packages listed in revealAllowed (guard_test.go).
//  2. Every output goes through a sink that masks: operator text through
//     i18n.Printer with a Redactor filter, logs through internal/logging's
//     allowlist handler. Nothing else writes to stdout/stderr or creates a
//     slog logger, and only listed files read the process environment.
//  3. Diagnostics name fields, flags, and variables, never user-supplied
//     values that may be credentials.
//  4. One shared corpus (package securetest) checks every sink for a fake
//     credential in each encoding it could take: raw, quoted, JSON-escaped,
//     Base64, hex, decimal byte lists, URL-encoded.
package secure

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Secret holds a credential. Every way of printing or encoding it yields
// Mask. A Secret holds only a handle; the value lives in a private table in
// this package. So even where fmt cannot call Secret's methods (an
// unexported field, printed with any verb) it can print nothing but the
// handle number, and encoding/json skips the unexported field.
type Secret struct{ id uint64 }

var (
	vault   sync.Map // uint64 -> string
	vaultID atomic.Uint64
)

// NewSecret wraps a credential. An empty string gives the zero Secret.
func NewSecret(v string) Secret {
	if v == "" {
		return Secret{}
	}
	id := vaultID.Add(1)
	vault.Store(id, v)
	return Secret{id}
}

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.id == 0 }

// Reveal returns the raw credential. Only packages listed in revealAllowed
// (guard_test.go) may call it; use it only to hand the credential to the
// process that needs it, never to build output.
func (s Secret) Reveal() string {
	if v, ok := vault.Load(s.id); ok {
		return v.(string)
	}
	return ""
}

// String implements fmt.Stringer.
func (s Secret) String() string { return Mask }

// GoString implements fmt.GoStringer (%#v).
func (s Secret) GoString() string { return "secure.Secret(" + Mask + ")" }

// Format implements fmt.Formatter, so every verb (%s %v %+v %q %x %d ...) prints Mask.
func (s Secret) Format(f fmt.State, verb rune) {
	out := Mask
	if verb == 'q' {
		out = `"` + Mask + `"`
	}
	_, _ = f.Write([]byte(out))
}

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Mask + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Mask), nil }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Mask) }
