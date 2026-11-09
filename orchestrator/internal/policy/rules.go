package policy

import (
	"path"
	"regexp"
	"strings"

	"github.com/0xyu/aco/internal/secure"
)

type secretRule struct {
	name     string
	desc     string
	re       *regexp.Regexp
	blocking bool
}

// secretRules scan added lines. Every rule except generic_assignment blocks.
var secretRules = []secretRule{
	{"aws_access_key", "an AWS access key ID", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA)[0-9A-Z]{16}\b`), true},
	{"aws_secret_key", "an AWS secret access key", regexp.MustCompile(`(?i)aws_?secret_?(?:access_?)?key["']?\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}\b`), true},
	{"private_key", "a private key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`), true},
	{"anthropic_key", "an Anthropic API key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`), true},
	{"openai_key", "an OpenAI-style API key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_\-]{16,}`), true},
	{"github_token", "a GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), true},
	{"slack_token", "a Slack token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`), true},
	{"google_api_key", "a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), true},
	{"stripe_key", "a Stripe live key", regexp.MustCompile(`\b[sr]k_live_[0-9A-Za-z]{20,}`), true},
	{"generic_assignment", "a hard-coded credential-like value", regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|passwd|password|client[_-]?secret)\b["']?\s*[:=]\s*["'][^"'\s]{16,}["']`), false},
}

// placeholderRE marks obvious example values that the generic rule skips.
var placeholderRE = regexp.MustCompile(`(?i)example|placeholder|changeme|dummy|xxxx|\$\{|<[a-z_]+>|your[_-]`)

// scanSecrets returns the secret rules that match line. A registered secret
// value (from the environment) matched by no pattern is reported as
// registered_secret.
func scanSecrets(line string, red *secure.Redactor) []secretRule {
	var out []secretRule
	for _, r := range secretRules {
		if !r.re.MatchString(line) {
			continue
		}
		if r.name == "generic_assignment" && placeholderRE.MatchString(line) {
			continue
		}
		if r.name == "openai_key" && strings.Contains(line, "sk-ant-") && !hasOtherSK(line) {
			continue // already reported as anthropic_key
		}
		out = append(out, r)
	}
	if len(out) == 0 && red != nil && red.String(line) != line {
		out = append(out, secretRule{name: "registered_secret", desc: "a secret value from the environment", blocking: true})
	}
	return out
}

var skRE = regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}`)

func hasOtherSK(line string) bool {
	for _, m := range skRE.FindAllString(line, -1) {
		if !strings.HasPrefix(m, "sk-ant-") {
			return true
		}
	}
	return false
}

var secretFileNames = []string{
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
	".env", ".env.*", ".netrc", ".pgpass", ".npmrc", ".pypirc", "credentials", "credentials.json", "*.kdbx",
}

var safeEnvSuffixes = []string{".example", ".sample", ".template", ".dist"}

// secretFileRule returns the matching secret file-name pattern, or "".
func secretFileRule(p string) string {
	base := strings.ToLower(path.Base(p))
	for _, s := range safeEnvSuffixes {
		if strings.HasSuffix(base, s) {
			return ""
		}
	}
	for _, pat := range secretFileNames {
		if ok, _ := path.Match(pat, base); ok {
			return pat
		}
	}
	return ""
}

type gamingRule struct {
	code string
	name string
	desc string
	re   *regexp.Regexp
}

// gamingRules flag added lines that skip tests or suppress findings.
var gamingRules = []gamingRule{
	{CodeTestSkipped, "go_skip", "Go test skipped (t.Skip)", regexp.MustCompile(`\b[tbf]\.Skip(?:Now|f)?\(`)},
	{CodeTestSkipped, "pytest_skip", "pytest test skipped", regexp.MustCompile(`@pytest\.mark\.(?:skip|skipif|xfail)\b|\bpytest\.(?:skip|xfail)\(`)},
	{CodeTestSkipped, "unittest_skip", "unittest test skipped", regexp.MustCompile(`@unittest\.(?:skip|skipIf|skipUnless|expectedFailure)\b|\.skipTest\(`)},
	{CodeTestSkipped, "js_skip", "JavaScript test skipped (.skip / x-prefix)", regexp.MustCompile(`\b(?:it|test|describe|context|suite)\.(?:skip|todo)\s*\(|\bx(?:it|test|describe|context)\s*\(`)},
	{CodeTestSkipped, "js_only", "JavaScript test focused (.only), other tests no longer run", regexp.MustCompile(`\b(?:it|test|describe|context|suite)\.only\s*\(|\bf(?:it|describe)\s*\(`)},
	{CodeTestSkipped, "junit_disabled", "JUnit test disabled", regexp.MustCompile(`@(?:Disabled|Ignore)\b`)},
	{CodeTestSkipped, "rust_ignore", "Rust test ignored", regexp.MustCompile(`#\[ignore\b`)},
	{CodeSuppression, "eslint_disable", "ESLint rule disabled (eslint-disable)", regexp.MustCompile(`eslint-disable`)},
	{CodeSuppression, "nosec", "security finding suppressed (#nosec)", regexp.MustCompile(`#\s*nosec\b`)},
	{CodeSuppression, "nolint", "linter suppressed (nolint)", regexp.MustCompile(`//\s*nolint\b`)},
	{CodeSuppression, "noqa", "linter suppressed (noqa)", regexp.MustCompile(`#\s*noqa\b`)},
	{CodeSuppression, "type_ignore", "type check suppressed (type: ignore)", regexp.MustCompile(`#\s*type:\s*ignore\b`)},
	{CodeSuppression, "ts_ignore", "TypeScript check suppressed", regexp.MustCompile(`@ts-(?:ignore|nocheck|expect-error)\b`)},
	{CodeSuppression, "nosemgrep", "Semgrep finding suppressed", regexp.MustCompile(`nosemgrep\b`)},
	{CodeSuppression, "lint_ignore_staticcheck", "staticcheck suppressed (lint:ignore)", regexp.MustCompile(`//\s*lint:ignore\b`)},
}

func scanGaming(line string) []gamingRule {
	var out []gamingRule
	for _, r := range gamingRules {
		if r.re.MatchString(line) {
			out = append(out, r)
		}
	}
	return out
}
