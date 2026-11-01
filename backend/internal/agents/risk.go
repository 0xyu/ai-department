package agents

import (
	"regexp"
	"strings"
)

type riskRule struct {
	category string
	re       *regexp.Regexp
}

// High-risk shell patterns: delete, deploy/remote, send externally, spend money.
var shellRisk = []riskRule{
	{"删除", regexp.MustCompile(`(?i)(^|[;&|\s(])(rm|rmdir|unlink|shred)\s`)},
	{"删除", regexp.MustCompile(`(?i)git\s+(clean|reset\s+--hard|branch\s+-D)`)},
	{"删除", regexp.MustCompile(`(?i)find\s.*-delete`)},
	{"部署/远程", regexp.MustCompile(`(?i)(^|[;&|\s(])(ssh|scp|rsync|docker|kubectl|helm|terraform|ansible(-playbook)?|systemctl|fly|vercel|heroku|netlify)\s`)},
	{"部署/远程", regexp.MustCompile(`(?i)\bdeploy\b`)},
	{"对外发送", regexp.MustCompile(`(?i)git\s+push`)},
	{"对外发送", regexp.MustCompile(`(?i)(npm|yarn|pnpm)\s+publish|twine\s+upload|gh\s+(release|pr|issue)\s+create`)},
	{"对外发送", regexp.MustCompile(`(?i)curl\b.*(-X\s*(POST|PUT|PATCH|DELETE)|--data|\s-d\s|\s-F\s|--form|\s-T\s|--upload-file)`)},
	{"对外发送", regexp.MustCompile(`(?i)wget\b.*--(post|method)`)},
	{"对外发送", regexp.MustCompile(`(?i)\b(sendmail|mailx?|smtp)\b`)},
	{"花钱", regexp.MustCompile(`(?i)\b(purchase|billing|checkout|stripe|paypal)\b`)},
}

var heredocStart = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// stripHeredocs removes heredoc bodies: they are file contents being written, not commands.
func stripHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	var term string
	for _, l := range lines {
		if term != "" {
			if strings.TrimSpace(l) == term {
				term = ""
				out = append(out, l)
			}
			continue
		}
		out = append(out, l)
		if m := heredocStart.FindStringSubmatch(l); m != nil {
			term = m[1]
		}
	}
	return strings.Join(out, "\n")
}

// ShellRisk returns the risk category of a shell command, or "" if it is low risk.
func ShellRisk(cmd string) string {
	cmd = stripHeredocs(cmd)
	for _, r := range shellRisk {
		if r.re.MatchString(cmd) {
			return r.category
		}
	}
	return ""
}
