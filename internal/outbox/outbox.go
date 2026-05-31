// Package outbox posts planned review actions through the host-agnostic provider seam.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	exitOK       = 0
	exitFailed   = 1
	exitAuth     = 3
	exitUpstream = 5
)

// Store is the durable state required by the post phase.
type Store interface {
	ListPlannedActions(ctx context.Context, runID string) ([]ledger.PlannedAction, error)
	UpdatePlannedAction(ctx context.Context, action ledger.PlannedAction) error
	CompleteRun(ctx context.Context, runID string, outcome ledger.Outcome, completedAt time.Time) error
}

// Limiter gates provider write calls.
type Limiter interface {
	Wait(context.Context, string) error
}

// Options contains post-phase dependencies.
type Options struct {
	Store    Store
	Provider gitprovider.GitProvider
	Limiter  Limiter
	Now      func() time.Time
}

// Request identifies the run and PR being posted.
type Request struct {
	Run             ledger.Run
	PRRef           gitprovider.PRRef
	PostingIdentity gitprovider.Identity
	DesiredOutcome  ledger.Outcome
}

// Result summarizes post-phase state after Post returns.
type Result struct {
	Outcome        ledger.Outcome
	ExitCode       int
	Posted         int
	Pending        int
	FailedTerminal int
	Aborted        bool
}

// InlineCommentPayload is the JSON payload for inline comment actions.
type InlineCommentPayload struct {
	Body         string            `json:"body"`
	Path         string            `json:"path"`
	Side         review.DiffSide   `json:"side"`
	Line         int               `json:"line"`
	SubjectType  review.AnchorKind `json:"subject_type"`
	DiffPosition int               `json:"diff_position"`
}

// ThreadReplyPayload is the JSON payload for thread reply actions.
type ThreadReplyPayload struct {
	Body    string `json:"body"`
	Summary bool   `json:"summary"`
}

// ResolveThreadPayload is the JSON payload for resolve thread actions.
type ResolveThreadPayload struct{}

// RollupCommentPayload is the JSON payload for rollup comment actions.
type RollupCommentPayload struct {
	Body string `json:"body"`
}

// SubmitReviewPayload is the JSON payload for submit review actions.
type SubmitReviewPayload struct {
	Body  string             `json:"body"`
	Event review.ReviewEvent `json:"event"`
}

type hostState struct {
	threads       []gitprovider.InlineThread
	issueComments []gitprovider.IssueComment
	reviews       []gitprovider.Review
}

type actionPlan struct {
	action ledger.PlannedAction
	body   string

	inlineComment *gitprovider.InlineComment
	threadID      gitprovider.ThreadID
	resolveThread gitprovider.ThreadID
	reviewRequest *gitprovider.ReviewRequest
}

