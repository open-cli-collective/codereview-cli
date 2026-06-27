// Package reviewplan builds the pure PR review action plan.
package reviewplan

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	defaultMaxInlineComments = 100
	inlineFooter             = "*Reply inline to this comment.*"
	fileLevelFallbackPrefix  = "_File-level note:_ "
	markerPrefix             = "<!-- codereview:"
	escapedMarkerPrefix      = "&lt;!-- codereview:"
)

// PostMode controls whether planned actions are postable or inspect-only.
type PostMode string

// PostMode values.
const (
	PostModeLive   PostMode = "live"
	PostModeDryRun PostMode = "dry_run"
)

// Valid reports whether m is a known post mode.
func (m PostMode) Valid() bool {
	switch m {
	case PostModeLive, PostModeDryRun:
		return true
	default:
		return false
	}
}

// Outcome is the planner-local review outcome.
type Outcome string

// Outcome values.
const (
	OutcomeApproved        Outcome = "approved"
	OutcomeComment         Outcome = "comment"
	OutcomeRequestChanges  Outcome = "request_changes"
	OutcomeNothingToReview Outcome = "nothing_to_review"
)

// Valid reports whether o is a known planner outcome.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeApproved, OutcomeComment, OutcomeRequestChanges, OutcomeNothingToReview:
		return true
	default:
		return false
	}
}

// ActionKind is the planner-local outbox action kind.
type ActionKind string

// Action kind values.
const (
	ActionKindInlineComment ActionKind = "inline_comment"
	ActionKindThreadReply   ActionKind = "thread_reply"
	ActionKindResolveThread ActionKind = "resolve_thread"
	ActionKindRollupComment ActionKind = "rollup_comment"
	ActionKindSubmitReview  ActionKind = "submit_review"
)

// ActionStatus is the planner-local planned action status.
type ActionStatus string

// Action status values.
const (
	ActionStatusPending     ActionStatus = "pending"
	ActionStatusPlannedOnly ActionStatus = "planned_only"
)

// ProviderCaps contains host capabilities that affect planning choices.
type ProviderCaps struct {
	NativeFileLevelComments bool
	ThreadResolution        bool
}

// EventOptions controls default review-event mapping.
type EventOptions struct {
	MajorEventRequestsChanges bool
	PostingIdentityIsPRAuthor bool
	AllowSelfApprove          bool
}

// ActionIDGenerator allocates a deterministic action id for an action kind.
type ActionIDGenerator func(ActionKind) (string, error)

// Request is the pure input to Build.
type Request struct {
	PostMode PostMode

	ProviderCaps ProviderCaps
	Diff         Diff

	Findings      []review.Finding
	Rollup        review.Rollup
	ThreadActions []review.ThreadAction
	// ThreadResponses are lifecycle-domain replies/resolutions for existing
	// inline discussion threads.
	ThreadResponses []review.ThreadResponseAction
	EventOptions    EventOptions

	NoDiff                  bool
	Profile                 string
	PostingIdentity         string
	HeadSHA                 string
	AgentDefinitionsChanged bool
	IncludeNits             bool
	MaxInlineComments       int

	// RunSummary is the execution metadata rendered in the rollup footer.
	RunSummary RunSummary
	// FindingReviewers maps each finding to the reviewer agent ID that
	// produced it; findings absent from the map group as unattributed.
	FindingReviewers map[review.FindingID]string

	Now         func() time.Time
	NewActionID ActionIDGenerator
}

// ThreadResponseRequest is the pure input to BuildThreadResponses.
type ThreadResponseRequest struct {
	PostMode     PostMode
	ProviderCaps ProviderCaps
	Responses    []review.ThreadResponseAction

	Now         func() time.Time
	NewActionID ActionIDGenerator
}

// Diff is the planner-owned diff metadata needed for anchoring.
type Diff struct {
	Files []DiffFile
}

// DiffFile describes one changed file in the PR diff.
type DiffFile struct {
	OldPath string
	Path    string
	Binary  bool
	Deleted bool
	Hunks   []DiffHunk
}

// DiffHunk describes line ranges and fallback target metadata for one hunk.
type DiffHunk struct {
	OldStart     int
	OldEnd       int
	NewStart     int
	NewEnd       int
	FallbackSide review.DiffSide
	FallbackLine int
	DiffPosition int
}

