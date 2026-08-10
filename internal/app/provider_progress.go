package app

import (
	"context"
	pathpkg "path"
	"strings"
	"unicode"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

type progressProvider struct {
	provider gitprovider.GitProvider
	logger   *progress.Logger
	hooks    *hookDispatcher
	command  string
}

type progressRangeProvider struct {
	progressProvider
}

func withProgressProvider(logger *progress.Logger, dispatcher *hookDispatcher, command string, provider gitprovider.GitProvider) gitprovider.GitProvider {
	if provider == nil {
		return nil
	}
	if dispatcher != nil && !dispatcher.enabled {
		dispatcher = nil
	}
	if logger == nil && dispatcher == nil {
		return provider
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = "review"
	}
	wrapped := progressProvider{provider: provider, logger: logger, hooks: dispatcher, command: command}
	if _, ok := provider.(interface {
		GetDiffBetweenRefs(context.Context, gitprovider.PRRef, string, string) (gitprovider.UnifiedDiff, error)
	}); ok {
		return progressRangeProvider{progressProvider: wrapped}
	}
	return wrapped
}

// start opens a progress span, or none when the wrapper exists only to observe
// provider calls for hooks.
func (p progressProvider) start(op, target string) *progress.Span {
	if p.logger == nil {
		return nil
	}
	return p.logger.Start(p.command, op, target)
}

func (p progressProvider) WhoAmI(ctx context.Context, creds gitprovider.Credential) (gitprovider.Identity, error) {
	span := p.start("resolve_identity", "runtime")
	identity, err := p.provider.WhoAmI(ctx, creds)
	return identity, endProgressSpan(span, err)
}

func (p progressProvider) ReviewAuthority(ctx context.Context, ref gitprovider.PRRef, identity gitprovider.Identity) (gitprovider.ReviewAuthority, error) {
	span := p.start("check_review_authority", "runtime")
	authority, err := p.provider.ReviewAuthority(ctx, ref, identity)
	return authority, endProgressSpan(span, err)
}

func (p progressProvider) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	span := p.start("fetch_pr", "pr")
	pr, err := p.provider.GetPR(ctx, ref)
	if err == nil && p.hooks != nil {
		p.hooks.observeAuthor(pr.Author.Login)
	}
	return pr, endProgressSpan(span, err)
}

func (p progressProvider) GetDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	span := p.start("fetch_diff", "pr")
	diff, err := p.provider.GetDiff(ctx, ref)
	return diff, endProgressSpan(span, err)
}

func (p progressRangeProvider) GetDiffBetweenRefs(ctx context.Context, ref gitprovider.PRRef, baseSHA, headSHA string) (gitprovider.UnifiedDiff, error) {
	rangeProvider := p.provider.(interface {
		GetDiffBetweenRefs(context.Context, gitprovider.PRRef, string, string) (gitprovider.UnifiedDiff, error)
	})
	span := p.start("fetch_diff_between_refs", "pr")
	diff, err := rangeProvider.GetDiffBetweenRefs(ctx, ref, baseSHA, headSHA)
	return diff, endProgressSpan(span, err)
}

func (p progressProvider) GetFileAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef, path string) ([]byte, error) {
	span := p.start("read_file", fileTarget(path))
	data, err := p.provider.GetFileAtRef(ctx, ref, gitRef, path)
	return data, endProgressSpan(span, err)
}

func (p progressProvider) ListTreeAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef, path string) ([]gitprovider.TreeEntry, error) {
	span := p.start("list_tree", fileTarget(path))
	entries, err := p.provider.ListTreeAtRef(ctx, ref, gitRef, path)
	return entries, endProgressSpan(span, err)
}

func (p progressProvider) ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	span := p.start("list_threads", "threads")
	threads, err := p.provider.ListInlineThreads(ctx, ref)
	return threads, endProgressSpan(span, err)
}

func (p progressProvider) ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error) {
	span := p.start("list_reviews", "reviews")
	reviews, err := p.provider.ListReviews(ctx, ref)
	return reviews, endProgressSpan(span, err)
}

func (p progressProvider) ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	span := p.start("list_issue_comments", "posts")
	comments, err := p.provider.ListIssueComments(ctx, ref)
	return comments, endProgressSpan(span, err)
}

func (p progressProvider) PostInlineComment(ctx context.Context, ref gitprovider.PRRef, c gitprovider.InlineComment) (gitprovider.CommentID, error) {
	span := p.start("post_inline_comment", "posts")
	id, err := p.provider.PostInlineComment(ctx, ref, c)
	return id, endProgressSpan(span, err)
}

func (p progressProvider) ReplyToThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	span := p.start("reply_thread", "posts")
	id, err := p.provider.ReplyToThread(ctx, ref, threadID, body)
	return id, endProgressSpan(span, err)
}

func (p progressProvider) ResolveThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID) error {
	span := p.start("resolve_thread", "posts")
	return endProgressSpan(span, p.provider.ResolveThread(ctx, ref, threadID))
}

func (p progressProvider) PostIssueComment(ctx context.Context, ref gitprovider.PRRef, body string) (gitprovider.CommentID, error) {
	span := p.start("post_issue_comment", "posts")
	id, err := p.provider.PostIssueComment(ctx, ref, body)
	return id, endProgressSpan(span, err)
}

func (p progressProvider) SubmitReview(ctx context.Context, ref gitprovider.PRRef, r gitprovider.ReviewRequest) (gitprovider.ReviewID, error) {
	span := p.start("submit_review", "posts")
	id, err := p.provider.SubmitReview(ctx, ref, r)
	return id, endProgressSpan(span, err)
}

func (p progressProvider) Capabilities() gitprovider.ProviderCaps {
	return p.provider.Capabilities()
}

func fileTarget(pathValue string) string {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return "file"
	}
	sanitized := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, pathValue)
	return "file:" + pathpkg.Clean(sanitized)
}
