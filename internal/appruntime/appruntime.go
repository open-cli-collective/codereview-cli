// Package appruntime contains command-independent application runtime helpers.
package appruntime

import (
	"context"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/reporoot"
)

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

// ResolveRepoRoot returns the current invocation worktree root or
// reporoot.ErrUnavailable when the command is not running inside a Git
// worktree.
func ResolveRepoRoot(ctx context.Context) (string, error) {
	return reporoot.Resolve(ctx, "", nil)
}
