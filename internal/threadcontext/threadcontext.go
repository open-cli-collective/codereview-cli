// Package threadcontext normalizes inline review threads into prompt-safe
// domain context.
package threadcontext

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// Options controls thread normalization.
type Options struct {
	PostingIdentity gitprovider.Identity
}

// Thread is one normalized inline review thread.
type Thread struct {
	ID               gitprovider.ThreadID
	Resolved         bool
	Anchor           Anchor
	Comments         []Comment
	Status           Status
	ResolvedSummary  *ThreadSummary
	CRSettledSummary *ThreadSummary
}

// Anchor identifies the review-thread location.
type Anchor struct {
	Path        string
	Side        review.DiffSide
	Line        int
	SubjectType review.AnchorKind
	CommitSHA   string
}

// Comment is one prompt-safe normalized thread comment.
type Comment struct {
	ID                        gitprovider.CommentID
	ThreadID                  gitprovider.ThreadID
	Body                      string
	Author                    gitprovider.Identity
	Anchor                    Anchor
	URL                       string
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	AuthoredByPostingIdentity bool
	HasFindingMarker          bool
	HasThreadReplyMarker      bool
	HasThreadSummaryMarker    bool
	providerOrder             int
}

// Status summarizes lifecycle-relevant thread state.
type Status struct {
	CRAuthoredFinding       bool
	HasCRSummary            bool
	LatestCRComment         *Comment
	LatestHumanReplyAfterCR *Comment
	PendingHumanReply       bool
}

// ThreadSummary is compact reviewer context for a provider-resolved or
// CR-settled inline thread.
type ThreadSummary struct {
	ThreadID                             gitprovider.ThreadID
	Anchor                               Anchor
	URL                                  string
	Body                                 string
	LastCommentID                        gitprovider.CommentID
	LastCommentAuthor                    gitprovider.Identity
	LastCommentAuthoredByPostingIdentity bool
	LastCommentHasThreadSummaryMarker    bool
}

// Normalize converts provider inline threads into deterministic prompt-safe
// domain threads.
func Normalize(threads []gitprovider.InlineThread, opts Options) ([]Thread, error) {
	if strings.TrimSpace(opts.PostingIdentity.ID) == "" && strings.TrimSpace(opts.PostingIdentity.Login) == "" {
		return nil, fmt.Errorf("threadcontext: posting identity is required")
	}
	out := make([]Thread, 0, len(threads))
	for _, thread := range threads {
		normalized := Thread{
			ID:       thread.ID,
			Resolved: thread.Resolved,
			Anchor: Anchor{
				Path:        thread.Path,
				Side:        thread.Side,
				Line:        thread.Line,
				SubjectType: thread.SubjectType,
				CommitSHA:   thread.CommitSHA,
			},
		}
		normalized.Comments = normalizeComments(thread, opts.PostingIdentity, normalized.Anchor)
		normalized.Status = statusForComments(normalized.Comments)
		if normalized.Resolved && len(normalized.Comments) > 0 {
			last := normalized.Comments[len(normalized.Comments)-1]
			normalized.ResolvedSummary = threadSummaryFromComment(normalized.ID, normalized.Anchor, last)
		}
		if !normalized.Resolved && len(normalized.Comments) > 0 {
			last := normalized.Comments[len(normalized.Comments)-1]
			if last.AuthoredByPostingIdentity && last.HasThreadSummaryMarker && strings.TrimSpace(last.Body) != "" {
				normalized.CRSettledSummary = threadSummaryFromComment(normalized.ID, normalized.Anchor, last)
			}
		}
		out = append(out, normalized)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return threadLess(out[i], out[j])
	})
	return out, nil
}

// EffectiveSettledSummary returns the compact settled context that reviewer
// prompts should use, preserving provider resolution as the preferred source.
func (thread Thread) EffectiveSettledSummary() (*ThreadSummary, bool) {
	if thread.ResolvedSummary != nil {
		return thread.ResolvedSummary, true
	}
	if thread.CRSettledSummary != nil {
		return thread.CRSettledSummary, true
	}
	return nil, false
}

