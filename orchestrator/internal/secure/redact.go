// Known secret values (from the
// environment or env file) and well-known credential patterns are masked by Redactor.
package secure

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Mask replaces every redacted secret.
const Mask = "[REDACTED]"

// minSecretLen avoids masking short values such as "1" or "true" in free
// text. Shorter registered secrets are still never echoed (see Echo).
const minSecretLen = 8

// SecretEnvVars are environment variables whose values are always secret.
var SecretEnvVars = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"CODEX_API_KEY",
	"ACO_API_TOKEN",
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
}

// secretNameRE flags other env vars that by name look secret.
var secretNameRE = regexp.MustCompile(`(?i)(_KEY|_TOKEN|_SECRET|PASSWORD|_PASS)$`)

// patterns are well-known credential formats. Order matters: the more
// specific Anthropic pattern runs before the generic "sk-" one.
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

// Redactor masks secrets in text: registered secret values (and their
// quoted, JSON-escaped, and URL-escaped forms) and the credential patterns.
// The zero value masks patterns only. It is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values []string // sorted longest first
	short  []string // registered secrets shorter than minSecretLen
}

// NewRedactor returns a Redactor that also masks the given secrets.
func NewRedactor(secrets ...Secret) *Redactor {
	r := &Redactor{}
	r.AddSecret(secrets...)
	return r
}

// AddSecret registers more secrets.
func (r *Redactor) AddSecret(secrets ...Secret) {
	vals := make([]string, 0, len(secrets))
	for _, s := range secrets {
		vals = append(vals, s.Reveal())
	}
	r.add(vals...)
}

func (r *Redactor) add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if len(v) < minSecretLen {
			if !slices.Contains(r.short, v) {
				r.short = append(r.short, v)
			}
			continue
		}
		for _, form := range encodedForms(v) {
			if !slices.Contains(r.values, form) {
				r.values = append(r.values, form)
			}
		}
	}
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

// encodedForms lists v as it may appear in output: raw, inside a Go or JSON
// quoted string, and URL-escaped.
func encodedForms(v string) []string {
	forms := []string{v}
	q := strconv.Quote(v)
	forms = append(forms, q[1:len(q)-1])
	if j, err := json.Marshal(v); err == nil {
		forms = append(forms, string(j[1:len(j)-1]))
	}
	forms = append(forms, url.QueryEscape(v), url.PathEscape(v))
	out := forms[:0]
	for _, f := range forms {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// AddFromEnv registers the values of secret-looking variables from env.
func (r *Redactor) AddFromEnv(env map[string]string) {
	var vals []string
	for k, v := range env {
		if IsSecretVar(k) {
			vals = append(vals, v)
		}
	}
	r.add(vals...)
}

// IsSecretVar reports whether an environment variable name holds a secret.
func IsSecretVar(name string) bool {
	for _, s := range SecretEnvVars {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return secretNameRE.MatchString(name)
}

// String masks all secrets in s.
func (r *Redactor) String(s string) string {
	if r != nil {
		r.mu.RLock()
		defer r.mu.RUnlock()
		for _, v := range r.values {
			if strings.Contains(s, v) {
				s = strings.ReplaceAll(s, v, Mask)
			}
		}
	}
	for _, p := range patterns {
		s = p.ReplaceAllString(s, Mask)
	}
	return s
}
