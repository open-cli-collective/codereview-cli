package benchmarkcmd

import (
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

func newProgressLogger(opts *root.Options) *progress.Logger {
	if opts == nil {
		return progress.New(nil, true, nil)
	}
	return progress.New(opts.Stderr, opts.Quiet, nil)
}

func endProgressSpan(span *progress.Span, err error) error {
	if span != nil {
		span.End(err)
	}
	return err
}
