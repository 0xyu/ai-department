// Package redact masks secrets in text: known secret values (from the
// environment or env file) and well-known credential patterns.
package redact

import (
	"regexp"
	"sort"
	"strings"
)

// Mask replaces every redacted secret.
const Mask = "[REDACTED]"

// minSecretLen avoids masking short values such as "1" or "true".
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

// Redactor masks secrets. The zero value masks patterns only.
type Redactor struct {
	values []string // sorted longest first
}

// New returns a Redactor that also masks the given literal values.
func New(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add registers more literal secret values.
func (r *Redactor) Add(values ...string) {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) >= minSecretLen {
			r.values = append(r.values, v)
		}
	}
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

// AddFromEnv registers the values of secret-looking variables from env.
func (r *Redactor) AddFromEnv(env map[string]string) {
	var vals []string
	for k, v := range env {
		if IsSecretVar(k) {
			vals = append(vals, v)
		}
	}
	r.Add(vals...)
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
