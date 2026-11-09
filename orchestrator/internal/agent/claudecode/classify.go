package claudecode

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/auth"
)

var (
	reAuth      = regexp.MustCompile(`(?i)invalid api key|invalid x-api-key|please run /login|not logged in|authentication[_ ]?(error|failed)|oauth token (has )?expired|api error: 40[13]\b|unauthorized|credentials? (are )?(missing|invalid)`)
	reQuota     = regexp.MustCompile(`(?i)credit balance is too low|insufficient[_ ]quota|exceeded your current quota|billing|spend(ing)? limit|payment required|api error: 402\b`)
	reSubLimit  = regexp.MustCompile(`(?i)usage limit reached|you'?ve hit your (usage )?limit|(5-hour|five[- ]hour|weekly|session|opus) limit reached|limit reached\W.*resets|claude ai usage limit`)
	reRateLimit = regexp.MustCompile(`(?i)api error: 429\b|rate[_ ]limit(ed|_error)?|too many requests`)
	reTransient = regexp.MustCompile(`(?i)overloaded|api error: (5\d\d)\b|\b529\b|econnreset|etimedout|econnrefused|socket hang up|network error|fetch failed|connection (error|reset|refused)|internal server error|service unavailable|bad gateway|gateway timeout|request timed out`)
	reMaxTurns  = regexp.MustCompile(`(?i)max(imum)? turns`)

	reResetUnix  = regexp.MustCompile(`(?i)limit reached\|(\d{10})`)
	reResetClock = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([A-Za-z_]+/[A-Za-z_/+-]+|UTC)\))?`)
	reTryIn      = regexp.MustCompile(`(?i)(?:try again|retry) in\s+(\d+)\s*(second|sec|s|minute|min|m|hour|hr|h)`)
	reRetryAfter = regexp.MustCompile(`(?i)retry[- ]after["':\s]+(\d+)`)
)

// classifyText maps an error text (result text, assistant error, stderr)
// and the API status to an error class. Empty class: unknown.
func classifyText(mode auth.Mode, text string, status int) agent.ErrorClass {
	switch {
	case status == 401 || status == 403 || reAuth.MatchString(text):
		return agent.ClassAuth
	case status == 402 || reQuota.MatchString(text):
		if mode == auth.API {
			return agent.ClassAPIQuota
		}
		return agent.ClassSubscriptionLimit
	case reSubLimit.MatchString(text):
		if mode == auth.API {
			return agent.ClassAPIRateLimit
		}
		return agent.ClassSubscriptionLimit
	case status == 429 || reRateLimit.MatchString(text):
		if mode == auth.API {
			return agent.ClassAPIRateLimit
		}
		return agent.ClassTransient
	case status >= 500 || reTransient.MatchString(text):
		return agent.ClassTransient
	case reMaxTurns.MatchString(text):
		return agent.ClassMaxTurns
	}
	return ""
}

// parseReset finds when a usage limit resets: a Unix timestamp
// ("limit reached|1736953200"), a clock time ("resets 3pm (Europe/Berlin)"),
// or a delay ("try again in 20 minutes"). Zero when unknown.
func parseReset(text string, now time.Time) time.Time {
	if m := reResetUnix.FindStringSubmatch(text); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return time.Unix(n, 0)
		}
	}
	if m := reTryIn.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := time.Second
		switch strings.ToLower(m[2][:1]) {
		case "m":
			unit = time.Minute
		case "h":
			unit = time.Hour
		}
		return now.Add(time.Duration(n) * unit)
	}
	if m := reResetClock.FindStringSubmatch(text); m != nil {
		h, _ := strconv.Atoi(m[1])
		min := 0
		if m[2] != "" {
			min, _ = strconv.Atoi(m[2])
		}
		switch strings.ToLower(m[3]) {
		case "pm":
			if h < 12 {
				h += 12
			}
		case "am":
			if h == 12 {
				h = 0
			}
		}
		if h > 23 || min > 59 {
			return time.Time{}
		}
		loc := now.Location()
		if m[4] != "" {
			if l, err := time.LoadLocation(m[4]); err == nil {
				loc = l
			}
		}
		n := now.In(loc)
		t := time.Date(n.Year(), n.Month(), n.Day(), h, min, 0, 0, loc)
		if !t.After(n) {
			t = t.Add(24 * time.Hour)
		}
		return t
	}
	return time.Time{}
}

// parseRetryAfter finds a retry delay in an error text.
func parseRetryAfter(text string) time.Duration {
	if m := reRetryAfter.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Duration(n) * time.Second
	}
	if m := reTryIn.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch strings.ToLower(m[2][:1]) {
		case "m":
			return time.Duration(n) * time.Minute
		case "h":
			return time.Duration(n) * time.Hour
		}
		return time.Duration(n) * time.Second
	}
	return 0
}

// outcome classifies a finished stream. It returns nil when the invocation
// succeeded (a success result); the caller then checks the structured output.
func outcome(mode auth.Mode, st *stream, stderr string, exited error, now time.Time) *agent.Error {
	r := st.result
	if r != nil && !r.IsError && r.Subtype == "success" {
		return nil
	}
	e := &agent.Error{Agent: "claude"}
	var text string
	status := 0
	if r != nil {
		text = r.Result + "\n" + strings.Join(r.Errors, "\n")
		status = r.apiStatus()
		switch r.Subtype {
		case "error_max_turns":
			e.Class, e.Message = agent.ClassMaxTurns, "turn limit reached"
			return e
		case "error_max_budget_usd":
			e.Class, e.Message = agent.ClassBudget, "the CLI's --max-budget-usd was reached"
			return e
		case "error_max_structured_output_retries":
			e.Class, e.Message = agent.ClassIncomplete, "no schema-valid structured output after the CLI's retries"
			return e
		}
	}
	text += "\n" + st.assistantErr + "\n" + st.lastText + "\n" + stderr
	if st.assistantErr == "authentication_failed" {
		status = 401
	}
	class := classifyText(mode, text, status)
	if st.rejected != nil && (class == "" || class == agent.ClassTransient || class == agent.ClassSubscriptionLimit || class == agent.ClassAPIRateLimit) {
		if mode == auth.API {
			class = agent.ClassAPIRateLimit
		} else {
			class = agent.ClassSubscriptionLimit
		}
		if st.rejected.RateLimit.ResetsAt > 0 {
			e.ResetAt = time.Unix(st.rejected.RateLimit.ResetsAt, 0)
		}
	}
	switch {
	case class != "":
		e.Class = class
	case r == nil && exited == nil:
		e.Class, e.Message = agent.ClassCrash, "the CLI exited without a result message"
		return e
	case r == nil:
		e.Class, e.Message = agent.ClassCrash, "the CLI exited abnormally without a result message"
		return e
	default:
		e.Class = agent.ClassCrash
	}
	switch e.Class {
	case agent.ClassSubscriptionLimit:
		if e.ResetAt.IsZero() {
			e.ResetAt = parseReset(text, now)
		}
		e.Message = "subscription usage limit reached"
	case agent.ClassAPIRateLimit, agent.ClassTransient:
		e.RetryAfter = parseRetryAfter(text)
		e.Message = firstLine(text)
	default:
		e.Message = firstLine(text)
	}
	return e
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
