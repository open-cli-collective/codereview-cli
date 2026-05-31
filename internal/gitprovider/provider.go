package gitprovider

import "context"

// GitProvider is the host-agnostic seam for pull-request reads and writes.
type GitProvider interface {
	WhoAmI(ctx context.Context, creds Credential) (Identity, error)

	GetPR(ctx context.Context, ref PRRef) (PR, error)
	GetDiff(ctx context.Context, ref PRRef) (UnifiedDiff, error)
	GetFileAtRef(ctx context.Context, ref PRRef, path string, gitRef string) ([]byte, error)
	ListTreeAtRef(ctx context.Context, ref PRRef, gitRef string, path string) ([]TreeEntry, error)
	ListInlineThreads(ctx context.Context, ref PRRef) ([]InlineThread, error)
	ListReviews(ctx context.Context, ref PRRef) ([]Review, error)
	ListIssueComments(ctx context.Context, ref PRRef) ([]IssueComment, error)

	PostInlineComment(ctx context.Context, ref PRRef, c InlineComment) (CommentID, error)
	ReplyToThread(ctx context.Context, ref PRRef, threadID ThreadID, body string) (CommentID, error)
	ResolveThread(ctx context.Context, ref PRRef, threadID ThreadID) error
	PostIssueComment(ctx context.Context, ref PRRef, body string) (CommentID, error)
	SubmitReview(ctx context.Context, ref PRRef, r ReviewRequest) (ReviewID, error)

	Capabilities() ProviderCaps
}
