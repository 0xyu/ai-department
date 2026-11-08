package secure

import (
	"regexp"
	"strings"
)

// Hidden is shown instead of a rejected value that may be a credential.
const Hidden = "[hidden]"

// maxEchoLen is the longest rejected value a diagnostic repeats. Valid
// choices (auth modes, efforts, languages, model IDs) are short; credentials
// are long.
const maxEchoLen = 20

var echoSafeRE = regexp.MustCompile(`^[A-Za-z0-9._:/+-]*$`)

// Echo returns v when a diagnostic may repeat it back to the operator, and
// Hidden otherwise. v is repeated only when it is short, uses plain
// characters, matches no credential pattern, and contains no secret
// registered with r, however short. Use it for every user-supplied value a
// diagnostic would quote. A nil Redactor checks the patterns only.
func (r *Redactor) Echo(v string) string {
	if len(v) > maxEchoLen || !echoSafeRE.MatchString(v) || r.String(v) != v || r.containsShort(v) {
		return Hidden
	}
	return v
}

// containsShort reports whether v contains a registered secret too short
// for String to mask.
func (r *Redactor) containsShort(v string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.short {
		if strings.Contains(v, s) {
			return true
		}
	}
	return false
}
