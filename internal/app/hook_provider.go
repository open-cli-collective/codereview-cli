package app

import (
	"context"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/hooks"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
)

type hookProvider struct {
	provider gitprovider.GitProvider
	hooks    *hookDispatcher
}

func withHookProvider(dispatcher *hookDispatcher, provider gitprovider.GitProvider) gitprovider.GitProvider {
	if provider == nil || dispatcher == nil || !dispatcher.enabled {
		return provider
	}
	return hookProvider{provider: provider, hooks: dispatcher}
}

func (p hookProvider) WhoAmI(ctx context.Context, creds gitprovider.Credential) (gitprovider.Identity, error) {
	return p.provider.WhoAmI(ctx, creds)
}

func (p hookProvider) ReviewAuthority(ctx context.Context, ref gitprovider.PRRef, identity gitprovider.Identity) (gitprovider.ReviewAuthority, error) {
	return p.provider.ReviewAuthority(ctx, ref, identity)
}

func (p hookProvider) GetPR(ctx context.Context, ref gitprovider.PRRef) (gitprovider.PR, error) {
	return p.provider.GetPR(ctx, ref)
}

func (p hookProvider) GetDiff(ctx context.Context, ref gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	return p.provider.GetDiff(ctx, ref)
}

func (p hookProvider) GetFileAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef, path string) ([]byte, error) {
	return p.provider.GetFileAtRef(ctx, ref, gitRef, path)
}

func (p hookProvider) ListTreeAtRef(ctx context.Context, ref gitprovider.PRRef, gitRef, path string) ([]gitprovider.TreeEntry, error) {
	return p.provider.ListTreeAtRef(ctx, ref, gitRef, path)
}

func (p hookProvider) ListInlineThreads(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	return p.provider.ListInlineThreads(ctx, ref)
}

func (p hookProvider) ListReviews(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.Review, error) {
	return p.provider.ListReviews(ctx, ref)
}

func (p hookProvider) ListIssueComments(ctx context.Context, ref gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	return p.provider.ListIssueComments(ctx, ref)
}

func (p hookProvider) PostInlineComment(ctx context.Context, ref gitprovider.PRRef, comment gitprovider.InlineComment) (gitprovider.CommentID, error) {
	p.preparePost(comment.Body)
	id, err := p.provider.PostInlineComment(ctx, ref, comment)
	p.posted(marker.ActionKindInlineComment, comment.Body, err)
	return id, err
}

func (p hookProvider) ReplyToThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	p.preparePost(body)
	id, err := p.provider.ReplyToThread(ctx, ref, threadID, body)
	p.posted(marker.ActionKindThreadReply, body, err)
	return id, err
}

func (p hookProvider) ResolveThread(ctx context.Context, ref gitprovider.PRRef, threadID gitprovider.ThreadID) error {
	p.preparePost("")
	err := p.provider.ResolveThread(ctx, ref, threadID)
	p.posted("resolve_thread", "", err)
	return err
}

func (p hookProvider) PostIssueComment(ctx context.Context, ref gitprovider.PRRef, body string) (gitprovider.CommentID, error) {
	p.preparePost(body)
	id, err := p.provider.PostIssueComment(ctx, ref, body)
	p.posted(marker.ActionKindRollupComment, body, err)
	return id, err
}

func (p hookProvider) SubmitReview(ctx context.Context, ref gitprovider.PRRef, review gitprovider.ReviewRequest) (gitprovider.ReviewID, error) {
	p.preparePost(review.Body)
	id, err := p.provider.SubmitReview(ctx, ref, review)
	p.posted(marker.ActionKindSubmitReview, review.Body, err)
	return id, err
}

func (p hookProvider) Capabilities() gitprovider.ProviderCaps { return p.provider.Capabilities() }

func (p hookProvider) posted(kind, body string, err error) {
	if err != nil {
		return
	}
	runID, rendered := actionMarker(body)
	run := p.hooks.observeRunID(runID)
	p.hooks.emit(p.hooks.event("posting.action"), hooks.Payload{ActionKind: kind, ActionMarker: rendered}, run, false)
}

func (p hookProvider) preparePost(body string) {
	if p.hooks.command != "respond" {
		return
	}
	runID, _ := actionMarker(body)
	p.hooks.emitOnce(p.hooks.event("plan.ready"), hooks.Payload{}, p.hooks.observeRunID(runID))
}

func actionMarker(body string) (string, string) {
	if actions := marker.FindActions(body); len(actions) > 0 {
		rendered, _ := marker.RenderAction(actions[0])
		return actions[0].RunID, rendered
	}
	if summaries := marker.FindThreadSummaries(body); len(summaries) > 0 {
		rendered, _ := marker.RenderThreadSummary(summaries[0])
		return summaries[0].RunID, rendered
	}
	return "", ""
}