// Plan is the complete pure action plan.
type Plan struct {
	Outcome          Outcome
	RollupMarkdown   string
	Actions          []Action
	AnchoredFindings []AnchoredFinding
	// Summary is the derived rollup metadata the rendered markdown was
	// built from; dry-run JSON exposes the same object.
	Summary Summary
}

// Action is one planner-local planned action.
type Action struct {
	ActionID  string
	Kind      ActionKind
	FindingID review.FindingID
	ThreadID  string
	PlannedAt time.Time
	Status    ActionStatus
	Required  bool
	Marker    MarkerPlacement

	InlineComment *InlineCommentPayload
	ThreadReply   *ThreadReplyPayload
	ResolveThread *ResolveThreadPayload
	RollupComment *RollupCommentPayload
	SubmitReview  *SubmitReviewPayload
}

// MarkerPlacement records where the post phase should place markers.
type MarkerPlacement struct {
	BodyBearing   bool
	Skip          bool
	ThreadSummary bool
	ActionKind    ActionKind
	Outcome       Outcome
}

// AnchoredFinding records durable finding anchoring after normalization.
type AnchoredFinding struct {
	FindingID    review.FindingID
	Severity     review.Severity
	FilePath     string
	Anchoring    review.Anchoring
	Side         *review.DiffSide
	Line         *int
	DiffPosition *int
	Body         string
}

// InlineCommentPayload mirrors the provider-neutral inline comment payload.
type InlineCommentPayload struct {
	Body         string
	Path         string
	Side         review.DiffSide
	Line         int
	SubjectType  review.AnchorKind
	DiffPosition int
}

// ThreadReplyPayload mirrors the thread reply payload.
type ThreadReplyPayload struct {
	Body    string
	Summary bool
}

// ResolveThreadPayload mirrors the resolve-thread payload.
type ResolveThreadPayload struct{}

// RollupCommentPayload mirrors the rollup comment payload.
type RollupCommentPayload struct {
	Body string
}

// SubmitReviewPayload mirrors the submit-review payload.
type SubmitReviewPayload struct {
	Body  string
	Event review.ReviewEvent
}

type builder struct {
	req          Request
	status       ActionStatus
	now          time.Time
	usedIDs      map[string]bool
	inlineCount  int
	maxInline    int
	findingsByID map[review.FindingID]review.Finding
	anchoredByID map[review.FindingID]AnchoredFinding
}

// Build turns validated review-domain values into a deterministic action plan.
func Build(req Request) (Plan, error) {
	b, err := newBuilder(req)
	if err != nil {
		return Plan{}, err
	}
	if req.NoDiff {
		return b.buildNoDiff()
	}
	return b.buildReview()
}

// BuildThreadResponses turns thread response-domain values into a response-only
// action plan. It never creates rollup comments or submit-review actions.
func BuildThreadResponses(req ThreadResponseRequest) (Plan, error) {
	b, err := newBuilder(Request{
		PostMode:     req.PostMode,
		ProviderCaps: req.ProviderCaps,
		Now:          req.Now,
		NewActionID:  req.NewActionID,
	})
	if err != nil {
		return Plan{}, err
	}
	actions, err := b.threadResponseActions(req.Responses)
	if err != nil {
		return Plan{}, err
	}
	outcome := OutcomeNothingToReview
	if len(actions) > 0 {
		outcome = OutcomeComment
	}
	return Plan{
		Outcome: outcome,
		Actions: actions,
	}, nil
}

// OutcomeFromReviewEvent maps a review-domain event into a planner outcome.
func OutcomeFromReviewEvent(event review.ReviewEvent) (Outcome, error) {
	switch event {
	case review.ReviewEventApprove:
		return OutcomeApproved, nil
	case review.ReviewEventComment:
		return OutcomeComment, nil
	case review.ReviewEventRequestChanges:
		return OutcomeRequestChanges, nil
	default:
		return "", fmt.Errorf("reviewplan: invalid review event %q", event)
	}
}

// ReviewEventForFindings implements the default event mapping from severity.
func ReviewEventForFindings(findings []review.Finding, opts EventOptions) review.ReviewEvent {
	var blocking, major bool
	for _, finding := range findings {
		switch finding.Severity {
		case review.SeverityBlocking:
			blocking = true
		case review.SeverityMajor:
			major = true
		case review.SeverityMinor, review.SeverityNits:
		}
	}
	switch {
	case blocking:
		return review.ReviewEventRequestChanges
	case major && opts.MajorEventRequestsChanges:
		return review.ReviewEventRequestChanges
	case major:
		return review.ReviewEventComment
	case opts.PostingIdentityIsPRAuthor && !opts.AllowSelfApprove:
		return review.ReviewEventComment
	default:
		return review.ReviewEventApprove
	}
}

