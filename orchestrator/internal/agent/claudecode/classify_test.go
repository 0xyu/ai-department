package claudecode

import (
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/auth"
)

func TestClassifyText(t *testing.T) {
	cases := []struct {
		mode   auth.Mode
		text   string
		status int
		want   agent.ErrorClass
	}{
		{auth.API, "Invalid API key · Please run /login", 0, agent.ClassAuth},
		{auth.Subscription, "OAuth token has expired", 0, agent.ClassAuth},
		{auth.Subscription, "x", 401, agent.ClassAuth},
		{auth.API, "Credit balance is too low", 0, agent.ClassAPIQuota},
		{auth.Subscription, "You've hit your limit · resets 4am", 0, agent.ClassSubscriptionLimit},
		{auth.Subscription, "Claude AI usage limit reached|1794197400", 0, agent.ClassSubscriptionLimit},
		{auth.Subscription, "5-hour limit reached ∙ resets 3pm", 0, agent.ClassSubscriptionLimit},
		{auth.API, "API Error: 429 rate_limit_error", 0, agent.ClassAPIRateLimit},
		{auth.Subscription, "API Error: 429 rate_limit_error", 0, agent.ClassTransient},
		{auth.Subscription, "API Error: 529 Overloaded", 0, agent.ClassTransient},
		{auth.Subscription, "x", 503, agent.ClassTransient},
		{auth.Subscription, "fetch failed: ECONNRESET", 0, agent.ClassTransient},
		{auth.Subscription, "something odd", 0, ""},
	}
	for _, c := range cases {
		if got := classifyText(c.mode, c.text, c.status); got != c.want {
			t.Errorf("%s %q %d: got %q, want %q", c.mode, c.text, c.status, got, c.want)
		}
	}
}

func TestParseReset(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) // 11:00 in Berlin
	cases := map[string]time.Time{
		"Claude AI usage limit reached|1794197400":           time.Unix(1794197400, 0),
		"You've hit your limit · resets 3pm (Europe/Berlin)": time.Date(2026, 11, 9, 15, 0, 0, 0, berlin),
		"resets 9am (Europe/Berlin)":                         time.Date(2026, 11, 10, 9, 0, 0, 0, berlin),
		"resets at 10:30 PM (UTC)":                           time.Date(2026, 11, 9, 22, 30, 0, 0, time.UTC),
		"limit reached, resets 12am (UTC)":                   time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		"try again in 20 minutes":                            now.Add(20 * time.Minute),
		"no time here":                                       {},
		"resets 27pm":                                        {},
	}
	for text, want := range cases {
		if got := parseReset(text, now); !got.Equal(want) {
			t.Errorf("%q: got %v, want %v", text, got, want)
		}
	}
	if d := parseRetryAfter(`{"retry-after": 12}`); d != 12*time.Second {
		t.Errorf("retry-after: %v", d)
	}
	if d := parseRetryAfter("please retry in 2 minutes"); d != 2*time.Minute {
		t.Errorf("retry in: %v", d)
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		"Here:\n```json\n{\"a\":1}\n```\nbye": `{"a":1}`,
		`prefix {"b":2} suffix`:               `{"b":2}`,
		"no json":                             "",
		"{broken":                             "",
	}
	for text, want := range cases {
		got := extractJSON(&message{Result: text})
		if string(got) != want {
			t.Errorf("%q: got %s, want %s", text, got, want)
		}
	}
	if got := extractJSON(&message{StructuredOutput: []byte(`{"s":1}`), Result: `{"r":1}`}); string(got) != `{"s":1}` {
		t.Errorf("structured_output must win: %s", got)
	}
	if got := extractJSON(&message{StructuredOutput: []byte(`null`)}); got != nil {
		t.Errorf("null structured output: %s", got)
	}
}
