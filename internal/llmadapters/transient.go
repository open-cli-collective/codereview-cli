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

// missingSessionCLIDetailSubstrings match the provider CLI messages for a
// conversation cr asked to continue that the CLI can no longer find. Claude
// reports one form when resuming (--resume) and another when forking.
var missingSessionCLIDetailSubstrings = []string{
	"no conversation found with session id",
	"no conversation found with session",
	"session not found",
}

// isMissingSessionCLIDetail reports whether detail says the conversation cr
// asked to resume or fork is gone, as opposed to the provider failing. The
// caller recovers by starting a fresh conversation rather than failing the task.
func isMissingSessionCLIDetail(detail string) bool {
	lowered := strings.ToLower(detail)
	for _, needle := range missingSessionCLIDetailSubstrings {
		if strings.Contains(lowered, needle) {
			return true
		}
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
