// Package llmadapters contains the concrete LLM HTTP and subprocess adapters.
package llmadapters

import (
	"context"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

type (
	// Request aliases the neutral invocation contract.
	Request = llm.Request
	// Response aliases the neutral result contract.
	Response = llm.Response
	// Stream aliases the neutral asynchronous result contract.
	Stream = llm.Stream
	// Usage aliases neutral usage accounting.
	Usage = llm.Usage
	// Quota aliases neutral quota state.
	Quota = llm.Quota
	// ReviewerWorkspaceMode aliases the neutral workspace capability.
	ReviewerWorkspaceMode = llm.ReviewerWorkspaceMode
	// ReviewerWorkspaceRequest aliases the neutral workspace request.
	ReviewerWorkspaceRequest = llm.ReviewerWorkspaceRequest
	// ScratchDirFactory aliases the shared scratch-directory factory.
	ScratchDirFactory = llm.ScratchDirFactory
	baseStream        = llm.BaseStream
	launchedProcess   = llm.LaunchedProcess
)

type subprocessResult struct {
	response Response
	err      error
}

const (
	// ReviewerWorkspaceNone means the adapter cannot use a reviewer workspace.
	ReviewerWorkspaceNone = llm.ReviewerWorkspaceNone
	// ReviewerWorkspacePermissionBounded means tool permissions bound workspace access.
	ReviewerWorkspacePermissionBounded = llm.ReviewerWorkspacePermissionBounded
	// ReviewerWorkspaceWrite means the adapter can use a writable workspace.
	ReviewerWorkspaceWrite = llm.ReviewerWorkspaceWrite
)

var (
	// ErrTransient marks a retryable provider failure.
	ErrTransient = llm.ErrTransient
)

// RequireReviewerWorkspace validates the neutral workspace capability.
func RequireReviewerWorkspace(adapter llm.Adapter) error {
	return llm.RequireReviewerWorkspace(adapter)
}

// AdapterReviewerWorkspaceMode returns an adapter's workspace capability.
func AdapterReviewerWorkspaceMode(adapter llm.Adapter) ReviewerWorkspaceMode {
	return llm.AdapterReviewerWorkspaceMode(adapter)
}

// SupportsReviewerWorkspace reports whether an adapter accepts a workspace.
func SupportsReviewerWorkspace(adapter llm.Adapter) bool {
	return llm.SupportsReviewerWorkspace(adapter)
}

func launchProcess(ctx context.Context, command string, args []string, dir string, env []string, timeout time.Duration, logPath string, cleanup func() error, withStdin bool) (*launchedProcess, error) {
	return llm.LaunchProcess(ctx, command, args, dir, env, timeout, logPath, cleanup, withStdin)
}
