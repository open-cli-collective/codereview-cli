package outbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const testWriteSettle = 12 * time.Second

// sendScript says what one write attempt does at the host.
type sendScript struct {
	land bool  // the host keeps the write
	err  error // what the client reports
}

// simHost is a host whose writes become visible to later reads, so reconciliation
// can be exercised against a write that landed but reported failure.
type simHost struct {
	identity gitprovider.Identity
	caps     gitprovider.ProviderCaps
	script   map[string][]sendScript // by write kind
	sends    map[string]int
	comments []gitprovider.IssueComment
	reviews  []gitprovider.Review
	thread   gitprovider.InlineThread
	reads    int
	readErrs []error // consumed by each host-state read after the first
}

func newSimHost(script map[string][]sendScript) *simHost {
	return &simHost{
		identity: botIdentity(),
		caps:     gitprovider.ProviderCaps{WriteSettle: testWriteSettle},
		script:   script,
		sends:    map[string]int{},
		thread:   gitprovider.InlineThread{ID: "thread-1", Path: "main.go", Side: review.DiffSideRight, Line: 12, SubjectType: review.AnchorKindLine},
	}
}

func (h *simHost) send(kind string) sendScript {
	h.sends[kind]++
	steps := h.script[kind]
	if h.sends[kind] <= len(steps) {
		return steps[h.sends[kind]-1]
	}
	return sendScript{land: true}
}

func (h *simHost) nextID(kind string) string { return fmt.Sprintf("%s-%d", kind, h.sends[kind]) }