// Post reconciles and posts pending actions for one live run.
func Post(ctx context.Context, opts Options, req Request) (Result, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := validateRequest(opts, req); err != nil {
		return Result{ExitCode: exitFailed}, err
	}

	actions, err := opts.Store.ListPlannedActions(ctx, req.Run.RunID)
	if err != nil {
		return Result{ExitCode: exitFailed}, err
	}
	actions = sortActions(actions)

	if !hasRelevantPending(actions) {
		outcome, exitCode := finalOutcome(req.DesiredOutcome, actions)
		if err := opts.Store.CompleteRun(ctx, req.Run.RunID, outcome, opts.Now().UTC()); err != nil {
			return Result{ExitCode: exitFailed}, err
		}
		return summarize(actions, outcome, exitCode, false), nil
	}

	state, err := readHostState(ctx, opts.Provider, req.PRRef)
	if err != nil {
		return Result{ExitCode: exitUpstream}, err
	}

	for i := range actions {
		if actions[i].Status != ledger.PlannedActionPending {
			continue
		}
		if irrelevant(actions[i]) {
			continue
		}
		match, err := reconcileAction(req, actions, actions[i], state)
		if err != nil {
			// Reconciliation is best-effort; dispatch planning below performs the
			// authoritative local validation and terminal state update.
			continue
		}
		if !match.ok {
			continue
		}
		now := opts.Now().UTC()
		actions[i].Status = ledger.PlannedActionPosted
		actions[i].PostedAt = &now
		actions[i].UpstreamID = strPtr(match.upstreamID)
		actions[i].Error = nil
		actions[i].FailureClass = nil
		if err := opts.Store.UpdatePlannedAction(ctx, actions[i]); err != nil {
			return Result{ExitCode: exitFailed}, err
		}
	}

	for i := range actions {
		if actions[i].Status != ledger.PlannedActionPending {
			continue
		}
		if irrelevant(actions[i]) {
			continue
		}
		plan, err := buildActionPlan(req, actions[i])
		if err != nil {
			if updateErr := markFailedTerminal(ctx, opts.Store, &actions[i], err, failureTerminal); updateErr != nil {
				return Result{ExitCode: exitFailed}, updateErr
			}
			continue
		}

		if err := opts.Limiter.Wait(ctx, req.PRRef.Host); err != nil {
			if isContextError(err) {
				return summarize(actions, ledger.OutcomeIncomplete, exitUpstream, false), err
			}
			if updateErr := recordPendingError(ctx, opts.Store, &actions[i], err); updateErr != nil {
				return Result{ExitCode: exitFailed}, updateErr
			}
			return summarize(actions, ledger.OutcomeIncomplete, exitUpstream, false), err
		}

		now := opts.Now().UTC()
		actions[i].Attempts++
		actions[i].AttemptedAt = &now
		if err := opts.Store.UpdatePlannedAction(ctx, actions[i]); err != nil {
			return Result{ExitCode: exitFailed}, err
		}

		upstreamID, err := dispatch(ctx, opts.Provider, req.PRRef, plan)
		if err == nil {
			now := opts.Now().UTC()
			actions[i].Status = ledger.PlannedActionPosted
			actions[i].PostedAt = &now
			actions[i].UpstreamID = strPtr(upstreamID)
			actions[i].Error = nil
			actions[i].FailureClass = nil
			if err := opts.Store.UpdatePlannedAction(ctx, actions[i]); err != nil {
				return Result{ExitCode: exitFailed}, err
			}
			continue
		}

		if errors.Is(err, gitprovider.ErrStaleSHA) {
			if updateErr := recordPendingError(ctx, opts.Store, &actions[i], err); updateErr != nil {
				return Result{ExitCode: exitFailed}, updateErr
			}
			if completeErr := opts.Store.CompleteRun(ctx, req.Run.RunID, ledger.OutcomeAborted, opts.Now().UTC()); completeErr != nil {
				return Result{ExitCode: exitFailed}, completeErr
			}
			result := summarize(actions, ledger.OutcomeAborted, exitUpstream, true)
			return result, nil
		}

		if errors.Is(err, gitprovider.ErrConflict) {
			refetched, readErr := readHostState(ctx, opts.Provider, req.PRRef)
			if readErr != nil {
				if updateErr := recordPendingError(ctx, opts.Store, &actions[i], readErr); updateErr != nil {
					return Result{ExitCode: exitFailed}, updateErr
				}
				return summarize(actions, ledger.OutcomeIncomplete, exitUpstream, false), readErr
			}
			match, matchErr := reconcileAction(req, actions, actions[i], refetched)
			if matchErr == nil && match.ok {
				now := opts.Now().UTC()
				actions[i].Status = ledger.PlannedActionPosted
				actions[i].PostedAt = &now
				actions[i].UpstreamID = strPtr(match.upstreamID)
				actions[i].Error = nil
				actions[i].FailureClass = nil
				if updateErr := opts.Store.UpdatePlannedAction(ctx, actions[i]); updateErr != nil {
					return Result{ExitCode: exitFailed}, updateErr
				}
				continue
			}
			conflictClass := classifyConflictCause(err)
			if conflictClass == failureRetryable {
				if updateErr := recordPendingError(ctx, opts.Store, &actions[i], err); updateErr != nil {
					return Result{ExitCode: exitFailed}, updateErr
				}
				continue
			}
			if updateErr := markFailedTerminal(ctx, opts.Store, &actions[i], err, conflictClass); updateErr != nil {
				return Result{ExitCode: exitFailed}, updateErr
			}
			continue
		}

		failureClass := classifyProviderError(err)
		switch failureClass {
		case failureRetryable:
			if updateErr := recordPendingError(ctx, opts.Store, &actions[i], err); updateErr != nil {
				return Result{ExitCode: exitFailed}, updateErr
			}
		case failureTerminal, failureAuth:
			if updateErr := markFailedTerminal(ctx, opts.Store, &actions[i], err, failureClass); updateErr != nil {
				return Result{ExitCode: exitFailed}, updateErr
			}
		}
	}

	outcome, exitCode := finalOutcome(req.DesiredOutcome, actions)
	if err := opts.Store.CompleteRun(ctx, req.Run.RunID, outcome, opts.Now().UTC()); err != nil {
		return Result{ExitCode: exitFailed}, err
	}
	return summarize(actions, outcome, exitCode, false), nil
}