// effectiveReviewEvent resolves the posted review event. It begins with the
// rollup model's chosen event (or the severity-derived default when the model
// did not specify one), then clamps it so a review whose findings are all
// minor/nit — nothing blocking or major — always approves. Non-blocking,
// non-major findings are suggestions and must never gate approval, regardless
// of what the rollup model proposed. Blocking/major findings still govern as
// before (the model's choice, or the severity default, stands).
func effectiveReviewEvent(rollupEvent review.ReviewEvent, findings []review.Finding, opts EventOptions) review.ReviewEvent {
	severityEvent := ReviewEventForFindings(findings, opts)
	if severityEvent == review.ReviewEventApprove {
		return review.ReviewEventApprove
	}
	if rollupEvent == "" {
		return severityEvent
	}
	return rollupEvent
}

func applySelfApprovalPolicy(event review.ReviewEvent, opts EventOptions) review.ReviewEvent {
	if event == review.ReviewEventApprove && opts.PostingIdentityIsPRAuthor && !opts.AllowSelfApprove {
		return review.ReviewEventComment
	}
	return event
}

func hasIncompleteReviewerCoverage(coverage []ReviewerCoverageSummary) bool {
	for _, entry := range coverage {
		switch entry.Status {
		case "", "complete_broad", "complete_constrained":
			continue
		case "incomplete_skipped", "incomplete_failed", "incomplete_unassigned":
			return true
		default:
			// Unknown coverage statuses are treated conservatively so a new
			// incomplete state cannot silently bypass approval coercion.
			return true
		}
	}
	return false
}

func newBuilder(req Request) (*builder, error) {
	if !req.PostMode.Valid() {
		return nil, fmt.Errorf("reviewplan: invalid post mode %q", req.PostMode)
	}
	if req.NewActionID == nil {
		return nil, errors.New("reviewplan: action ID generator is required")
	}
	status := ActionStatusPending
	if req.PostMode == PostModeDryRun {
		status = ActionStatusPlannedOnly
	}
	now := time.Now().UTC()
	if req.Now != nil {
		now = req.Now().UTC()
	}
	maxInline := req.MaxInlineComments
	if maxInline <= 0 {
		maxInline = defaultMaxInlineComments
	}
	findingsByID := make(map[review.FindingID]review.Finding, len(req.Findings))
	for _, finding := range req.Findings {
		if err := finding.Validate(); err != nil {
			return nil, fmt.Errorf("reviewplan: invalid finding %q: %w", finding.ID, err)
		}
		if !finding.ID.Assigned() {
			return nil, errors.New("reviewplan: finding ID is required")
		}
		if findingsByID[finding.ID].ID.Assigned() {
			return nil, fmt.Errorf("reviewplan: duplicate finding ID %q", finding.ID)
		}
		findingsByID[finding.ID] = finding
	}
	return &builder{
		req:          req,
		status:       status,
		now:          now,
		usedIDs:      map[string]bool{},
		maxInline:    maxInline,
		findingsByID: findingsByID,
		anchoredByID: map[review.FindingID]AnchoredFinding{},
	}, nil
}

func (b *builder) buildNoDiff() (Plan, error) {
	if len(b.req.Findings) > 0 {
		return Plan{}, errors.New("reviewplan: no-diff plan cannot contain findings")
	}
	body := b.renderNoDiffRollup()
	rollup, err := b.newAction(ActionKindRollupComment)
	if err != nil {
		return Plan{}, err
	}
	rollup.Required = true
	rollup.Marker = actionMarker(ActionKindRollupComment, OutcomeNothingToReview)
	rollup.RollupComment = &RollupCommentPayload{Body: body}
	return Plan{
		Outcome:        OutcomeNothingToReview,
		RollupMarkdown: body,
		Actions:        []Action{rollup},
	}, nil
}

