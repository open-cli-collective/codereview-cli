package reviewruntime

import "github.com/open-cli-collective/codereview-cli/internal/progress"

func endProgressSpan(span *progress.Span, err error) error {
	if span != nil {
		_ = span.End(err)
	}
	return err
}