func validateRequest(opts Options, req Request) error {
	if opts.Store == nil {
		return fmt.Errorf("outbox: store is required")
	}
	if opts.Provider == nil {
		return fmt.Errorf("outbox: provider is required")
	}
	if opts.Limiter == nil {
		return fmt.Errorf("outbox: limiter is required")
	}
	if err := req.PRRef.Validate(); err != nil {
		return fmt.Errorf("outbox: invalid PR ref: %w", err)
	}
	for field, value := range map[string]string{
		"run ID":   req.Run.RunID,
		"head SHA": req.Run.SHA,
		"base SHA": req.Run.BaseSHA,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("outbox: %s is required", field)
		}
	}
	if req.Run.PostMode != ledger.PostModeLive {
		return fmt.Errorf("outbox: live post mode is required")
	}
	if strings.TrimSpace(req.PostingIdentity.ID) == "" && strings.TrimSpace(req.PostingIdentity.Login) == "" {
		return fmt.Errorf("outbox: posting identity is required")
	}
	if !validDesiredOutcome(req.DesiredOutcome) {
		return fmt.Errorf("outbox: invalid desired outcome %q", req.DesiredOutcome)
	}
	return nil
}

func validDesiredOutcome(outcome ledger.Outcome) bool {
	return outcome == ledger.OutcomeApproved ||
		outcome == ledger.OutcomeRequestChanges ||
		outcome == ledger.OutcomeComment ||
		outcome == ledger.OutcomeNothingToReview
}

func readHostState(ctx context.Context, provider gitprovider.GitProvider, ref gitprovider.PRRef) (hostState, error) {
	threads, err := provider.ListInlineThreads(ctx, ref)
	if err != nil {
		return hostState{}, err
	}
	comments, err := provider.ListIssueComments(ctx, ref)
	if err != nil {
		return hostState{}, err
	}
	reviews, err := provider.ListReviews(ctx, ref)
	if err != nil {
		return hostState{}, err
	}
	return hostState{threads: threads, issueComments: comments, reviews: reviews}, nil
}

type reconcileMatch struct {
	ok         bool
	upstreamID string
}