func (b *builder) buildReview() (Plan, error) {
	ordered, err := b.orderedFindings()
	if err != nil {
		return Plan{}, err
	}
	event := effectiveReviewEvent(b.req.Rollup.ReviewEvent, ordered, b.req.EventOptions)
	if !event.Valid() {
		return Plan{}, fmt.Errorf("reviewplan: invalid rollup review event %q", event)
	}
	event = applySelfApprovalPolicy(event, b.req.EventOptions)
	if len(b.req.RunSummary.ReviewerFailures) > 0 && event == review.ReviewEventApprove {
		event = review.ReviewEventComment
	}
	if hasIncompleteReviewerCoverage(b.req.RunSummary.ReviewerCoverage) && event == review.ReviewEventApprove {
		event = review.ReviewEventComment
	}
	outcome, err := OutcomeFromReviewEvent(event)
	if err != nil {
		return Plan{}, err
	}

	b.populateAnchoredFindings()

	var actions []Action
	threadReplies, resolves, err := b.threadActions()
	if err != nil {
		return Plan{}, err
	}
	actions = append(actions, threadReplies...)
	actions = append(actions, resolves...)
	responseActions, err := b.threadResponseActions(b.req.ThreadResponses)
	if err != nil {
		return Plan{}, err
	}
	actions = append(actions, responseActions...)

	commentActions, err := b.commentActions(ordered)
	if err != nil {
		return Plan{}, err
	}
	actions = append(actions, commentActions...)

	anchored := b.anchoredForOrdered(ordered)
	summary := b.deriveSummary(b.renderedFindings(ordered))
	rollupBody := b.renderRollup(ordered, anchored, summary)
	rollup, err := b.newAction(ActionKindRollupComment)
	if err != nil {
		return Plan{}, err
	}
	rollup.Required = true
	rollup.Marker = actionMarker(ActionKindRollupComment, outcome)
	rollup.RollupComment = &RollupCommentPayload{Body: rollupBody}
	actions = append(actions, rollup)

	submit, err := b.newAction(ActionKindSubmitReview)
	if err != nil {
		return Plan{}, err
	}
	submit.Required = true
	submit.Marker = actionMarker(ActionKindSubmitReview, "")
	submit.SubmitReview = &SubmitReviewPayload{
		Body:  submitReviewBody(outcome),
		Event: event,
	}
	actions = append(actions, submit)

	return Plan{
		Outcome:          outcome,
		RollupMarkdown:   rollupBody,
		Actions:          actions,
		AnchoredFindings: b.anchoredInInputOrder(),
		Summary:          summary,
	}, nil
}

// renderedFindings filters ordered findings to those the rollup renders.
func (b *builder) renderedFindings(ordered []review.Finding) []review.Finding {
	if b.req.IncludeNits {
		return ordered
	}
	rendered := make([]review.Finding, 0, len(ordered))
	for _, finding := range ordered {
		if finding.Severity == review.SeverityNits {
			continue
		}
		rendered = append(rendered, finding)
	}
	return rendered
}

