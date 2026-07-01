// Package reporoot resolves the current Git worktree root for review safety checks.
package reporoot

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrUnavailable reports that the current process is not running inside a Git
// worktree, so no invocation root is available.
var ErrUnavailable = errors.New("reporoot: unavailable")

// GitCommand resolves a git command in an optional working directory.
type GitCommand func(context.Context, string, ...string) ([]byte, error)

// Resolve returns the canonical Git worktree root for dir or the current
// process directory when dir is empty.
func Resolve(ctx context.Context, dir string, run GitCommand) (string, error) {
	if run == nil {
		run = defaultGitCommand
	}
	out, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if isNotInWorktree(err) {
			return "", ErrUnavailable
		}
		return "", err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("git rev-parse --show-toplevel returned empty output")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("canonicalize repo root %s: %w", root, err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("canonicalize repo root %s: %w", root, err)
	}
	return canonical, nil
}

func defaultGitCommand(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed git command with structured arguments.
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return output, nil
}

func isNotInWorktree(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not a git repository")
}
