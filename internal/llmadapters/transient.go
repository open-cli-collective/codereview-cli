package llmadapters

import (
	"fmt"
	"strings"
)

var transientCLIDetailSubstrings = []string{
	"overloaded_error",
	"overloaded",
	"rate limit",
	"rate_limit",
	"429",
	"500",
	"502",
	"503",
	"504",
	"529",
	"timed out",
	"timeout",
	"temporarily unavailable",
	"service unavailable",
	"connection reset",
}

func classifyHTTPStatusTransient(status int) bool {
	switch status {
	case 429, 500, 502, 503, 504, 529:
		return true
	default:
		return false
	}
}

// isMissingSessionCLIDetail reports whether detail says the conversation cr
// asked to resume or fork is gone, as opposed to the provider failing. The
// caller recovers by starting a fresh conversation rather than failing the task.
//
// Only the two message forms Claude actually emits are matched — resume's
// "No conversation found with session ID: <id>" and fork's "source session
// <id> not found". detail is not always a provider control message (the
// foreground path falls back to the whole stdout transcript, and the
// errored-result path carries the model's own text), so broad needles like a
// bare "session not found" could reclassify arbitrary output. New provider
// phrasings get added here when observed, not preempted.
func isMissingSessionCLIDetail(detail string) bool {
	lowered := strings.ToLower(detail)
	if strings.Contains(lowered, "no conversation found with session") {
		return true
	}
	// Fork reports "source session <id> not found", so the id sits between the
	// two halves and a single substring cannot match it.
	return strings.Contains(lowered, "source session") && strings.Contains(lowered, "not found")
}

// classifyCLIDetail wraps runErr with the sentinel that matches detail, so every
// site that surfaces a provider CLI failure classifies it the same way. A
// missing conversation is checked first: it is recoverable by a caller that can
// start fresh, while a transient error is only worth the same call again.
func classifyCLIDetail(runErr error, detail string) error {
	switch {
	case isMissingSessionCLIDetail(detail):
		return fmt.Errorf("%w: %w", ErrMissingProviderSession, runErr)
	case isTransientCLIDetail(detail):
		return fmt.Errorf("%w: %w", ErrTransient, runErr)
	default:
		return runErr
	}
}

func isTransientCLIDetail(detail string) bool {
	lowered := strings.ToLower(detail)
	for _, needle := range transientCLIDetailSubstrings {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}
