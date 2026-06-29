// Package cmdruntime contains shared command runtime contracts.
package cmdruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-cli-collective/cli-common/credstore"
	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmderr"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
	"github.com/open-cli-collective/codereview-cli/internal/threadrespond"
)

// Runner executes the configured review pipeline.
type Runner interface {
	DryRun(context.Context, pipeline.Request) (pipeline.Result, error)
	Live(context.Context, pipeline.Request, reviewrun.Flags) (reviewrun.Result, error)
}

// ResponseRunner executes response-only thread lifecycle runs.
type ResponseRunner interface {
	Respond(context.Context, threadrespond.Request) (threadrespond.Result, error)
}

// Runtime contains per-command dependencies that need cleanup after a run.
type Runtime struct {
	Runner          Runner
	Responder       ResponseRunner
	PostingIdentity gitprovider.Identity
	Cleanup         func()
}

// Options carries command flags that affect runtime construction.
type Options struct {
	MaxAgents                         int
	MaxConcurrency                    int
	PRRef                             gitprovider.PRRef
	RequireOpinionatedReviewAuthority bool
	Retention                         datalifecycle.RetentionPolicy
	RetentionManualOnly               bool
	ResolveRepoRoot                   func(context.Context) (string, error)
	GitCommand                        func(context.Context, string, ...string) ([]byte, error)
}

// Factory builds the concrete runtime used by review lifecycle commands.
type Factory func(cmd *cobra.Command, opts *root.Options, cfg config.File, profile config.Profile, runtimeOpts Options) (Runtime, error)

// ConfigPath resolves the active config path from root options.
func ConfigPath(opts *root.Options) (string, error) {
	if opts != nil && opts.ConfigPath != "" {
		return opts.ConfigPath, nil
	}
	return config.Path()
}

// MapRunError maps lower-level runtime errors to CLI exit-code wrappers.
func MapRunError(err error) error {
	switch {
	case errors.Is(err, config.ErrInvalid),
		errors.Is(err, config.ErrNotConfigured),
		errors.Is(err, config.ErrProfileNotFound),
		errors.Is(err, config.ErrSecretsProfileNotFound),
		errors.Is(err, config.ErrUnsupported):
		return cmderr.Config(err)
	case errors.Is(err, agents.ErrUnsafeSource):
		return exitcode.Usage(err)
	case errors.Is(err, gitprovider.ErrAuth),
		errors.Is(err, gitprovider.ErrPermission),
		errors.Is(err, gitprovider.ErrIneligibleReviewAuthority),
		errors.Is(err, gitprovider.ErrRetryable),
		errors.Is(err, gitprovider.ErrNotFound),
		errors.Is(err, gitprovider.ErrConflict),
		errors.Is(err, gitprovider.ErrStaleSHA),
		errors.Is(err, gitprovider.ErrDiffTooLarge):
		return cmderr.Provider(err)
	case errors.Is(err, credentials.ErrInvalidBackendSelection),
		errors.Is(err, credentials.ErrWrongService),
		errors.Is(err, credstore.ErrRefEmpty),
		errors.Is(err, credstore.ErrRefSegmentCount),
		errors.Is(err, credstore.ErrRefInvalidChar),
		errors.Is(err, credstore.ErrKeyNotAllowed),
		errors.Is(err, credstore.ErrExists),
		errors.Is(err, credstore.ErrFilePassphraseRequired),
		errors.Is(err, credstore.ErrSecretServiceFailClosed),
		errors.Is(err, credstore.ErrStoreClosed),
		errors.Is(err, credstore.ErrBackendNotImplemented):
		return cmderr.Credential(err)
	default:
		return err
	}
}

// RetentionPolicyFromConfig maps config retention settings to runtime policy.
func RetentionPolicyFromConfig(retention config.RetentionConfig) datalifecycle.RetentionPolicy {
	if retention.MaxAgeDays == nil {
		return datalifecycle.RetentionPolicy{}
	}
	maxAgeDays := *retention.MaxAgeDays
	if maxAgeDays == 0 {
		return datalifecycle.RetentionPolicy{LiveForever: true}
	}
	return datalifecycle.RetentionPolicy{LiveMaxAge: time.Duration(maxAgeDays) * 24 * time.Hour}
}

// MissingResponderError reports that a command runtime cannot execute response runs.
func MissingResponderError() error {
	return fmt.Errorf("respond: runtime responder is required")
}
