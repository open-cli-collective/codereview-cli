package app

import (
	"strconv"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitexec"
	githubprovider "github.com/open-cli-collective/codereview-cli/internal/gitprovider/github"
	"github.com/open-cli-collective/codereview-cli/internal/outbox"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

func endProgressSpan(span *progress.Span, err error) error {
	if span != nil {
		_ = span.End(err)
	}
	return err
}

// githubRetryProgress reports each retried GitHub read, so a slow run shows why.
func githubRetryProgress(logger *progress.Logger, command string) func(githubprovider.RetryEvent) {
	return func(event githubprovider.RetryEvent) {
		logger.InfoReason(command, "retry_read", string(event.Op), event.Reason,
			progress.Field{Key: "attempt", Value: strconv.Itoa(event.Attempt)},
			progress.Field{Key: "max_attempts", Value: strconv.Itoa(event.MaxAttempts)},
			progress.Field{Key: "delay_ms", Value: strconv.FormatInt(event.Delay.Milliseconds(), 10)},
		)
	}
}

// gitFetchRetryProgress reports each retried git fetch.
func gitFetchRetryProgress(logger *progress.Logger, command string) func(gitexec.RetryEvent) {
	return func(event gitexec.RetryEvent) {
		logger.InfoReason(command, "retry_fetch", "workbench", event.Reason,
			progress.Field{Key: "attempt", Value: strconv.Itoa(event.Attempt)},
			progress.Field{Key: "max_attempts", Value: strconv.Itoa(event.MaxAttempts)},
			progress.Field{Key: "delay_ms", Value: strconv.FormatInt(event.Delay.Milliseconds(), 10)},
		)
	}
}

// writeRetryProgress reports each step of recovering a failed GitHub write.
func writeRetryProgress(logger *progress.Logger, command string) func(outbox.WriteRetryEvent) {
	return func(event outbox.WriteRetryEvent) {
		fields := []progress.Field{
			{Key: "phase", Value: string(event.Phase)},
			{Key: "action", Value: event.ActionID},
			{Key: "attempt", Value: strconv.Itoa(event.Attempt)},
			{Key: "max_attempts", Value: strconv.Itoa(event.MaxAttempts)},
		}
		if event.Delay > time.Duration(0) {
			fields = append(fields, progress.Field{Key: "delay_ms", Value: strconv.FormatInt(event.Delay.Milliseconds(), 10)})
		}
		logger.InfoReason(command, "retry_post", string(event.Kind), event.Reason, fields...)
	}
}
