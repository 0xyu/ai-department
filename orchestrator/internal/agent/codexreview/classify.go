package codexreview

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/auth"
)

// Error texts Codex prints in "error" and "turn.failed" events (verified
// with codex-cli 0.162.0 where reproducible: logged out, invalid API key,
// unknown model). The usage-limit wording follows the CLI's message:
// "You've hit your usage limit. … try again in 2 hours 13 minutes." or
// "… try again at 3:05 PM." / "… try again at Nov 10th, 2026 3:05 PM.".
var (
	reAuth      = regexp.MustCompile(`(?i)\b401\b|unauthorized|not logged in|invalid_api_key|incorrect api key|token (has )?expired|refresh token|please (log|sign) ?in|codex login|authentication (failed|required)|\b403\b|forbidden`)
	reQuota     = regexp.MustCompile(`(?i)insufficient_quota|exceeded your current quota|billing|quota exceeded|payment required|\b402\b|out of credits`)
	reUsage     = regexp.MustCompile(`(?i)usage limit|usage_limit_reached|hit your (usage )?limit|usage_not_included`)
	reRateLimit = regexp.MustCompile(`(?i)\b429\b|rate[_ ]limit|too many requests`)
	reTransient = regexp.MustCompile(`(?i)\b5\d\d\b|overloaded|server_error|internal server error|service unavailable|bad gateway|gateway timeout|stream (disconnected|error)|connection (reset|refused|closed|error)|timed? ?out|network error|error sending request|econnreset`)
	reModel     = regexp.MustCompile(`(?i)model_not_found|model .* (is )?not supported|does not exist or you do not have access|unknown model`)

	reTryIn  = regexp.MustCompile(`(?i)try again in\s+((?:\d+\s*(?:days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b[\s,and]*)+)`)
	reDurPt  = regexp.MustCompile(`(?i)(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b`)
	reTryAt  = regexp.MustCompile(`(?i)try again at\s+(?:([A-Z][a-z]{2})\w*\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\s+)?(\d{1,2}):(\d{2})\s*(AM|PM)?`)
	reRetryS = regexp.MustCompile(`(?i)(?:retry[- ]after["':\s]+|try again in\s+)(\d+(?:\.\d+)?)\s*(ms|s|sec|seconds)?\b`)
)

// classify maps the stream's error text (and stderr) to an error class.
func classify(mode auth.Mode, text string) agent.ErrorClass {
	switch {
	case reUsage.MatchString(text):
		if mode == auth.API {
			return agent.ClassAPIRateLimit
		}
		return agent.ClassSubscriptionLimit
	case reQuota.MatchString(text):
		if mode == auth.API {
			return agent.ClassAPIQuota
		}
		return agent.ClassSubscriptionLimit
	case reRateLimit.MatchString(text):
		if mode == auth.API {
			return agent.ClassAPIRateLimit
		}
		return agent.ClassTransient
	case reAuth.MatchString(text):
		return agent.ClassAuth
	case reModel.MatchString(text):
		return agent.ClassCrash // a configuration error: retrying does not help
	case reTransient.MatchString(text):
		return agent.ClassTransient
	}
	return ""
}

func unit(u string) time.Duration {
	switch strings.ToLower(u[:1]) {
	case "d":
		return 24 * time.Hour
	case "h":
		return time.Hour
	case "m":
		if strings.HasPrefix(strings.ToLower(u), "ms") {
			return time.Millisecond
		}
		return time.Minute
	}
	return time.Second
}

var months = map[string]time.Month{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

// parseReset finds when a usage limit resets. Zero when unknown. A clock
// time without a date is today or, if already past, tomorrow (local time:
// Codex formats it in the user's zone).
func parseReset(text string, now time.Time) time.Time {
	if m := reTryIn.FindStringSubmatch(text); m != nil {
		var d time.Duration
		for _, p := range reDurPt.FindAllStringSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(p[1])
			d += time.Duration(n) * unit(p[2])
		}
		if d > 0 {
			return now.Add(d)
		}
	}
	if m := reTryAt.FindStringSubmatch(text); m != nil {
		h, _ := strconv.Atoi(m[4])
		mi, _ := strconv.Atoi(m[5])
		switch strings.ToUpper(m[6]) {
		case "PM":
			if h < 12 {
				h += 12
			}
		case "AM":
			if h == 12 {
				h = 0
			}
		}
		if h > 23 || mi > 59 {
			return time.Time{}
		}
		loc := now.Location()
		if m[1] != "" {
			mon, ok := months[strings.ToLower(m[1])]
			day, _ := strconv.Atoi(m[2])
			year, _ := strconv.Atoi(m[3])
			if !ok || day < 1 || day > 31 {
				return time.Time{}
			}
			return time.Date(year, mon, day, h, mi, 0, 0, loc)
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, loc)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return t
	}
	return time.Time{}
}

// parseRetryAfter finds a retry delay ("Please try again in 20s",
// "retry-after: 30").
func parseRetryAfter(text string) time.Duration {
	if m := reRetryS.FindStringSubmatch(text); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		if strings.EqualFold(m[2], "ms") {
			return time.Duration(f * float64(time.Millisecond))
		}
		return time.Duration(f * float64(time.Second))
	}
	if m := reTryIn.FindStringSubmatch(text); m != nil {
		var d time.Duration
		for _, p := range reDurPt.FindAllStringSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(p[1])
			d += time.Duration(n) * unit(p[2])
		}
		return d
	}
	return 0
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 300 {
				l = l[:300]
			}
			return l
		}
	}
	return ""
}
