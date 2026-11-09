package agent

import (
	"errors"
	"fmt"
	"time"
)

// ErrorClass is how a failed invocation is handled (§14.1).
type ErrorClass string

// Error classes.
const (
	// ClassTransient: overloaded, 5xx, network. Back off and retry.
	ClassTransient ErrorClass = "transient"
	// ClassSubscriptionLimit: the subscription's usage limit; ResetAt when known.
	ClassSubscriptionLimit ErrorClass = "subscription_limit"
	// ClassAPIRateLimit: 429 in api mode; RetryAfter when known.
	ClassAPIRateLimit ErrorClass = "api_rate_limit"
	// ClassAPIQuota: billing or quota exhausted in api mode. Run fails (api_quota).
	ClassAPIQuota ErrorClass = "api_quota"
	// ClassMaxTurns: the turn limit ended the session; it can be resumed.
	ClassMaxTurns ErrorClass = "max_turns"
	// ClassIncomplete: no structured output, or it fails the schema.
	ClassIncomplete ErrorClass = "incomplete"
	// ClassCrash: the CLI exited without a result or with an unexpected error.
	ClassCrash ErrorClass = "crash"
	// ClassTimeout: the invocation or idle timeout killed the process tree.
	ClassTimeout ErrorClass = "timeout"
	// ClassAuth: not logged in, missing key, or authenticated in the other mode.
	ClassAuth ErrorClass = "auth"
	// ClassBudget: the USD budget is used up (api mode).
	ClassBudget ErrorClass = "budget_exceeded"
	// ClassCancelled: the context was cancelled; the process tree was killed.
	ClassCancelled ErrorClass = "cancelled"
	// ClassSchemaInvalid: the reviewer's answer failed the review schema
	// after the retries with validation errors and the fallback model
	// (§14.1 malformed review). The attempt is exhausted.
	ClassSchemaInvalid ErrorClass = "schema_invalid"
	// ClassPolicy: the reviewer read outside its confinement, wrote, or used
	// a tool it may not (§16.3); the review was discarded, also on retry.
	ClassPolicy ErrorClass = "policy_violation"
)

// Retryable reports whether the engine may retry the same step automatically.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ClassTransient, ClassAPIRateLimit, ClassSubscriptionLimit, ClassMaxTurns, ClassIncomplete, ClassCrash, ClassTimeout:
		return true
	}
	return false
}

// Error is a failed agent invocation. Message is English, short, and masked.
type Error struct {
	Class      ErrorClass
	Agent      string
	Message    string
	ResetAt    time.Time     // subscription limit, when the CLI said
	RetryAfter time.Duration // rate limit / transient, when the CLI said
	// Invocation holds what is known about the failed call (session ID for
	// resume, usage so far, artifact refs).
	Invocation Invocation
	// Calls are all calls of an operation that made several (reviewer
	// retries), so their usage is not lost when it fails.
	Calls []Invocation
	Err   error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Agent, e.Class)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if !e.ResetAt.IsZero() {
		s += " (resets " + e.ResetAt.UTC().Format(time.RFC3339) + ")"
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf returns the class of an agent error, or "" for other errors.
func ClassOf(err error) ErrorClass {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Class
	}
	return ""
}
