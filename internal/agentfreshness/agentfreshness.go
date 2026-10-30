// Package agentfreshness reports profile agent sources whose local checkout may
// not match the shared catalog it tracks.
//
// Profile agent sources are local directories, so a reviewer merged upstream
// does not run until someone updates the checkout. This package makes that
// visible before a review: it fetches each source's upstream branch and reports
// checkouts that are behind, plus sources cr cannot check at all. It never
// fails a review.
package agentfreshness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
)

// fetchTimeout bounds the upstream fetch so an unreachable remote delays a
// review by seconds, not minutes.
const fetchTimeout = 15 * time.Second

// GitCommand runs git in dir and returns its standard output.
type GitCommand func(ctx context.Context, dir string, args ...string) ([]byte, error)

// Kind classifies a notice.
type Kind string

const (
	// KindUnversioned reports a source outside any Git worktree.
	KindUnversioned Kind = "unversioned"
	// KindNoUpstream reports a source whose checked-out branch tracks no upstream.
	KindNoUpstream Kind = "no_upstream"
	// KindBehind reports a checkout missing commits from its upstream.
	KindBehind Kind = "behind"
	// KindFetchFailed reports a checkout that matched its last fetched upstream
	// but whose upstream could not be fetched to confirm it is current.
	KindFetchFailed Kind = "fetch_failed"
)

// Notice describes one agent source that may not reflect its shared catalog.
type Notice struct {
	Kind     Kind
	Source   string
	RepoRoot string
	Upstream string
	Behind   int
	// FetchError is set when the upstream could not be fetched, so Behind was
	// counted against the last fetched upstream state.
	FetchError string
}

// Warn checks dirs with the host git and writes any notices to w. A nil w
// skips the check entirely.
func Warn(ctx context.Context, dirs []string, w io.Writer) {
	if w == nil || len(dirs) == 0 {
		return
	}
	Write(w, Check(ctx, dirs, defaultGitCommand))
}

// Check reports sources that are unversioned, track no upstream, or are behind
// their upstream. Missing or unreadable sources are left to the agent loader,
// and sources sharing a checkout are checked once.
func Check(ctx context.Context, dirs []string, run GitCommand) []Notice {
	var notices []Notice
	seenSources := map[string]bool{}
	seenRepos := map[string]bool{}
	for _, source := range agents.InspectProfileSources(dirs) {
		if ctx.Err() != nil {
			return notices
		}
		if source.CanonicalPath == "" || seenSources[source.CanonicalPath] {
			continue
		}
		seenSources[source.CanonicalPath] = true
		out, err := run(ctx, source.CanonicalPath, "rev-parse", "--show-toplevel")
		if err != nil {
			notices = append(notices, Notice{Kind: KindUnversioned, Source: source.ConfiguredPath})
			continue
		}
		root := strings.TrimSpace(string(out))
		if seenRepos[root] {
			continue
		}
		seenRepos[root] = true
		if notice, ok := checkRepo(ctx, run, source.ConfiguredPath, root); ok {
			notices = append(notices, notice)
		}
	}
	return notices
}

func checkRepo(ctx context.Context, run GitCommand, source, root string) (Notice, bool) {
	out, err := run(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		return Notice{Kind: KindNoUpstream, Source: source, RepoRoot: root}, true
	}
	upstream := strings.TrimSpace(string(out))
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	_, fetchErr := run(fetchCtx, root, "fetch", "--quiet", "--no-tags")
	cancel()
	out, err = run(ctx, root, "rev-list", "--count", "HEAD..@{upstream}")
	if err != nil {
		return Notice{}, false
	}
	behind, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return Notice{}, false
	}
	notice := Notice{Kind: KindBehind, Source: source, RepoRoot: root, Upstream: upstream, Behind: behind}
	if fetchErr != nil {
		notice.FetchError = fetchErr.Error()
		if behind == 0 {
			notice.Kind = KindFetchFailed
		}
	}
	return notice, notice.Kind == KindFetchFailed || behind > 0
}

// Write renders notices as one line each.
func Write(w io.Writer, notices []Notice) {
	for _, notice := range notices {
		_, _ = fmt.Fprintln(w, notice.String())
	}
}

func (n Notice) String() string {
	switch n.Kind {
	case KindUnversioned:
		return fmt.Sprintf("note: agent source %s is not in a Git repository; cr cannot check whether its reviewers are current", n.Source)
	case KindNoUpstream:
		return fmt.Sprintf("note: agent source %s is in %s, whose branch tracks no upstream; cr cannot check whether its reviewers are current", n.Source, n.RepoRoot)
	case KindBehind:
		line := fmt.Sprintf("warning: agent source %s is %d %s behind %s; reviewer changes merged there will not run. Update with: git -C %s merge --ff-only %s",
			n.Source, n.Behind, plural(n.Behind, "commit", "commits"), n.Upstream, shellQuote(n.RepoRoot), shellQuote(n.Upstream))
		if n.FetchError != "" {
			line += fmt.Sprintf(" (fetch failed, counted from the last fetch: %s)", firstLine(n.FetchError))
		}
		return line
	case KindFetchFailed:
		return fmt.Sprintf("note: agent source %s matches the last fetch of %s, but cr could not fetch it to confirm it is current: %s", n.Source, n.Upstream, firstLine(n.FetchError))
	default:
		return fmt.Sprintf("note: agent source %s: %s", n.Source, n.Kind)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// shellQuote returns s as one POSIX shell word, so the suggested update
// command can be pasted as-is.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+:@%=,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func defaultGitCommand(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed git command with structured arguments.
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANG=C", "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if message := strings.TrimSpace(string(exitErr.Stderr)); message != "" {
				return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
			}
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}