// PendingCRAuthoredFindingThreads filters to unresolved CR-authored finding
// threads that have a human reply after the latest CR-authored comment.
func PendingCRAuthoredFindingThreads(threads []Thread) []Thread {
	out := make([]Thread, 0, len(threads))
	for _, thread := range threads {
		if thread.Resolved {
			continue
		}
		if !thread.Status.CRAuthoredFinding || !thread.Status.PendingHumanReply {
			continue
		}
		out = append(out, thread)
	}
	return out
}

// SanitizeBody removes closed codereview marker comments and escapes any
// remaining marker opening so prompt-facing text cannot carry live markers.
func SanitizeBody(body string) string {
	const startNeedle = "<!-- codereview:"
	const endNeedle = " -->"

	var b strings.Builder
	searchFrom := 0
	for searchFrom < len(body) {
		start := strings.Index(body[searchFrom:], startNeedle)
		if start == -1 {
			b.WriteString(body[searchFrom:])
			break
		}
		start += searchFrom
		b.WriteString(body[searchFrom:start])
		payloadStart := start + len(startNeedle)
		end := strings.Index(body[payloadStart:], endNeedle)
		nextStart := strings.Index(body[payloadStart:], startNeedle)
		if nextStart != -1 && (end == -1 || nextStart < end) {
			searchFrom = payloadStart + nextStart
			continue
		}
		if end == -1 {
			b.WriteString("&lt;!-- codereview:")
			searchFrom = payloadStart
			continue
		}
		searchFrom = payloadStart + end + len(endNeedle)
	}
	return strings.TrimSpace(marker.SanitizeModelContent(b.String()))
}

func normalizeComments(thread gitprovider.InlineThread, posting gitprovider.Identity, threadAnchor Anchor) []Comment {
	comments := make([]Comment, 0, len(thread.Comments))
	for i, raw := range thread.Comments {
		authoredByPosting := raw.Author.Same(posting) || githubBotLoginEquivalent(raw.Author.Login, posting.Login)
		hasFindingMarker, hasThreadReplyMarker := false, false
		if authoredByPosting {
			for _, found := range marker.FindActions(raw.Body) {
				switch found.Kind {
				case marker.ActionKindInlineComment:
					hasFindingMarker = true
				case marker.ActionKindThreadReply:
					hasThreadReplyMarker = true
				}
			}
		}
		hasThreadSummaryMarker := authoredByPosting && len(marker.FindThreadSummaries(raw.Body)) > 0
		commentAnchor := Anchor{
			Path:        raw.Path,
			Side:        raw.Side,
			Line:        raw.Line,
			SubjectType: raw.SubjectType,
			CommitSHA:   raw.CommitSHA,
		}
		comments = append(comments, Comment{
			ID:                        raw.ID,
			ThreadID:                  commentThreadID(raw.ThreadID, thread.ID),
			Body:                      SanitizeBody(raw.Body),
			Author:                    raw.Author,
			Anchor:                    firstNonZeroAnchor(commentAnchor, threadAnchor),
			URL:                       raw.URL,
			CreatedAt:                 raw.CreatedAt,
			UpdatedAt:                 raw.UpdatedAt,
			AuthoredByPostingIdentity: authoredByPosting,
			HasFindingMarker:          hasFindingMarker,
			HasThreadReplyMarker:      hasThreadReplyMarker,
			HasThreadSummaryMarker:    hasThreadSummaryMarker,
			providerOrder:             i,
		})
	}
	sort.SliceStable(comments, func(i, j int) bool {
		return commentLess(comments[i], comments[j])
	})
	return comments
}

