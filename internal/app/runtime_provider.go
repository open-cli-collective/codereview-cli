package app

import (
	"context"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

// runtimeProvider keeps repository reads on the profile git credential while
// reviewer-authenticated writes and authority checks flow through the posting
// credential.
type runtimeProvider struct {
	read  gitprovider.GitProvider
	write gitprovider.GitProvider
}

func (p runtimeProvider) WhoAmI(ctx context.Context, creds gitprovider.Credential) (gitprovider.Identity, error) {
	return p.write.WhoAmI(ctx, creds)
}

func (p runtimeProvider) ReviewAuthority(ctx context.Context, ref gitprovider.PRRef, identity gitprovider.Identity) (gitprovider.ReviewAuthority, error) {
	return p.write.ReviewAuthority(ctx, ref, identity)
}

func (p runtimeProvider) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	return p.read.GetPR(ctx, ref)
}

func (p runtimeProvider) GetDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	return p.read.GetDiff(ctx, ref)
}

func (p runtimeProvider) GetFileAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, path string) ([]byte, error) {
	return p.read.GetFileAtRef(ctx, ref, gitRef, path)
}

func (p runtimeProvider) ListTreeAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef string, path string) ([]gitprovider.TreeEntry, error) {
	return p.read.ListTreeAtRef(ctx, ref, gitRef, path)
}

func (p runtimeProvider) ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	return p.read.ListInlineThreads(ctx, ref)
}

func (p runtimeProvider) ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error) {
	return p.read.ListReviews(ctx, ref)
}

func (p runtimeProvider) ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	return p.read.ListIssueComments(ctx, ref)
}

func (p runtimeProvider) PostInlineComment(ctx context.Context, ref gitprovider.PRRef, c gitprovider.InlineComment) (gitprovider.CommentID, error) {
	return p.write.PostInlineComment(ctx, ref, c)
}

func (p runtimeProvider) ReplyToThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	return p.write.ReplyToThread(ctx, ref, threadID, body)
}

func (p runtimeProvider) ResolveThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID) error {
	return p.write.ResolveThread(ctx, ref, threadID)
}

func (p runtimeProvider) PostIssueComment(ctx context.Context, ref gitprovider.PRRef, body string) (gitprovider.CommentID, error) {
	return p.write.PostIssueComment(ctx, ref, body)
}

func (p runtimeProvider) SubmitReview(ctx context.Context, ref gitprovider.PRRef, r gitprovider.ReviewRequest) (gitprovider.ReviewID, error) {
	return p.write.SubmitReview(ctx, ref, r)
}

func (p runtimeProvider) Capabilities() gitprovider.ProviderCaps {
	return p.write.Capabilities()
}