func reconcileAction(req Request, actions []ledger.PlannedAction, action ledger.PlannedAction, state hostState) (reconcileMatch, error) {
	switch action.Kind {
	case ledger.PlannedActionInlineComment:
		return reconcileInline(req, action, state), nil
	case ledger.PlannedActionThreadReply:
		payload, err := decodePayload[ThreadReplyPayload](action)
		if err != nil {
			return reconcileMatch{}, err
		}
		return reconcileThreadReply(req, action, payload, state), nil
	case ledger.PlannedActionRollupComment:
		return reconcileRollup(req, action, state), nil
	case ledger.PlannedActionSubmitReview:
		return reconcileSubmitReview(req, action, state), nil
	case ledger.PlannedActionResolveThread:
		if _, err := decodePayload[ResolveThreadPayload](action); err != nil {
			return reconcileMatch{}, err
		}
		return reconcileResolve(action, actions, state), nil
	default:
		return reconcileMatch{}, fmt.Errorf("outbox: unsupported action kind %q", action.Kind)
	}
}

func reconcileInline(req Request, action ledger.PlannedAction, state hostState) reconcileMatch {
	for _, thread := range state.threads {
		for _, comment := range thread.Comments {
			if !sameIdentity(comment.Author, req.PostingIdentity) {
				continue
			}
			if actionMarkerMatches(req, action, marker.ActionKindInlineComment, "", comment.Body) {
				return reconcileMatch{ok: true, upstreamID: string(comment.ID)}
			}
		}
	}
	return reconcileMatch{}
}

func reconcileThreadReply(req Request, action ledger.PlannedAction, payload ThreadReplyPayload, state hostState) reconcileMatch {
	if action.ThreadID == nil {
		return reconcileMatch{}
	}
	for _, thread := range state.threads {
		if thread.ID != gitprovider.ThreadID(*action.ThreadID) {
			continue
		}
		for _, comment := range thread.Comments {
			if !sameIdentity(comment.Author, req.PostingIdentity) {
				continue
			}
			if payload.Summary {
				for _, found := range marker.FindThreadSummaries(comment.Body) {
					if found.RunID == req.Run.RunID && found.ActionID == action.ActionID {
						return reconcileMatch{ok: true, upstreamID: string(comment.ID)}
					}
				}
				continue
			}
			if actionMarkerMatches(req, action, marker.ActionKindThreadReply, "", comment.Body) {
				return reconcileMatch{ok: true, upstreamID: string(comment.ID)}
			}
		}
	}
	return reconcileMatch{}
}

func reconcileRollup(req Request, action ledger.PlannedAction, state hostState) reconcileMatch {
	for _, comment := range state.issueComments {
		if !sameIdentity(comment.Author, req.PostingIdentity) {
			continue
		}
		if actionMarkerMatches(req, action, marker.ActionKindRollupComment, string(req.DesiredOutcome), comment.Body) {
			return reconcileMatch{ok: true, upstreamID: string(comment.ID)}
		}
	}
	return reconcileMatch{}
}

func reconcileSubmitReview(req Request, action ledger.PlannedAction, state hostState) reconcileMatch {
	for _, review := range state.reviews {
		if !sameIdentity(review.Author, req.PostingIdentity) {
			continue
		}
		if actionMarkerMatches(req, action, marker.ActionKindSubmitReview, "", review.Body) {
			return reconcileMatch{ok: true, upstreamID: string(review.ID)}
		}
	}
	return reconcileMatch{}
}

func reconcileResolve(action ledger.PlannedAction, actions []ledger.PlannedAction, state hostState) reconcileMatch {
	if action.ThreadID == nil {
		return reconcileMatch{}
	}
	threadID := gitprovider.ThreadID(*action.ThreadID)
	resolved := false
	for _, thread := range state.threads {
		if thread.ID == threadID {
			resolved = thread.Resolved
			break
		}
	}
	if !resolved || !sameThreadReplyPosted(action, actions) {
		return reconcileMatch{}
	}
	return reconcileMatch{ok: true, upstreamID: string(threadID)}
}

func sameThreadReplyPosted(action ledger.PlannedAction, actions []ledger.PlannedAction) bool {
	if action.ThreadID == nil {
		return false
	}
	for _, candidate := range actions {
		if candidate.Kind != ledger.PlannedActionThreadReply || candidate.Status != ledger.PlannedActionPosted {
			continue
		}
		if candidate.ThreadID != nil && *candidate.ThreadID == *action.ThreadID {
			return true
		}
	}
	return false
}