func (h *simHost) ListInlineThreads(context.Context, gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	h.reads++
	if h.reads > 1 && len(h.readErrs) > 0 {
		err := h.readErrs[0]
		h.readErrs = h.readErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return []gitprovider.InlineThread{h.thread}, nil
}

func (h *simHost) ListReviews(context.Context, gitprovider.PRRef) ([]gitprovider.Review, error) {
	return slices.Clone(h.reviews), nil
}

func (h *simHost) ListIssueComments(context.Context, gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	return slices.Clone(h.comments), nil
}

func (h *simHost) PostInlineComment(context.Context, gitprovider.PRRef, gitprovider.InlineComment) (gitprovider.CommentID, error) {
	return "", errors.New("simHost: inline comments are bundled")
}

func (h *simHost) ReplyToThread(_ context.Context, _ gitprovider.PRRef, _ gitprovider.ThreadID, body string) (gitprovider.CommentID, error) {
	step := h.send("reply")
	id := gitprovider.CommentID(h.nextID("reply"))
	if step.land {
		h.thread.Comments = append(h.thread.Comments, gitprovider.ThreadComment{ID: id, ThreadID: h.thread.ID, Author: h.identity, Body: body})
	}
	if step.err != nil {
		return "", step.err
	}
	return id, nil
}

func (h *simHost) ResolveThread(context.Context, gitprovider.PRRef, gitprovider.ThreadID) error {
	step := h.send("resolve")
	if step.land {
		h.thread.Resolved = true
	}
	return step.err
}

func (h *simHost) PostIssueComment(_ context.Context, _ gitprovider.PRRef, body string) (gitprovider.CommentID, error) {
	step := h.send("rollup")
	id := gitprovider.CommentID(h.nextID("rollup"))
	if step.land {
		h.comments = append(h.comments, gitprovider.IssueComment{ID: id, Author: h.identity, Body: body})
	}
	if step.err != nil {
		return "", step.err
	}
	return id, nil
}

func (h *simHost) SubmitReview(_ context.Context, _ gitprovider.PRRef, request gitprovider.ReviewRequest) (gitprovider.ReviewID, error) {
	step := h.send("review")
	id := gitprovider.ReviewID(h.nextID("review"))
	if step.land {
		h.reviews = append(h.reviews, gitprovider.Review{ID: id, Author: h.identity, Body: request.Body})
		for _, comment := range request.Comments {
			h.thread.Comments = append(h.thread.Comments, gitprovider.ThreadComment{ID: gitprovider.CommentID(id), ThreadID: h.thread.ID, Author: h.identity, Body: comment.Body})
		}
	}
	if step.err != nil {
		return "", step.err
	}
	return id, nil
}

func (h *simHost) Capabilities() gitprovider.ProviderCaps { return h.caps }

func retryableWriteError(op gitprovider.Operation) error {
	return gitprovider.WrapError(gitprovider.ErrRetryable, op, errors.New("github: status 502"))
}

type writeRetryRecorder struct {
	delays []time.Duration
	events []WriteRetryEvent
}

func (r *writeRetryRecorder) policy() *WriteRetry {
	return &WriteRetry{
		Jitter: func(d time.Duration) time.Duration { return d },
		Sleep: func(_ context.Context, d time.Duration) error {
			r.delays = append(r.delays, d)
			return nil
		},
		OnEvent: func(event WriteRetryEvent) { r.events = append(r.events, event) },
	}
}

func (r *writeRetryRecorder) phases() []WriteRetryPhase {
	phases := make([]WriteRetryPhase, 0, len(r.events))
	for _, event := range r.events {
		phases = append(phases, event.Phase)
	}
	return phases
}

func writeRetryKinds() []struct {
	name   string
	send   string
	action func(runID string) ledger.PlannedAction
	count  func(*simHost) int
} {
	return []struct {
		name   string
		send   string
		action func(runID string) ledger.PlannedAction
		count  func(*simHost) int
	}{
		{
			name: "rollup comment",
			send: "rollup",
			action: func(runID string) ledger.PlannedAction {
				return plannedAction(runID, "action-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"})
			},
			count: func(h *simHost) int { return len(h.comments) },
		},
		{
			name: "submit review",
			send: "review",
			action: func(runID string) ledger.PlannedAction {
				return plannedAction(runID, "action-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{Body: "review", Event: review.ReviewEventComment})
			},
			count: func(h *simHost) int { return len(h.reviews) },
		},
		{
			name: "thread reply",
			send: "reply",
			action: func(runID string) ledger.PlannedAction {
				return plannedAction(runID, "action-1", ledger.PlannedActionThreadReply, true, "thread-1", ThreadReplyPayload{Body: "reply"})
			},
			count: func(h *simHost) int { return len(h.thread.Comments) },
		},
	}
}

func TestWriteThatLandedAndReturned5xxIsNeverSentTwice(t *testing.T) {
	for _, tt := range writeRetryKinds() {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t)
			run := allocateRun(t, store, ledger.PostModeLive)
			host := newSimHost(map[string][]sendScript{tt.send: {{land: true, err: retryableWriteError(gitprovider.OperationSubmitReview)}}})
			insertAction(t, store, tt.action(run.RunID))
			recorder := &writeRetryRecorder{}

			result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}, testRequest(run))
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			if host.sends[tt.send] != 1 || tt.count(host) != 1 {
				t.Fatalf("sends = %d, on host = %d, want one of each", host.sends[tt.send], tt.count(host))
			}
			if result.ExitCode != exitOK || result.Posted != 1 || result.Pending != 0 {
				t.Fatalf("result = %#v, want posted", result)
			}
			action := actionByID(t, store, run.RunID, "action-1")
			if action.Status != ledger.PlannedActionPosted || action.Attempts != 1 || action.Error != nil || action.UpstreamID == nil || *action.UpstreamID == "" {
				t.Fatalf("action = %#v, want posted from the marker match", action)
			}
			if !slices.Equal(recorder.phases(), []WriteRetryPhase{WriteRetryWaiting, WriteRetryReconciled}) {
				t.Fatalf("phases = %v", recorder.phases())
			}
		})
	}
}

