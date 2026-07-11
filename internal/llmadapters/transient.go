package llmadapters

import "strings"

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

func isTransientCLIDetail(detail string) bool {
	lowered := strings.ToLower(detail)
	for _, needle := range transientCLIDetailSubstrings {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}