func actionMarkerMatches(req Request, action ledger.PlannedAction, kind string, outcome string, body string) bool {
	for _, found := range marker.FindActions(body) {
		if found.RunID != req.Run.RunID {
			continue
		}
		if found.ActionID != action.ActionID {
			continue
		}
		if found.Kind != kind || found.SHA != req.Run.SHA || found.BaseSHA != req.Run.BaseSHA {
			continue
		}
		if found.Outcome != outcome {
			continue
		}
		return true
	}
	return false
}

func buildActionPlan(req Request, action ledger.PlannedAction) (actionPlan, error) {
	switch action.Kind {
	case ledger.PlannedActionInlineComment:
		payload, err := decodePayload[InlineCommentPayload](action)
		if err != nil {
			return actionPlan{}, err
		}
		body, err := bodyWithActionMarker(req, action, marker.ActionKindInlineComment, "", payload.Body)
		if err != nil {
			return actionPlan{}, err
		}
		comment := gitprovider.InlineComment{
			CommitSHA:    req.Run.SHA,
			Body:         body,
			Path:         payload.Path,
			Side:         payload.Side,
			Line:         payload.Line,
			SubjectType:  payload.SubjectType,
			DiffPosition: payload.DiffPosition,
		}
		if err := comment.Validate(); err != nil {
			return actionPlan{}, err
		}
		return actionPlan{action: action, inlineComment: &comment}, nil
	case ledger.PlannedActionThreadReply:
		payload, err := decodePayload[ThreadReplyPayload](action)
		if err != nil {
			return actionPlan{}, err
		}
		if action.ThreadID == nil || strings.TrimSpace(*action.ThreadID) == "" {
			return actionPlan{}, fmt.Errorf("outbox: thread reply target is required")
		}
		var body string
		if payload.Summary {
			body, err = bodyWithThreadSummaryMarker(req, action, payload.Body)
		} else {
			body, err = bodyWithActionMarker(req, action, marker.ActionKindThreadReply, "", payload.Body)
		}
		if err != nil {
			return actionPlan{}, err
		}
		return actionPlan{action: action, body: body, threadID: gitprovider.ThreadID(*action.ThreadID)}, nil
	case ledger.PlannedActionResolveThread:
		if _, err := decodePayload[ResolveThreadPayload](action); err != nil {
			return actionPlan{}, err
		}
		if action.ThreadID == nil || strings.TrimSpace(*action.ThreadID) == "" {
			return actionPlan{}, fmt.Errorf("outbox: resolve thread target is required")
		}
		return actionPlan{action: action, resolveThread: gitprovider.ThreadID(*action.ThreadID)}, nil
	case ledger.PlannedActionRollupComment:
		payload, err := decodePayload[RollupCommentPayload](action)
		if err != nil {
			return actionPlan{}, err
		}
		body, err := bodyWithActionMarker(req, action, marker.ActionKindRollupComment, string(req.DesiredOutcome), payload.Body)
		if err != nil {
			return actionPlan{}, err
		}
		return actionPlan{action: action, body: body}, nil
	case ledger.PlannedActionSubmitReview:
		payload, err := decodePayload[SubmitReviewPayload](action)
		if err != nil {
			return actionPlan{}, err
		}
		body, err := bodyWithActionMarker(req, action, marker.ActionKindSubmitReview, "", payload.Body)
		if err != nil {
			return actionPlan{}, err
		}
		request := gitprovider.ReviewRequest{CommitSHA: req.Run.SHA, Event: payload.Event, Body: body}
		if err := request.Validate(); err != nil {
			return actionPlan{}, err
		}
		return actionPlan{action: action, reviewRequest: &request}, nil
	default:
		return actionPlan{}, fmt.Errorf("outbox: unsupported action kind %q", action.Kind)
	}
}