func TestWriteThatDidNotLandIsResentOnceMarkerIsAbsent(t *testing.T) {
	for _, tt := range writeRetryKinds() {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t)
			run := allocateRun(t, store, ledger.PostModeLive)
			host := newSimHost(map[string][]sendScript{tt.send: {{land: false, err: retryableWriteError(gitprovider.OperationSubmitReview)}}})
			insertAction(t, store, tt.action(run.RunID))
			recorder := &writeRetryRecorder{}

			result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}, testRequest(run))
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			if host.sends[tt.send] != 2 || tt.count(host) != 1 {
				t.Fatalf("sends = %d, on host = %d, want two sends and one on the host", host.sends[tt.send], tt.count(host))
			}
			if result.ExitCode != exitOK || result.Posted != 1 {
				t.Fatalf("result = %#v, want posted", result)
			}
			action := actionByID(t, store, run.RunID, "action-1")
			if action.Status != ledger.PlannedActionPosted || action.Attempts != 2 || action.Error != nil {
				t.Fatalf("action = %#v, want posted after two attempts", action)
			}
			if !slices.Equal(recorder.phases(), []WriteRetryPhase{WriteRetryWaiting, WriteRetryResending}) {
				t.Fatalf("phases = %v", recorder.phases())
			}
			if host.reads != 2 {
				t.Fatalf("host-state reads = %d, want the opening read and one before the resend", host.reads)
			}
		})
	}
}

func TestResentWriteThatLandsOnSecondFailureIsNotSentAThirdTime(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	failure := retryableWriteError(gitprovider.OperationSubmitReview)
	host := newSimHost(map[string][]sendScript{"review": {{land: false, err: failure}, {land: true, err: failure}}})
	insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{Body: "review", Event: review.ReviewEventComment}))
	recorder := &writeRetryRecorder{}

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if host.sends["review"] != 2 || len(host.reviews) != 1 || result.ExitCode != exitOK {
		t.Fatalf("sends = %d reviews = %d result = %#v, want two sends, one review, success", host.sends["review"], len(host.reviews), result)
	}
	if want := []time.Duration{testWriteSettle, 2 * testWriteSettle}; !slices.Equal(recorder.delays, want) {
		t.Fatalf("delays = %v, want %v", recorder.delays, want)
	}
}

func TestFailedMarkerCheckNeverResends(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	host := newSimHost(map[string][]sendScript{"review": {{land: false, err: retryableWriteError(gitprovider.OperationSubmitReview)}}})
	host.readErrs = []error{retryableWriteError(gitprovider.OperationListInlineThreads)}
	insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{Body: "review", Event: review.ReviewEventComment}))
	recorder := &writeRetryRecorder{}

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if host.sends["review"] != 1 {
		t.Fatalf("sends = %d, want 1: a write must not be resent unverified", host.sends["review"])
	}
	if result.ExitCode != exitUpstream || result.Pending != 1 || result.Outcome != ledger.OutcomeIncomplete || !strings.Contains(result.PendingError, "status 502") {
		t.Fatalf("result = %#v, want incomplete with the cause", result)
	}
	if got := recorder.phases(); !slices.Equal(got, []WriteRetryPhase{WriteRetryWaiting, WriteRetryAbandoned}) {
		t.Fatalf("phases = %v", got)
	}
}

func TestExhaustedRetriesStayPendingAndTheNextRunReconciles(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	failure := retryableWriteError(gitprovider.OperationPostIssueComment)
	// The last send lands although it reports failure, so only the later run can learn of it.
	host := newSimHost(map[string][]sendScript{"rollup": {{err: failure}, {err: failure}, {land: true, err: failure}}})
	insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))
	recorder := &writeRetryRecorder{}
	opts := Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}

	result, err := Post(context.Background(), opts, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if host.sends["rollup"] != defaultWriteMaxAttempts || result.ExitCode != exitUpstream || result.Outcome != ledger.OutcomeIncomplete || result.Pending != 1 {
		t.Fatalf("sends = %d result = %#v, want %d sends and incomplete", host.sends["rollup"], result, defaultWriteMaxAttempts)
	}
	action := actionByID(t, store, run.RunID, "action-1")
	if action.Status != ledger.PlannedActionPending || action.Attempts != defaultWriteMaxAttempts || action.Error == nil {
		t.Fatalf("action = %#v, want pending with the error", action)
	}

	result, err = Post(context.Background(), opts, testRequest(run))
	if err != nil {
		t.Fatalf("second Post: %v", err)
	}
	if host.sends["rollup"] != defaultWriteMaxAttempts || len(host.comments) != 1 || result.ExitCode != exitOK {
		t.Fatalf("sends = %d comments = %d result = %#v, want no new send and success", host.sends["rollup"], len(host.comments), result)
	}
}