func (b *builder) orderedFindings() ([]review.Finding, error) {
	if len(b.req.Rollup.OrderedFindings) == 0 {
		if len(b.req.Rollup.DedupeLog) > 0 && len(b.req.Findings) > 0 {
			return nil, errors.New("reviewplan: ordered findings are required when dedupe log is present")
		}
		return append([]review.Finding(nil), b.req.Findings...), nil
	}
	dropped, err := b.droppedFindings()
	if err != nil {
		return nil, err
	}
	ordered := make([]review.Finding, 0, len(b.req.Rollup.OrderedFindings))
	seen := map[review.FindingID]bool{}
	for _, id := range b.req.Rollup.OrderedFindings {
		if dropped[id] {
			return nil, fmt.Errorf("reviewplan: dropped finding %q cannot be ordered", id)
		}
		finding, ok := b.findingsByID[id]
		if !ok {
			return nil, fmt.Errorf("reviewplan: ordered finding %q is unknown", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("reviewplan: ordered finding %q appears more than once", id)
		}
		seen[id] = true
		ordered = append(ordered, finding)
	}
	for _, entry := range b.req.Rollup.DedupeLog {
		if !seen[entry.Kept] {
			return nil, fmt.Errorf("reviewplan: dedupe kept finding %q is not ordered", entry.Kept)
		}
	}
	for _, finding := range b.req.Findings {
		if seen[finding.ID] || dropped[finding.ID] {
			continue
		}
		return nil, fmt.Errorf("reviewplan: finding %q is neither ordered nor dropped", finding.ID)
	}
	return ordered, nil
}

func (b *builder) droppedFindings() (map[review.FindingID]bool, error) {
	dropped := map[review.FindingID]bool{}
	for _, entry := range b.req.Rollup.DedupeLog {
		if _, ok := b.findingsByID[entry.Kept]; !ok {
			return nil, fmt.Errorf("reviewplan: dedupe kept finding %q is unknown", entry.Kept)
		}
		for _, id := range entry.Dropped {
			if _, ok := b.findingsByID[id]; !ok {
				return nil, fmt.Errorf("reviewplan: dedupe dropped finding %q is unknown", id)
			}
			if id == entry.Kept {
				return nil, fmt.Errorf("reviewplan: dedupe kept finding %q cannot be dropped", id)
			}
			if dropped[id] {
				return nil, fmt.Errorf("reviewplan: dedupe dropped finding %q appears more than once", id)
			}
			dropped[id] = true
		}
	}
	return dropped, nil
}

func (b *builder) populateAnchoredFindings() {
	for _, finding := range b.req.Findings {
		anchored := b.anchorFinding(finding)
		b.anchoredByID[finding.ID] = anchored
	}
}

func (b *builder) anchorFinding(finding review.Finding) AnchoredFinding {
	body := sanitize(finding.Body)
	anchored := AnchoredFinding{
		FindingID: finding.ID,
		Severity:  finding.Severity,
		FilePath:  finding.FilePath,
		Anchoring: review.AnchoringRollupOnly,
		Body:      body,
	}
	diffFile, ok := b.findDiffFile(finding.FilePath)
	if !ok {
		return anchored
	}
	anchored.FilePath = diffFile.Path
	if diffFile.Binary || diffFile.Deleted {
		return anchored
	}
	if finding.Anchor.Kind == review.AnchorKindLine && lineInHunk(diffFile, finding.Anchor.Side, finding.Anchor.Line) {
		anchored.Anchoring = review.AnchoringInline
		anchored.Side = sidePtr(finding.Anchor.Side)
		anchored.Line = intPtr(finding.Anchor.Line)
		return anchored
	}
	if b.req.ProviderCaps.NativeFileLevelComments {
		anchored.Anchoring = review.AnchoringFileLevelNative
		return anchored
	}
	hunk, ok := firstFallbackHunk(diffFile)
	if !ok {
		return anchored
	}
	anchored.Anchoring = review.AnchoringFileLevelFallback
	anchored.Side = sidePtr(hunk.FallbackSide)
	anchored.Line = intPtr(hunk.FallbackLine)
	anchored.DiffPosition = intPtr(hunk.DiffPosition)
	return anchored
}

func (b *builder) findDiffFile(path string) (DiffFile, bool) {
	for _, file := range b.req.Diff.Files {
		if file.Path == path || file.OldPath == path {
			if file.Path == "" {
				file.Path = path
			}
			return file, true
		}
	}
	return DiffFile{}, false
}

func (b *builder) threadActions() ([]Action, []Action, error) {
	var replies []Action
	var resolves []Action
	for _, thread := range b.req.ThreadActions {
		switch thread.Decision {
		case review.ThreadDecisionSkip:
			continue
		case review.ThreadDecisionSummarizeOnly, review.ThreadDecisionSummarizeAndResolve:
		default:
			return nil, nil, fmt.Errorf("reviewplan: invalid thread decision %q", thread.Decision)
		}
		if strings.TrimSpace(thread.ThreadID) == "" {
			return nil, nil, errors.New("reviewplan: thread action ID is required")
		}
		if strings.TrimSpace(thread.Summary) == "" {
			return nil, nil, fmt.Errorf("reviewplan: thread %q summary is required", thread.ThreadID)
		}
		reply, err := b.newAction(ActionKindThreadReply)
		if err != nil {
			return nil, nil, err
		}
		reply.ThreadID = thread.ThreadID
		reply.Marker = MarkerPlacement{BodyBearing: true, ThreadSummary: true}
		reply.ThreadReply = &ThreadReplyPayload{Body: sanitize(thread.Summary), Summary: true}
		replies = append(replies, reply)
		if thread.Decision != review.ThreadDecisionSummarizeAndResolve || !b.req.ProviderCaps.ThreadResolution {
			continue
		}
		resolve, err := b.newAction(ActionKindResolveThread)
		if err != nil {
			return nil, nil, err
		}
		resolve.ThreadID = thread.ThreadID
		resolve.ResolveThread = &ResolveThreadPayload{}
		resolves = append(resolves, resolve)
	}
	return replies, resolves, nil
}

func (b *builder) threadResponseActions(responses []review.ThreadResponseAction) ([]Action, error) {
	var replies []Action
	var resolves []Action
	for _, response := range responses {
		if err := response.Validate(); err != nil {
			return nil, fmt.Errorf("reviewplan: invalid thread response: %w", err)
		}
		reply, err := b.newAction(ActionKindThreadReply)
		if err != nil {
			return nil, err
		}
		reply.Required = true
		reply.ThreadID = response.ThreadID
		reply.Marker = MarkerPlacement{
			BodyBearing:   true,
			ThreadSummary: response.Kind == review.ThreadResponseSummaryReply,
		}
		reply.ThreadReply = &ThreadReplyPayload{
			Body:    sanitize(response.Body),
			Summary: response.Kind == review.ThreadResponseSummaryReply,
		}
		replies = append(replies, reply)
		if !response.Resolve || !b.req.ProviderCaps.ThreadResolution {
			continue
		}
		resolve, err := b.newAction(ActionKindResolveThread)
		if err != nil {
			return nil, err
		}
		resolve.Required = true
		resolve.ThreadID = response.ThreadID
		resolve.ResolveThread = &ResolveThreadPayload{}
		resolves = append(resolves, resolve)
	}
	actions := make([]Action, 0, len(replies)+len(resolves))
	actions = append(actions, replies...)
	actions = append(actions, resolves...)
	return actions, nil
}

func (b *builder) commentActions(ordered []review.Finding) ([]Action, error) {
	var actions []Action
	for _, finding := range ordered {
		anchored := b.anchoredByID[finding.ID]
		if anchored.Anchoring == review.AnchoringRollupOnly {
			continue
		}
		if b.inlineCount >= b.maxInline {
			anchored.Anchoring = review.AnchoringRollupOnly
			anchored.Side = nil
			anchored.Line = nil
			anchored.DiffPosition = nil
			b.anchoredByID[finding.ID] = anchored
			continue
		}
		action, err := b.newAction(ActionKindInlineComment)
		if err != nil {
			return nil, err
		}
		action.FindingID = finding.ID
		action.Marker = actionMarker(ActionKindInlineComment, "")
		action.InlineComment = inlinePayload(anchored)
		actions = append(actions, action)
		b.inlineCount++
	}
	return actions, nil
}

func inlinePayload(anchored AnchoredFinding) *InlineCommentPayload {
	payload := &InlineCommentPayload{
		Body:         commentBody(anchored),
		Path:         anchored.FilePath,
		SubjectType:  review.AnchorKindFile,
		DiffPosition: 0,
	}
	switch anchored.Anchoring {
	case review.AnchoringInline, review.AnchoringFileLevelFallback:
		payload.SubjectType = review.AnchorKindLine
		if anchored.Side != nil {
			payload.Side = *anchored.Side
		}
		if anchored.Line != nil {
			payload.Line = *anchored.Line
		}
		if anchored.DiffPosition != nil {
			payload.DiffPosition = *anchored.DiffPosition
		}
	case review.AnchoringFileLevelNative, review.AnchoringRollupOnly:
	}
	return payload
}

func commentBody(anchored AnchoredFinding) string {
	body := strings.TrimSpace(anchored.Body)
	if anchored.Anchoring == review.AnchoringFileLevelFallback {
		body = fileLevelFallbackPrefix + anchored.FilePath + "\n\n" + body
	}
	return body + "\n\n" + inlineFooter
}

func (b *builder) newAction(kind ActionKind) (Action, error) {
	id, err := b.req.NewActionID(kind)
	if err != nil {
		return Action{}, fmt.Errorf("reviewplan: allocate %s action ID: %w", kind, err)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Action{}, fmt.Errorf("reviewplan: %s action ID is empty", kind)
	}
	if b.usedIDs[id] {
		return Action{}, fmt.Errorf("reviewplan: duplicate action ID %q", id)
	}
	b.usedIDs[id] = true
	return Action{
		ActionID:  id,
		Kind:      kind,
		PlannedAt: b.now,
		Status:    b.status,
	}, nil
}

func actionMarker(kind ActionKind, outcome Outcome) MarkerPlacement {
	return MarkerPlacement{
		BodyBearing: true,
		Skip:        true,
		ActionKind:  kind,
		Outcome:     outcome,
	}
}

func lineInHunk(file DiffFile, side review.DiffSide, line int) bool {
	if line <= 0 || !side.Valid() {
		return false
	}
	for _, hunk := range file.Hunks {
		switch side {
		case review.DiffSideLeft:
			if hunk.OldStart <= line && line <= hunk.OldEnd {
				return true
			}
		case review.DiffSideRight:
			if hunk.NewStart <= line && line <= hunk.NewEnd {
				return true
			}
		}
	}
	return false
}

func firstFallbackHunk(file DiffFile) (DiffHunk, bool) {
	for _, hunk := range file.Hunks {
		if hunk.FallbackSide.Valid() && hunk.FallbackLine > 0 && hunk.DiffPosition > 0 {
			return hunk, true
		}
	}
	return DiffHunk{}, false
}

func (b *builder) renderRollup(ordered []review.Finding, anchored []AnchoredFinding, summary Summary) string {
	var out strings.Builder
	out.WriteString("## Automated PR Review\n\n")
	writeRunMetadata(&out, b.req)
	out.WriteString("### Summary\n\n")
	if len(summary.Reviewers) > 0 {
		writeReviewerTable(&out, summary.Reviewers)
		b.writeReviewerSections(&out, anchored, summary.Reviewers)
		writeReviewerCoverageDiagnostics(&out, summary.Run.ReviewerCoverage)
		writeReviewerFailureDiagnostics(&out, summary.Run.ReviewerFailures)
	} else {
		counts := severityCounts(ordered)
		out.WriteString("| Severity | Findings |\n")
		out.WriteString("|----------|----------|\n")
		for _, severity := range review.SeverityOrder() {
			if severity == review.SeverityNits && !b.req.IncludeNits {
				continue
			}
			out.WriteString("| ")
			out.WriteString(severity.String())
			out.WriteString(" | ")
			fmt.Fprint(&out, counts[severity])
			out.WriteString(" |\n")
		}
		out.WriteString("\n")
		for _, finding := range anchored {
			if finding.Severity == review.SeverityNits && !b.req.IncludeNits {
				continue
			}
			writeFindingBlock(&out, finding)
		}
	}
	if len(summary.Reviewers) == 0 {
		writeReviewerCoverageDiagnostics(&out, summary.Run.ReviewerCoverage)
		writeReviewerFailureDiagnostics(&out, summary.Run.ReviewerFailures)
	}
	fmt.Fprintf(&out, "*%d PR discussion threads considered. %d summarized; %d resolved.*\n", summary.Threads.Considered, summary.Threads.Summarized, summary.Threads.Resolved)
	if b.req.AgentDefinitionsChanged {
		out.WriteString("\n---\n\n")
		out.WriteString("> Note: This PR modifies reviewer definitions under `.codereview/agents/`. The review was conducted using base-branch versions; changes will affect future reviews after merge.\n")
	}
	writeRunFooter(&out, summary.Run, summary.Totals)
	return strings.TrimSpace(out.String())
}

// writeReviewerSections renders findings grouped per reviewer in summary
// order, each group inside a collapsible details block.
func (b *builder) writeReviewerSections(out *strings.Builder, anchored []AnchoredFinding, reviewers []ReviewerSummary) {
	grouped := map[string][]AnchoredFinding{}
	for _, finding := range anchored {
		if finding.Severity == review.SeverityNits && !b.req.IncludeNits {
			continue
		}
		name := b.req.FindingReviewers[finding.FindingID]
		if name == "" {
			name = UnattributedReviewer
		}
		grouped[name] = append(grouped[name], finding)
	}
	for _, reviewer := range reviewers {
		findings := grouped[reviewer.Name]
		if len(findings) == 0 {
			continue
		}
		label := fmt.Sprintf("%s (%d finding", escapeCell(reviewer.Name), len(findings))
		if len(findings) > 1 {
			label += "s"
		}
		label += ")"
		out.WriteString("<details>\n<summary><strong>")
		out.WriteString(label)
		out.WriteString("</strong></summary>\n\n")
		for _, finding := range findings {
			writeFindingBlock(out, finding)
		}
		out.WriteString("</details>\n\n")
	}
}

func writeFindingBlock(out *strings.Builder, finding AnchoredFinding) {
	out.WriteString("### ")
	out.WriteString(displaySeverity(finding.Severity))
	out.WriteString(" - `")
	out.WriteString(finding.FilePath)
	if finding.Line != nil {
		out.WriteString(":")
		fmt.Fprint(out, *finding.Line)
	}
	out.WriteString("`\n\n")
	out.WriteString("> ")
	out.WriteString(strings.ReplaceAll(finding.Body, "\n", "\n> "))
	out.WriteString("\n\n")
}

func (b *builder) renderNoDiffRollup() string {
	var out strings.Builder
	out.WriteString("## Automated PR Review\n\n")
	writeRunMetadata(&out, b.req)
	out.WriteString("### Summary\n\n")
	out.WriteString("Nothing to review for this diff.")
	return strings.TrimSpace(out.String())
}

func writeRunMetadata(out *strings.Builder, req Request) {
	sha := shortSHA(req.HeadSHA)
	if sha != "" {
		out.WriteString("**Reviewed commit:** `")
		out.WriteString(sha)
		out.WriteString("`\n")
	}
	if strings.TrimSpace(req.Profile) != "" || strings.TrimSpace(req.PostingIdentity) != "" {
		out.WriteString("**Profile:** `")
		out.WriteString(sanitize(req.Profile))
		out.WriteString("` - **Posting as:** `")
		out.WriteString(sanitize(req.PostingIdentity))
		out.WriteString("`\n")
	}
	out.WriteString("\n")
}

func submitReviewBody(outcome Outcome) string {
	return "Automated PR review completed with outcome: " + string(outcome) + "."
}

func severityCounts(findings []review.Finding) map[review.Severity]int {
	counts := map[review.Severity]int{}
	for _, finding := range findings {
		counts[finding.Severity]++
	}
	return counts
}

func threadSummaryCounts(actions []review.ThreadAction, caps ProviderCaps) ThreadCounts {
	var counts ThreadCounts
	for _, action := range actions {
		if action.Decision == review.ThreadDecisionSkip {
			continue
		}
		counts.Considered++
		counts.Summarized++
		if action.Decision == review.ThreadDecisionSummarizeAndResolve && caps.ThreadResolution {
			counts.Resolved++
		}
	}
	return counts
}

func threadResponseSummaryCounts(responses []review.ThreadResponseAction, caps ProviderCaps) ThreadCounts {
	var counts ThreadCounts
	for _, response := range responses {
		if strings.TrimSpace(response.ThreadID) == "" {
			continue
		}
		counts.Considered++
		if response.Kind == review.ThreadResponseSummaryReply {
			counts.Summarized++
		}
		if response.Resolve && caps.ThreadResolution {
			counts.Resolved++
		}
	}
	return counts
}

func (b *builder) anchoredInInputOrder() []AnchoredFinding {
	anchored := make([]AnchoredFinding, 0, len(b.req.Findings))
	for _, finding := range b.req.Findings {
		anchored = append(anchored, b.anchoredByID[finding.ID])
	}
	return anchored
}

func (b *builder) anchoredForOrdered(ordered []review.Finding) []AnchoredFinding {
	anchored := make([]AnchoredFinding, 0, len(ordered))
	for _, finding := range ordered {
		anchored = append(anchored, b.anchoredByID[finding.ID])
	}
	return anchored
}

func displaySeverity(severity review.Severity) string {
	value := severity.String()
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

func sanitize(text string) string {
	return strings.ReplaceAll(text, markerPrefix, escapedMarkerPrefix)
}

func sidePtr(side review.DiffSide) *review.DiffSide {
	value := side
	return &value
}

func intPtr(value int) *int {
	return &value
}

// SortActions returns actions in durable outbox order.
func SortActions(actions []Action) []Action {
	sorted := append([]Action(nil), actions...)
	sort.SliceStable(sorted, func(i, j int) bool {
		io, jo := actionOrder(sorted[i].Kind), actionOrder(sorted[j].Kind)
		if io != jo {
			return io < jo
		}
		return sorted[i].ActionID < sorted[j].ActionID
	})
	return sorted
}

func actionOrder(kind ActionKind) int {
	switch kind {
	case ActionKindThreadReply:
		return 0
	case ActionKindResolveThread:
		return 1
	case ActionKindInlineComment:
		return 2
	case ActionKindRollupComment:
		return 3
	case ActionKindSubmitReview:
		return 4
	default:
		return 99
	}
}