func decodePayload[T any](action ledger.PlannedAction) (T, error) {
	var payload T
	if err := json.Unmarshal([]byte(action.PayloadJSON), &payload); err != nil {
		return payload, fmt.Errorf("outbox: decode %s payload: %w", action.Kind, err)
	}
	return payload, nil
}

func bodyWithActionMarker(req Request, action ledger.PlannedAction, kind string, outcome string, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("outbox: body is required")
	}
	rendered, err := marker.RenderAction(marker.ActionMarker{
		RunID:    req.Run.RunID,
		ActionID: action.ActionID,
		Kind:     kind,
		SHA:      req.Run.SHA,
		BaseSHA:  req.Run.BaseSHA,
		Outcome:  outcome,
	})
	if err != nil {
		return "", err
	}
	return marker.RenderSkip() + "\n" + rendered + "\n\n" + marker.SanitizeModelContent(body), nil
}

func bodyWithThreadSummaryMarker(req Request, action ledger.PlannedAction, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("outbox: body is required")
	}
	rendered, err := marker.RenderThreadSummary(marker.ThreadSummaryMarker{
		RunID:    req.Run.RunID,
		ActionID: action.ActionID,
	})
	if err != nil {
		return "", err
	}
	return rendered + "\n\n" + marker.SanitizeModelContent(body), nil
}

func dispatch(ctx context.Context, provider gitprovider.GitProvider, ref gitprovider.PRRef, plan actionPlan) (string, error) {
	switch plan.action.Kind {
	case ledger.PlannedActionInlineComment:
		id, err := provider.PostInlineComment(ctx, ref, *plan.inlineComment)
		return string(id), err
	case ledger.PlannedActionThreadReply:
		id, err := provider.ReplyToThread(ctx, ref, plan.threadID, plan.body)
		return string(id), err
	case ledger.PlannedActionResolveThread:
		err := provider.ResolveThread(ctx, ref, plan.resolveThread)
		return string(plan.resolveThread), err
	case ledger.PlannedActionRollupComment:
		id, err := provider.PostIssueComment(ctx, ref, plan.body)
		return string(id), err
	case ledger.PlannedActionSubmitReview:
		id, err := provider.SubmitReview(ctx, ref, *plan.reviewRequest)
		return string(id), err
	default:
		return "", fmt.Errorf("outbox: unsupported action kind %q", plan.action.Kind)
	}
}

func recordPendingError(ctx context.Context, store Store, action *ledger.PlannedAction, err error) error {
	action.Status = ledger.PlannedActionPending
	action.Error = strPtr(err.Error())
	action.FailureClass = nil
	return store.UpdatePlannedAction(ctx, *action)
}

func markFailedTerminal(ctx context.Context, store Store, action *ledger.PlannedAction, err error, class failureClass) error {
	action.Status = ledger.PlannedActionFailedTerminal
	action.Error = strPtr(err.Error())
	action.FailureClass = failureClassPtr(class)
	return store.UpdatePlannedAction(ctx, *action)
}

type failureClass int

const (
	failureTerminal failureClass = iota
	failureRetryable
	failureAuth
)

const (
	failureClassStorageTerminal = "terminal"
	failureClassStorageAuth     = "auth"
)

func failureClassPtr(class failureClass) *string {
	value := failureClassStorageTerminal
	if class == failureAuth {
		value = failureClassStorageAuth
	}
	return &value
}

func terminalFailureClass(action ledger.PlannedAction) failureClass {
	if action.FailureClass != nil && *action.FailureClass == failureClassStorageAuth {
		return failureAuth
	}
	return failureTerminal
}

func classifyProviderError(err error) failureClass {
	switch {
	case errors.Is(err, gitprovider.ErrRetryable):
		return failureRetryable
	case errors.Is(err, gitprovider.ErrAuth), errors.Is(err, gitprovider.ErrPermission):
		return failureAuth
	default:
		return failureTerminal
	}
}