func statusForComments(comments []Comment) Status {
	status := Status{}
	latestCRIndex := -1
	for i := range comments {
		comment := &comments[i]
		if comment.AuthoredByPostingIdentity && (comment.HasFindingMarker || comment.HasThreadReplyMarker) {
			status.CRAuthoredFinding = true
		}
		if comment.AuthoredByPostingIdentity && comment.HasThreadSummaryMarker {
			status.HasCRSummary = true
		}
		if comment.AuthoredByPostingIdentity && (comment.HasFindingMarker || comment.HasThreadReplyMarker || comment.HasThreadSummaryMarker) {
			status.LatestCRComment = comment
			latestCRIndex = i
		}
	}
	if latestCRIndex == -1 {
		return status
	}
	for i := latestCRIndex + 1; i < len(comments); i++ {
		comment := &comments[i]
		if !comment.AuthoredByPostingIdentity {
			status.LatestHumanReplyAfterCR = comment
			status.PendingHumanReply = true
		}
	}
	return status
}

func firstNonZeroAnchor(primary, fallback Anchor) Anchor {
	if !anchorPresent(primary) {
		return fallback
	}
	if strings.TrimSpace(primary.Path) == "" {
		primary.Path = fallback.Path
	}
	if primary.SubjectType == review.AnchorKindFile {
		if strings.TrimSpace(primary.CommitSHA) == "" {
			primary.CommitSHA = fallback.CommitSHA
		}
		return primary
	}
	if primary.Side == "" {
		primary.Side = fallback.Side
	}
	if primary.Line == 0 {
		primary.Line = fallback.Line
	}
	if primary.SubjectType == "" {
		primary.SubjectType = fallback.SubjectType
	}
	if strings.TrimSpace(primary.CommitSHA) == "" {
		primary.CommitSHA = fallback.CommitSHA
	}
	return primary
}

func threadSummaryFromComment(threadID gitprovider.ThreadID, threadAnchor Anchor, comment Comment) *ThreadSummary {
	return &ThreadSummary{
		ThreadID:                             threadID,
		Anchor:                               firstNonZeroAnchor(comment.Anchor, threadAnchor),
		URL:                                  comment.URL,
		Body:                                 comment.Body,
		LastCommentID:                        comment.ID,
		LastCommentAuthor:                    comment.Author,
		LastCommentAuthoredByPostingIdentity: comment.AuthoredByPostingIdentity,
		LastCommentHasThreadSummaryMarker:    comment.HasThreadSummaryMarker,
	}
}

func anchorPresent(anchor Anchor) bool {
	return strings.TrimSpace(anchor.Path) != "" ||
		anchor.Side != "" ||
		anchor.Line != 0 ||
		anchor.SubjectType != "" ||
		strings.TrimSpace(anchor.CommitSHA) != ""
}

func commentLess(left, right Comment) bool {
	leftTime, rightTime := effectiveTime(left), effectiveTime(right)
	if !leftTime.Equal(rightTime) {
		return leftTime.Before(rightTime)
	}
	if left.providerOrder != right.providerOrder {
		return left.providerOrder < right.providerOrder
	}
	return left.ID < right.ID
}

func threadLess(left, right Thread) bool {
	if left.Anchor.Path != right.Anchor.Path {
		return left.Anchor.Path < right.Anchor.Path
	}
	if left.Anchor.Line != right.Anchor.Line {
		return left.Anchor.Line < right.Anchor.Line
	}
	return left.ID < right.ID
}

func effectiveTime(comment Comment) time.Time {
	if !comment.CreatedAt.IsZero() {
		return comment.CreatedAt
	}
	return comment.UpdatedAt
}

func commentThreadID(commentID gitprovider.ThreadID, threadID gitprovider.ThreadID) gitprovider.ThreadID {
	if strings.TrimSpace(string(commentID)) != "" {
		return commentID
	}
	return threadID
}

func githubBotLoginEquivalent(left, right string) bool {
	trimmedLeft := strings.TrimSpace(left)
	trimmedRight := strings.TrimSpace(right)
	if !strings.HasSuffix(trimmedLeft, "[bot]") && !strings.HasSuffix(trimmedRight, "[bot]") {
		return false
	}
	left = normalizedGitHubBotLogin(trimmedLeft)
	right = normalizedGitHubBotLogin(trimmedRight)
	return left != "" && left == right
}

func normalizedGitHubBotLogin(login string) string {
	login = strings.TrimSpace(login)
	return strings.TrimSuffix(login, "[bot]")
}