func TestBundledInlineCommentsFollowAReconciledReview(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	host := newSimHost(map[string][]sendScript{"review": {{land: true, err: retryableWriteError(gitprovider.OperationSubmitReview)}}})
	host.caps.BundleInlineOnSubmit = true
	insertAction(t, store, plannedAction(run.RunID, "inline-1", ledger.PlannedActionInlineComment, true, "", InlineCommentPayload{Body: "inline", Path: "main.go", Side: review.DiffSideRight, Line: 12, SubjectType: review.AnchorKindLine}))
	insertAction(t, store, plannedAction(run.RunID, "submit-1", ledger.PlannedActionSubmitReview, true, "", SubmitReviewPayload{Body: "review", Event: review.ReviewEventComment}))

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: (&writeRetryRecorder{}).policy()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if host.sends["review"] != 1 || result.ExitCode != exitOK || result.Posted != 2 {
		t.Fatalf("sends = %d result = %#v, want one send and both actions posted", host.sends["review"], result)
	}
	inline := actionByID(t, store, run.RunID, "inline-1")
	if inline.Status != ledger.PlannedActionPosted || inline.UpstreamID == nil || *inline.UpstreamID != "review-1" {
		t.Fatalf("inline = %#v, want posted with the review", inline)
	}
}

func TestResolveThreadRetriesAreSafeWithoutReply(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	host := newSimHost(map[string][]sendScript{"resolve": {{land: true, err: retryableWriteError(gitprovider.OperationResolveThread)}}})
	insertAction(t, store, plannedAction(run.RunID, "resolve-1", ledger.PlannedActionResolveThread, true, "thread-1", ResolveThreadPayload{}))

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: (&writeRetryRecorder{}).policy()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.ExitCode != exitOK || !host.thread.Resolved {
		t.Fatalf("result = %#v resolved = %t, want resolved", result, host.thread.Resolved)
	}
}

func TestRetryAfterExtendsTheSettleWaitAndLongWaitsAreAbandoned(t *testing.T) {
	hinted := func(after time.Duration) error {
		return &gitprovider.RetryAfterError{After: after, Err: retryableWriteError(gitprovider.OperationPostIssueComment)}
	}
	t.Run("honored", func(t *testing.T) {
		store := openStore(t)
		run := allocateRun(t, store, ledger.PostModeLive)
		host := newSimHost(map[string][]sendScript{"rollup": {{err: hinted(90 * time.Second)}}})
		insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))
		recorder := &writeRetryRecorder{}

		if _, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}, testRequest(run)); err != nil {
			t.Fatalf("Post: %v", err)
		}
		if !slices.Equal(recorder.delays, []time.Duration{90 * time.Second}) {
			t.Fatalf("delays = %v, want the requested 90s", recorder.delays)
		}
	})
	t.Run("too long", func(t *testing.T) {
		store := openStore(t)
		run := allocateRun(t, store, ledger.PostModeLive)
		host := newSimHost(map[string][]sendScript{"rollup": {{err: hinted(30 * time.Minute)}}})
		insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))
		recorder := &writeRetryRecorder{}

		result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}, testRequest(run))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if host.sends["rollup"] != 1 || len(recorder.delays) != 0 || result.ExitCode != exitUpstream {
			t.Fatalf("sends = %d delays = %v result = %#v, want one send, no wait, incomplete", host.sends["rollup"], recorder.delays, result)
		}
	})
}