func classifyConflictCause(err error) failureClass {
	switch {
	case errors.Is(err, gitprovider.ErrRetryable):
		return failureRetryable
	case errors.Is(err, gitprovider.ErrAuth), errors.Is(err, gitprovider.ErrPermission):
		return failureAuth
	case errors.Is(err, gitprovider.ErrNotFound):
		return failureTerminal
	default:
		return failureTerminal
	}
}

func finalOutcome(success ledger.Outcome, actions []ledger.PlannedAction) (ledger.Outcome, int) {
	hasRequiredPending := false
	hasRequiredTerminal := false
	hasAuthTerminal := false
	for _, action := range actions {
		if !action.Required {
			continue
		}
		switch action.Status {
		case ledger.PlannedActionSuperseded, ledger.PlannedActionPlannedOnly:
			continue
		case ledger.PlannedActionPending:
			hasRequiredPending = true
		case ledger.PlannedActionFailedTerminal:
			hasRequiredTerminal = true
			if terminalFailureClass(action) == failureAuth {
				hasAuthTerminal = true
			}
		case ledger.PlannedActionPosted:
		default:
			hasRequiredPending = true
		}
	}
	switch {
	case hasRequiredTerminal && hasAuthTerminal:
		return ledger.OutcomeFailed, exitAuth
	case hasRequiredTerminal:
		return ledger.OutcomeFailed, exitFailed
	case hasRequiredPending:
		return ledger.OutcomeIncomplete, exitUpstream
	default:
		return success, exitOK
	}
}

func summarize(actions []ledger.PlannedAction, outcome ledger.Outcome, exitCode int, aborted bool) Result {
	result := Result{Outcome: outcome, ExitCode: exitCode, Aborted: aborted}
	for _, action := range actions {
		if irrelevant(action) {
			continue
		}
		switch action.Status {
		case ledger.PlannedActionPosted:
			result.Posted++
		case ledger.PlannedActionPending:
			result.Pending++
		case ledger.PlannedActionFailedTerminal:
			result.FailedTerminal++
		case ledger.PlannedActionSuperseded, ledger.PlannedActionPlannedOnly:
		}
	}
	return result
}

func irrelevant(action ledger.PlannedAction) bool {
	return action.Status == ledger.PlannedActionPlannedOnly || action.Status == ledger.PlannedActionSuperseded
}

func hasRelevantPending(actions []ledger.PlannedAction) bool {
	for _, action := range actions {
		if action.Status == ledger.PlannedActionPending {
			return true
		}
	}
	return false
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func sortActions(actions []ledger.PlannedAction) []ledger.PlannedAction {
	sorted := append([]ledger.PlannedAction(nil), actions...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && actionLess(sorted[j], sorted[j-1]); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted
}

func actionLess(a, b ledger.PlannedAction) bool {
	ao, bo := actionOrder(a.Kind), actionOrder(b.Kind)
	if ao != bo {
		return ao < bo
	}
	return a.ActionID < b.ActionID
}

func actionOrder(kind ledger.PlannedActionKind) int {
	switch kind {
	case ledger.PlannedActionThreadReply:
		return 0
	case ledger.PlannedActionResolveThread:
		return 1
	case ledger.PlannedActionInlineComment:
		return 2
	case ledger.PlannedActionRollupComment:
		return 3
	case ledger.PlannedActionSubmitReview:
		return 4
	default:
		return 99
	}
}

func sameIdentity(author gitprovider.Identity, target gitprovider.Identity) bool {
	if strings.TrimSpace(author.ID) != "" && strings.TrimSpace(target.ID) != "" {
		return author.ID == target.ID
	}
	return strings.TrimSpace(author.Login) != "" && author.Login == target.Login
}

func strPtr(value string) *string {
	return &value
}