func TestNonRetryableFailureAfterResendGoesThroughNormalHandling(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	host := newSimHost(map[string][]sendScript{"rollup": {
		{err: retryableWriteError(gitprovider.OperationPostIssueComment)},
		{err: gitprovider.WrapError(gitprovider.ErrPermission, gitprovider.OperationPostIssueComment, errors.New("github: status 403"))},
	}})
	insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: (&writeRetryRecorder{}).policy()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if host.sends["rollup"] != 2 || result.ExitCode != exitAuth || result.FailedTerminal != 1 {
		t.Fatalf("sends = %d result = %#v, want terminal auth failure", host.sends["rollup"], result)
	}
}

func TestSpentRetriesLeaveLaterActionsWithOneAttempt(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	failure := retryableWriteError(gitprovider.OperationReplyToThread)
	host := newSimHost(map[string][]sendScript{
		"reply":  {{err: failure}, {err: failure}, {err: failure}},
		"rollup": {{err: failure}},
	})
	insertAction(t, store, plannedAction(run.RunID, "reply-1", ledger.PlannedActionThreadReply, true, "thread-1", ThreadReplyPayload{Body: "reply"}))
	insertAction(t, store, plannedAction(run.RunID, "rollup-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))
	recorder := &writeRetryRecorder{}

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if host.sends["reply"] != defaultWriteMaxAttempts || host.sends["rollup"] != 1 {
		t.Fatalf("sends = %v, want the outage to stop retrying after one action", host.sends)
	}
	if result.ExitCode != exitUpstream || result.Pending != 2 {
		t.Fatalf("result = %#v, want both pending", result)
	}
}

func TestCancelDuringWriteRetryWaitLeavesActionPending(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	host := newSimHost(map[string][]sendScript{"rollup": {{err: retryableWriteError(gitprovider.OperationPostIssueComment)}}})
	insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))
	policy := &WriteRetry{Sleep: func(context.Context, time.Duration) error { return context.Canceled }}

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: policy}, testRequest(run))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result.ExitCode != exitUpstream || host.sends["rollup"] != 1 {
		t.Fatalf("result = %#v sends = %d, want upstream exit and no resend", result, host.sends["rollup"])
	}
	if action := actionByID(t, store, run.RunID, "action-1"); action.Status != ledger.PlannedActionPending {
		t.Fatalf("action = %#v, want pending", action)
	}
}

func TestPostWithoutWriteRetrySendsOnce(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	host := newSimHost(map[string][]sendScript{"rollup": {{err: retryableWriteError(gitprovider.OperationPostIssueComment)}}})
	insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if host.sends["rollup"] != 1 || result.ExitCode != exitUpstream {
		t.Fatalf("sends = %d result = %#v", host.sends["rollup"], result)
	}
}

func TestProviderWithoutWriteSettleIsNotResentInRun(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, ledger.PostModeLive)
	host := newSimHost(map[string][]sendScript{"rollup": {{err: retryableWriteError(gitprovider.OperationPostIssueComment)}}})
	host.caps.WriteSettle = 0
	insertAction(t, store, plannedAction(run.RunID, "action-1", ledger.PlannedActionRollupComment, true, "", RollupCommentPayload{Body: "rollup"}))
	recorder := &writeRetryRecorder{}

	result, err := Post(context.Background(), Options{Store: store, Provider: host, Limiter: noopLimiter{}, Now: fixedClock(), WriteRetry: recorder.policy()}, testRequest(run))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if host.sends["rollup"] != 1 || len(recorder.delays) != 0 || result.ExitCode != exitUpstream || result.Pending != 1 {
		t.Fatalf("sends = %d delays = %v result = %#v, want one send left pending", host.sends["rollup"], recorder.delays, result)
	}
}

func TestDefaultWriteJitterNeverShortensTheSettleWait(t *testing.T) {
	for range 200 {
		if got := addJitter(testWriteSettle); got < testWriteSettle || got >= testWriteSettle+testWriteSettle/4 {
			t.Fatalf("addJitter = %s", got)
		}
	}
}
