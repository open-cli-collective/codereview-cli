package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/datalifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmadapters"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/marker"
	"github.com/open-cli-collective/codereview-cli/internal/reporoot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/runlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/stagemodel"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func dryRunForTest(ctx context.Context, opts Options, req Request) (Result, error) {
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	if opts.Adapter != nil {
		opts.Adapter = repoGuidanceFixtureAdapter{Adapter: opts.Adapter}
	}
	return DryRun(ctx, opts, req)
}

func selectionOnlyForTest(ctx context.Context, opts Options, req SelectionRequest) (SelectionResult, error) {
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	return SelectionOnly(ctx, opts, req)
}

func liveForTest(ctx context.Context, opts Options, req Request, run ledger.Run) (Result, error) {
	configureWorkbenchFixtureForTest(ctx, &opts, req.PRRef)
	if opts.Adapter != nil {
		opts.Adapter = repoGuidanceFixtureAdapter{Adapter: opts.Adapter}
	}
	return Live(ctx, opts, req, run)
}

type repoGuidanceFixtureAdapter struct {
	llm.Adapter
}

func (a repoGuidanceFixtureAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	if capable, ok := a.Adapter.(interface {
		ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode
	}); ok {
		return capable.ReviewerWorkspaceMode()
	}
	return llm.ReviewerWorkspaceNone
}

func (a repoGuidanceFixtureAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if req.ReviewerWorkspace != nil && strings.Contains(req.Prompt, `"id": "repo:guidance"`) {
		return staticStream{
			sessionID: "repo-reviewer-session",
			output:    coverageOnlyJSON("repo:guidance", []string{"main.go"}, nil),
		}, nil
	}
	return a.Adapter.Start(ctx, req)
}

func TestResolveInvocationRootForSafetyTreatsUnavailableAsUnknownAndOtherErrorsAsFatal(t *testing.T) {
	root, err := resolveInvocationRootForSafety(context.Background(), Options{
		ResolveRepoRoot: func(context.Context) (string, error) {
			return "", reporoot.ErrUnavailable
		},
	})
	if err != nil || root != "" {
		t.Fatalf("unavailable root = (%q, %v), want empty root and nil error", root, err)
	}

	wantErr := errors.New("resolver failed")
	root, err = resolveInvocationRootForSafety(context.Background(), Options{
		ResolveRepoRoot: func(context.Context) (string, error) {
			return "", wantErr
		},
	})
	if !errors.Is(err, wantErr) || root != "" {
		t.Fatalf("resolver error = (%q, %v), want empty root and %v", root, err, wantErr)
	}
}

func TestValidateRepositoryBinding(t *testing.T) {
	ref := gitprovider.PRRef{Host: "github.com", Owner: "acme", Repo: "widgets", Number: 42}
	valid := gitprovider.PR{
		Ref:  ref,
		Base: gitprovider.PRBranchRef{Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo},
		Head: gitprovider.PRBranchRef{Host: ref.Host, Owner: "contributor", Repo: ref.Repo},
	}
	tests := []struct {
		name string
		host string
		pr   gitprovider.PR
	}{
		{name: "valid same-host fork", host: ref.Host, pr: valid},
		{name: "configured host mismatch", host: "git.example.com", pr: valid},
		{name: "fetched identity mismatch", host: ref.Host, pr: func() gitprovider.PR { pr := valid; pr.Ref.Number++; return pr }()},
		{name: "base identity mismatch", host: ref.Host, pr: func() gitprovider.PR { pr := valid; pr.Base.Repo = "other"; return pr }()},
		{name: "cross-host head", host: ref.Host, pr: func() gitprovider.PR { pr := valid; pr.Head.Host = "git.example.com"; return pr }()},
		{name: "mixed-case identity", host: "GitHub.com", pr: func() gitprovider.PR {
			pr := valid
			pr.Ref.Owner, pr.Ref.Repo = "ACME", "Widgets"
			pr.Base.Owner, pr.Base.Repo = "ACME", "Widgets"
			return pr
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRepositoryBinding(ref, tt.host, tt.pr)
			valid := tt.name == "valid same-host fork" || tt.name == "mixed-case identity"
			if valid && err != nil {
				t.Fatalf("validateRepositoryBinding: %v", err)
			}
			if !valid && err == nil {
				t.Fatal("validateRepositoryBinding succeeded, want rejection")
			}
		})
	}
}

func TestSelectionOnlyRejectsRepositoryMismatchBeforeGit(t *testing.T) {
	provider, req := dryRunHarness(t)
	req.Profile.Git.Host = "git.example.com"
	gitCalls := 0
	artifactDir := t.TempDir()
	_, err := selectionOnlyForTest(context.Background(), Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		GitCommand: func(context.Context, string, ...string) ([]byte, error) {
			gitCalls++
			return nil, errors.New("unexpected git call")
		},
	}, selectionRequestFromReview(req, artifactDir))
	if err == nil || !strings.Contains(err.Error(), "configured git host") {
		t.Fatalf("SelectionOnly error = %v, want configured host mismatch", err)
	}
	if gitCalls != 0 {
		t.Fatalf("Git calls = %d, want zero before trust binding", gitCalls)
	}
	entries, readErr := os.ReadDir(artifactDir)
	if readErr != nil {
		t.Fatalf("ReadDir artifacts: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("artifacts = %#v, want no writes before trust binding", entries)
	}
}

func TestLiveClassifiesRepositoryBindingFailureAsTerminalBeforeGit(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	run := allocateLiveRun(t, store, provider, req, "terminal-binding")
	req.Profile.Git.Host = "git.example.com"
	gitCalls := 0
	_, err := Live(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		ResolveRepoRoot: func(context.Context) (string, error) { return "", reporoot.ErrUnavailable },
		GitCommand: func(context.Context, string, ...string) ([]byte, error) {
			gitCalls++
			return nil, errors.New("unexpected git call")
		},
	}, req, run)
	if err == nil || ClassifyFailure(err) != FailureTerminal {
		t.Fatalf("Live error = %v kind %v, want terminal repository binding failure", err, ClassifyFailure(err))
	}
	if gitCalls != 0 {
		t.Fatalf("Git calls = %d, want zero before trust binding", gitCalls)
	}
}

func TestLiveClassifiesUnsafeFetchRefAsTerminal(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.pr.Base.Ref = "main"
	run := allocateLiveRun(t, store, provider, req, "terminal-ref")
	_, err := liveForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:    store,
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
	}, req, run)
	if err == nil || ClassifyFailure(err) != FailureTerminal {
		t.Fatalf("Live error = %v kind %v, want terminal unsafe-ref failure", err, ClassifyFailure(err))
	}
}

func TestBuildPlanClassifiesActionIDFailureTerminalAcrossPaths(t *testing.T) {
	wantErr := errors.New("action ID unavailable")
	opts := Options{
		Now: fixedNow,
		NewActionID: func(reviewplan.ActionKind) (string, error) {
			return "", wantErr
		},
	}
	req := Request{
		ProfileName:     "home",
		PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
	}
	pr := gitprovider.PR{Head: gitprovider.PRBranchRef{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	tests := []struct {
		name    string
		noDiff  bool
		rollup  review.Rollup
		sources []agents.SourceInfo
	}{
		{name: "no diff", noDiff: true},
		{name: "repo guidance unavailable", sources: []agents.SourceInfo{{Kind: agents.SourceRepo, Status: agents.SourceStatusMissing}}},
		{name: "normal review", rollup: review.Rollup{ReviewEvent: review.ReviewEventComment}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := opts.buildPlan(req, pr, reviewplan.PostModeLive, reviewplan.ProviderCaps{}, reviewplan.Diff{}, nil, tt.rollup, nil, tt.noDiff, false, planRunInputs{repoSources: tt.sources})
			if !errors.Is(err, wantErr) || ClassifyFailure(err) != FailureTerminal {
				t.Fatalf("buildPlan error = %v kind %v, want terminal action-ID failure", err, ClassifyFailure(err))
			}
		})
	}
}

func TestReviewPipelineAcceptanceHarnessDryRunWithFakes(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.pr.Body = "Document the checkout-native review contract."
	provider.threads = []gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    false,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
		Comments: []gitprovider.ThreadComment{{
			ID:     "comment-1",
			Body:   "Inline concern",
			Author: gitprovider.Identity{Login: "reviewer"},
		}},
	}}
	provider.issueComments = []gitprovider.IssueComment{{
		ID:     "issue-1",
		Body:   "Top-level concern",
		Author: gitprovider.Identity{Login: "maintainer"},
	}}
	provider.reviews = []gitprovider.Review{{
		ID:     "review-1",
		Body:   "Review body",
		Author: gitprovider.Identity{Login: "architect"},
		Event:  review.ReviewEventComment,
	}, {
		ID:     "review-2",
		Body:   "Approved body should stay out of reviewer-facing discussion",
		Author: gitprovider.Identity{Login: "approver"},
		Event:  review.ReviewEventApprove,
	}}
	baseAdapter := &llm.FakeAdapter{
		NameValue:      "fake-llm",
		QuotaValue:     llm.Quota{BlockRemainingPct: 87, WeeklyRemainingPct: 64},
		QuotaSupported: true,
	}
	baseAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON([]string{"Top-level concern", "Review body"}, []threadSummary{{path: "main.go", line: 2, status: "unresolved", summary: "Inline concern"}}), 8, 2))
	baseAdapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	baseAdapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	baseAdapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	adapter := newPromptValidatingAdapter(baseAdapter,
		promptValidation{
			stage: "dossier summary",
			wants: []string{
				"Top-level concern",
				"Inline concern",
				"Review body",
			},
		},
		promptValidation{
			stage: "selection",
			wants: []string{
				"Document the checkout-native review contract.",
				"Top-level concern",
				"Inline concern",
				"Review body",
				`"workbench"`,
				provider.pr.Head.SHA,
				`"id": "harness:reviewer"`,
			},
		},
		promptValidation{
			stage:            "reviewer",
			requireWorkspace: true,
			wants: []string{
				"Document the checkout-native review contract.",
				"Review carefully.",
				`"id": "harness:reviewer"`,
				"go file changed",
				"Guidance provenance: repo@refs/heads/main:",
				"main.go",
			},
		},
		promptValidation{
			stage: "rollup",
			wants: []string{
				"finding-1",
				"harness:reviewer",
				"main.go",
				"Fix this",
			},
		},
	)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	oldDryRun := allocatePipelineRun(t, store, layout, "old-dry-run", ledger.PostModeDryRun, fixedNow().Add(-8*24*time.Hour))
	provider.onGetPR = func() {
		if _, err := store.GetRun(ctx, oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("expired dry-run before provider GetPR error = %v, want ErrNotFound", err)
		}
	}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-1" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	adapter.AssertConsumed(t)

	if result.Run.RunID != "run-1" || result.Run.PostMode != ledger.PostModeDryRun {
		t.Fatalf("run = %#v, want dry-run run-1", result.Run)
	}
	if !result.QuotaSupported || result.Quota.BlockRemainingPct != 87 || result.QuotaLow {
		t.Fatalf("quota result = supported %v quota %#v low %v", result.QuotaSupported, result.Quota, result.QuotaLow)
	}
	if len(result.Findings) != 1 || result.Findings[0].ID != "finding-1" {
		t.Fatalf("findings = %#v", result.Findings)
	}
	if len(result.Sessions) != 4 {
		t.Fatalf("sessions len = %d, want selection/two reviewers/rollup", len(result.Sessions))
	}
	requests := adapter.Requests()
	if len(requests) != 4 {
		t.Fatalf("requests len = %d, want dossier-summary/selection/reviewer/rollup", len(requests))
	}
	assertPromptContains(t, requests[0].Prompt, "Top-level concern", "Inline concern", "Review body")
	assertPromptContains(t, requests[1].Prompt, "Document the checkout-native review contract.", `"workbench"`, provider.pr.Head.SHA)
	assertPromptContains(t, requests[2].Prompt, "Document the checkout-native review contract.", `"id": "harness:reviewer"`, "main.go")
	if requests[2].ReviewerWorkspace == nil {
		t.Fatalf("reviewer request = %#v, want prepared reviewer workspace", requests[2])
	}
	assertPromptContains(t, requests[3].Prompt, "finding-1", "harness:reviewer", "main.go")
	for _, request := range requests {
		if request.Model != "claude-sonnet-4-6" || request.Effort != "medium" {
			t.Fatalf("request = model:%q effort:%q, want claude-sonnet-4-6/medium from agent config", request.Model, request.Effort)
		}
		if request.Fast {
			t.Fatalf("request = %#v, want fast omitted by default", request)
		}
	}
	for _, session := range result.Sessions {
		if session.Model != "claude-sonnet-4-6" || session.Effort == nil || *session.Effort != "medium" {
			t.Fatalf("session = model:%q effort:%v, want claude-sonnet-4-6/medium from agent config", session.Model, session.Effort)
		}
	}
	reviewerSession, ok := sessionWithProviderID(result.Sessions, "reviewer-session")
	if !ok {
		t.Fatalf("sessions = %#v, want reviewer provider session", result.Sessions)
	}
	if len(result.PlannedActions) != 2 {
		t.Fatalf("planned actions len = %d, want inline/submit", len(result.PlannedActions))
	}
	for _, action := range result.PlannedActions {
		if action.Status != ledger.PlannedActionPlannedOnly {
			t.Fatalf("action status = %q, want planned_only for %#v", action.Status, action)
		}
		payload, err := action.Payload()
		if err != nil {
			t.Fatalf("dry-run payload: %v", err)
		}
		if strings.Contains(fmt.Sprint(payload), "<!-- codereview:") {
			t.Fatalf("dry-run payload contains real marker: %#v", payload)
		}
	}

	run, err := store.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Outcome == nil || *run.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("stored outcome = %#v, want dry_run", run.Outcome)
	}
	if _, err := store.GetRun(ctx, oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("expired dry-run GetRun error = %v, want ErrNotFound", err)
	}
	storedFindings, err := store.ListFindings(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(storedFindings) != 1 || storedFindings[0].SessionRowID != reviewerSession.SessionRowID {
		t.Fatalf("stored findings = %#v, want reviewer session FK %q", storedFindings, reviewerSession.SessionRowID)
	}
	storedActions, err := store.ListPlannedActions(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	if len(storedActions) != len(result.PlannedActions) {
		t.Fatalf("stored actions len = %d, want %d", len(storedActions), len(result.PlannedActions))
	}

	assertFileContains(t, result.Artifacts.DiffPatch, "diff --git a/main.go b/main.go")
	assertFileContains(t, result.Artifacts.FindingsJSON, `"severity": "major"`)
	assertFileContains(t, result.Artifacts.RollupMarkdown, "Automated PR Review")
	assertFileContains(t, result.Artifacts.WorkbenchMetadataPath(), `"schema_version": 2`)
	assertFileContains(t, result.Artifacts.WorkbenchMetadataPath(), `"repo_path":`)
	assertAgentSourcesArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "pr-intent.md"), "Document the checkout-native review contract.")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "main.go:2")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "Top-level concern")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "Review body")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "repo-guidance.md"), "Guidance provenance: repo@refs/heads/main:")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "repo-guidance.md"), "Guidance source status: available")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "repo-guidance.md"), "PR-head .codereview/agents changes do not affect this listing.")
	assertDossierIndexArtifact(t, result.Artifacts.DossierDir, "final/discussion.md")
	assertFileOmits(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "provider_session_id", "session_row_id", "mergeability", "approval", "CI status", "Approved body should stay out of reviewer-facing discussion")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "raw", "top-level-comments.json"), "Approved body should stay out of reviewer-facing discussion")
	slicePath, err := result.Artifacts.SlicePatch("harness:reviewer", "main.go")
	if err != nil {
		t.Fatalf("SlicePatch: %v", err)
	}
	assertFileContains(t, slicePath, "+var changed = true")
	for _, path := range []string{result.Artifacts.FindingsJSON, result.Artifacts.RollupMarkdown} {
		data, err := os.ReadFile(path) // #nosec G304 -- test reads artifact paths returned by the pipeline under t.TempDir.
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if strings.Contains(string(data), "<!-- codereview:") {
			t.Fatalf("artifact %s contains real marker: %s", path, data)
		}
	}
}

func TestDryRunResumesIncompleteRunAttempt(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	resume := allocateDryRunForProvider(t, store, layout, provider, req, "run-resume", fixedNow().Add(-time.Minute))

	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:    store,
		Layout:   layout,
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called despite resumable dry-run")
			return "unexpected"
		},
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID != resume.RunID || result.Artifacts.Dir != resume.ArtifactPath {
		t.Fatalf("result run = %q artifacts %q, want resumed %q artifacts %q", result.Run.RunID, result.Artifacts.Dir, resume.RunID, resume.ArtifactPath)
	}
	stored, err := store.GetRun(ctx, resume.RunID)
	if err != nil {
		t.Fatalf("GetRun resume: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("resumed run outcome = %v, want dry_run", stored.Outcome)
	}
}

func TestFindIncompleteDryRunMatchesLegacyDisplayNameKey(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.PostingIdentity.DisplayName = "Legacy Reviewer"
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           "legacy-display-name",
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.DisplayName,
		PostMode:        ledger.PostModeDryRun,
		StartedAt:       fixedNow(),
		ArtifactPath:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	if err := runartifact.WriteMarker(run.ArtifactPath, runartifact.KindReview, run.RunID); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	got, ok, err := findIncompleteDryRun(ctx, store, req, provider.pr)
	if err != nil || !ok || got.RunID != run.RunID {
		t.Fatalf("findIncompleteDryRun = (%q, %v, %v), want legacy run", got.RunID, ok, err)
	}
}

func TestDryRunDoesNotResumeThreadResponseRun(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	responseRun := allocateDryRunForProvider(t, store, layout, provider, req, "run-response", fixedNow().Add(-time.Minute))
	removeReviewRunMarkerForTest(t, responseRun.ArtifactPath)
	writeResponseRunMarkerForTest(t, responseRun.ArtifactPath, responseRun.RunID)

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-review" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID != "run-review" {
		t.Fatalf("result run = %q, want fresh review run", result.Run.RunID)
	}
	if result.Artifacts.Dir == responseRun.ArtifactPath {
		t.Fatalf("result artifacts dir = %q, want not response artifact root", result.Artifacts.Dir)
	}
	storedResponse, err := store.GetRun(ctx, responseRun.RunID)
	if err != nil {
		t.Fatalf("GetRun response: %v", err)
	}
	if storedResponse.Outcome != nil {
		t.Fatalf("response run outcome = %#v, want still incomplete", storedResponse.Outcome)
	}
}

func TestDryRunResumesPinnedReviewRunByPinnedSHAs(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	fixture, reviewBaseSHA, reviewHeadSHA := newPinnedReviewFixtureForRef(t, req.PRRef)
	provider.pr = fixture.pr
	addRepoAgentFixture(provider)
	provider.fixtureRepoDir = fixture.repoDir
	provider.diffBetween = gitprovider.UnifiedDiff{}
	req.ReviewBaseSHA = reviewBaseSHA
	req.ReviewHeadSHA = reviewHeadSHA
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	resume := allocateDryRunForSHAs(t, store, layout, req, "run-pinned-resume", reviewHeadSHA, reviewBaseSHA, fixedNow().Add(-time.Minute))

	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:    store,
		Layout:   layout,
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called despite resumable pinned dry-run")
			return "unexpected"
		},
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID != resume.RunID || result.Artifacts.Dir != resume.ArtifactPath {
		t.Fatalf("result run/artifacts = %q %q, want %q %q", result.Run.RunID, result.Artifacts.Dir, resume.RunID, resume.ArtifactPath)
	}
	if len(provider.diffBetweenCalls) != 1 || provider.diffBetweenCalls[0].baseSHA != reviewBaseSHA || provider.diffBetweenCalls[0].headSHA != reviewHeadSHA {
		t.Fatalf("diff between calls = %#v, want pinned base/head", provider.diffBetweenCalls)
	}
}

func TestReviewPipelineAcceptanceHarnessResumesFailedDurableTask(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	run := allocateDryRunForProvider(t, store, layout, provider, req, "run-task-resume", fixedNow().Add(-time.Minute))

	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	firstAdapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	firstAdapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	firstAdapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"missing-finding"}), 30, 6))
	firstAdapter.Queue(fakeLLMResult("rollup-retry-session", rollupJSON("comment", []string{"missing-finding"}), 30, 6))
	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         firstAdapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "unexpected-fresh-run" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err == nil || !errors.Is(err, ErrStructuredOutputInvalidAfterRetry) {
		t.Fatalf("first DryRun error = %v, want invalid rollup after retry", err)
	}
	stored, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun first: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeIncomplete {
		t.Fatalf("first run outcome = %#v, want incomplete", stored.Outcome)
	}
	artifacts := ArtifactPathsFromDir(run.ArtifactPath)
	successMetadata := map[string]llmlifecycle.Metadata{}
	for _, taskID := range []string{orchestratorSelectionStage, reviewerTaskID("harness:reviewer"), reviewerTaskID("repo:guidance")} {
		meta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(artifacts), taskID)
		if err != nil || !ok || meta.Status != llmTaskStatusSucceeded {
			t.Fatalf("task %s metadata = %#v ok %v err %v, want succeeded", taskID, meta, ok, err)
		}
		successMetadata[taskID] = meta
	}
	rollupMeta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(artifacts), orchestratorRollupStage)
	if err != nil || !ok || rollupMeta.Status != llmTaskStatusFailedBlocking {
		t.Fatalf("rollup metadata = %#v ok %v err %v, want failed_blocking", rollupMeta, ok, err)
	}

	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	secondAdapter.Queue(fakeLLMResult("rollup-fixed-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  secondAdapter,
		Store:    store,
		Layout:   layout,
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called despite resumable dry-run")
			return "unexpected"
		},
		NewSessionRowID: sequence("resume-session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("second DryRun: %v", err)
	}
	if result.Run.RunID != run.RunID || result.Artifacts.Dir != run.ArtifactPath {
		t.Fatalf("result run/artifacts = %q %q, want %q %q", result.Run.RunID, result.Artifacts.Dir, run.RunID, run.ArtifactPath)
	}
	if len(secondAdapter.Requests()) != 0 {
		t.Fatalf("second adapter starts = %#v, want cached completed tasks and resumed rollup only", secondAdapter.Requests())
	}
	resumes := secondAdapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "rollup-retry-session" {
		t.Fatalf("second adapter resumes = %#v, want only failed rollup retry session", resumes)
	}
	resumeReq := resumes[0].Request
	if resumeReq.Model != "claude-sonnet-4-6" || resumeReq.Effort != "medium" {
		t.Fatalf("resume request = model:%q effort:%q, want claude-sonnet-4-6/medium", resumeReq.Model, resumeReq.Effort)
	}
	assertPromptContains(t, resumeReq.Prompt, "finding-1", "harness:reviewer", "main.go")
	if len(result.Findings) != 1 || result.Findings[0].ID != "finding-1" {
		t.Fatalf("result findings = %#v, want cached reviewer finding", result.Findings)
	}
	for taskID, want := range successMetadata {
		got, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(artifacts), taskID)
		if err != nil || !ok {
			t.Fatalf("task %s metadata after resume = %#v ok %v err %v, want reused metadata", taskID, got, ok, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("task %s metadata after resume = %#v, want unchanged %#v", taskID, got, want)
		}
	}
	stored, err = store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun second: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("second run outcome = %#v, want dry_run", stored.Outcome)
	}
}

func TestDryRunResumeReusesPersistedPlanningRows(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())

	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	firstAdapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 8, 2))
	firstAdapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	firstAdapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	firstAdapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	failingComplete := &completeFailingStore{Store: store, err: errors.New("complete failed after planning")}
	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         firstAdapter,
		Store:           failingComplete,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-planning-resume" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err == nil || !strings.Contains(err.Error(), "complete failed after planning") {
		t.Fatalf("first DryRun error = %v, want complete failure", err)
	}
	actions, err := store.ListPlannedActions(ctx, "run-planning-resume")
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	if len(actions) == 0 {
		t.Fatal("planned actions len = 0, want persisted planning rows")
	}

	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  secondAdapter,
		Store:    store,
		Layout:   layout,
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called despite resumable post-planning dry-run")
			return "unexpected"
		},
		NewSessionRowID: sequence("resume-session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("second DryRun: %v", err)
	}
	if result.Run.RunID != "run-planning-resume" {
		t.Fatalf("second result run = %#v, want resumed dry-run", result.Run)
	}
	stored, err := store.GetRun(ctx, "run-planning-resume")
	if err != nil {
		t.Fatalf("GetRun second: %v", err)
	}
	if stored.Outcome == nil || *stored.Outcome != ledger.OutcomeDryRun {
		t.Fatalf("stored second run outcome = %#v, want dry_run", stored.Outcome)
	}
	if len(secondAdapter.Requests()) != 0 {
		t.Fatalf("second adapter starts = %#v, want cached LLM tasks", secondAdapter.Requests())
	}
	if len(result.PlannedActions) != len(actions) {
		t.Fatalf("result planned actions len = %d, want persisted %d", len(result.PlannedActions), len(actions))
	}
}

func TestDryRunRerunBypassesIncompleteRunAttempt(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	req.Rerun = true
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	resume := allocateDryRunForProvider(t, store, layout, provider, req, "run-resume", fixedNow().Add(-time.Minute))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-fresh" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID != "run-fresh" {
		t.Fatalf("result run = %q, want fresh run", result.Run.RunID)
	}
	if result.Artifacts.Dir == resume.ArtifactPath {
		t.Fatalf("result artifacts dir = %q, want fresh artifact root", result.Artifacts.Dir)
	}
	storedResume, err := store.GetRun(ctx, resume.RunID)
	if err != nil {
		t.Fatalf("GetRun resume: %v", err)
	}
	if storedResume.Outcome != nil {
		t.Fatalf("bypassed run outcome = %v, want still incomplete", storedResume.Outcome)
	}
}

func TestDryRunIncompleteReviewerCoverageForcesCommentOutcome(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 1, 1))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", coverageOnlyJSON("harness:reviewer", nil, []string{"main.go"}, "could not inspect assigned file"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("approve", nil), 1, 1))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-incomplete-coverage" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Plan.Outcome != reviewplan.OutcomeComment {
		t.Fatalf("outcome = %q, want comment despite approve rollup", result.Plan.Outcome)
	}
	coverage := result.Plan.Summary.Run.ReviewerCoverage
	if len(coverage) != 2 ||
		coverage[0].AgentID != "harness:reviewer" ||
		coverage[0].Status != reviewerCoverageIncompleteSkipped ||
		!reflect.DeepEqual(coverage[0].SkippedFiles, []string{"main.go"}) ||
		coverage[1].AgentID != "repo:guidance" ||
		coverage[1].Status != reviewerCoverageCompleteBroad {
		t.Fatalf("coverage = %#v, want incomplete skipped reviewer coverage", coverage)
	}
	if !strings.Contains(result.Plan.RollupMarkdown, "### Reviewer Coverage") {
		t.Fatalf("rollup markdown missing reviewer coverage:\n%s", result.Plan.RollupMarkdown)
	}
}

func TestDryRunNormalizesReviewerFindingsFileAlias(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsFileAliasJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-file-alias" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(result.Findings) != 1 || result.Findings[0].FilePath != "main.go" {
		t.Fatalf("findings = %#v, want canonical main.go path", result.Findings)
	}
	storedFindings, err := store.ListFindings(ctx, "run-file-alias")
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(storedFindings) != 1 || storedFindings[0].FilePath != "main.go" {
		t.Fatalf("stored findings = %#v, want canonical main.go path", storedFindings)
	}
	data, err := os.ReadFile(result.Artifacts.FindingsJSON) // #nosec G304 -- test reads artifact path returned by the pipeline under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", result.Artifacts.FindingsJSON, err)
	}
	if !strings.Contains(string(data), `"file_path": "main.go"`) {
		t.Fatalf("findings artifact = %s, want canonical file_path", data)
	}
	if strings.Contains(string(data), `"file":`) {
		t.Fatalf("findings artifact leaked file alias: %s", data)
	}
}

func TestDryRunWithPinnedReviewSHAsUsesCompareDiffAndPinnedFileRefs(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	writeAgentFullContent(t, req.Profile.AgentSources[0], "harness", "reviewer")
	fixture, reviewBaseSHA, reviewHeadSHA := newPinnedReviewFixtureForRef(t, req.PRRef)
	provider.pr = fixture.pr
	addRepoAgentFixture(provider)
	provider.fixtureRepoDir = fixture.repoDir
	req.ReviewBaseSHA = reviewBaseSHA
	req.ReviewHeadSHA = reviewHeadSHA
	provider.diffBetween = gitprovider.UnifiedDiff{Raw: smallDiff("main.go")}
	provider.files[fileKey{gitRef: reviewBaseSHA, path: "main.go"}] = []byte("package main\nvar changed = false\n")
	provider.files[fileKey{gitRef: reviewHeadSHA, path: "main.go"}] = []byte("package main\nvar changed = true\n")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSONForAgents("main.go", "harness:reviewer", "repo:guidance"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-pinned" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	if len(provider.diffBetweenCalls) != 1 || provider.diffBetweenCalls[0].baseSHA != reviewBaseSHA || provider.diffBetweenCalls[0].headSHA != reviewHeadSHA {
		t.Fatalf("diff between calls = %#v, want pinned base/head", provider.diffBetweenCalls)
	}
	if result.CurrentBaseSHA != provider.pr.Base.SHA || result.CurrentHeadSHA != provider.pr.Head.SHA ||
		result.ReviewBaseSHA != reviewBaseSHA || result.ReviewHeadSHA != reviewHeadSHA {
		t.Fatalf("result SHAs = current %s/%s review %s/%s", result.CurrentBaseSHA, result.CurrentHeadSHA, result.ReviewBaseSHA, result.ReviewHeadSHA)
	}
	if !strings.Contains(result.Artifacts.Dir, reviewHeadSHA) || !strings.Contains(result.Artifacts.Dir, reviewBaseSHA) {
		t.Fatalf("artifact dir = %s, want pinned head/base SHAs", result.Artifacts.Dir)
	}
	for _, call := range provider.fileCalls {
		if call.path == "main.go" && (call.gitRef == reviewBaseSHA || call.gitRef == reviewHeadSHA) {
			t.Fatalf("file calls = %#v, want no stuffed diff file reads in reviewer workspace mode", provider.fileCalls)
		}
	}
	requests := adapter.Requests()
	if len(requests) < 1 {
		t.Fatalf("adapter requests = %d, want selection request", len(requests))
	}
	selectionPrompt := requests[0].Prompt
	if !strings.Contains(selectionPrompt, reviewBaseSHA) || !strings.Contains(selectionPrompt, reviewHeadSHA) {
		t.Fatalf("selection prompt missing pinned review SHAs: %s", selectionPrompt)
	}
	if strings.Contains(selectionPrompt, provider.pr.Head.SHA) {
		t.Fatalf("selection prompt contains current PR SHAs: %s", selectionPrompt)
	}
	if requests[1].ReviewerWorkspace == nil {
		t.Fatalf("reviewer request = %#v, want reviewer workspace", requests[1])
	}
	workspace := requests[1].ReviewerWorkspace
	if !strings.Contains(workspace.RepoDir, filepath.Join("workbench", "reviewers")) ||
		!strings.HasPrefix(workspace.ScratchDir, result.Artifacts.WorkbenchScratch+string(filepath.Separator)) ||
		workspace.MaxToolOutputBytes != 32*1024 {
		t.Fatalf("reviewer workspace request = %#v, want disposable repo, scratch, and default cap", workspace)
	}
	if provider.threadCalls != 0 {
		t.Fatalf("thread calls = %d, want no live thread reads for pinned review", provider.threadCalls)
	}
	if provider.reviewCalls != 0 || provider.issueCommentCalls != 0 {
		t.Fatalf("review/comment calls = %d/%d, want no live discussion reads for pinned review", provider.reviewCalls, provider.issueCommentCalls)
	}
	if !containsFileCall(provider.treeCalls, fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents"}) {
		t.Fatalf("tree calls = %#v, want repo agents loaded from current base SHA", provider.treeCalls)
	}
	if containsFileCall(provider.treeCalls, fileKey{gitRef: reviewBaseSHA, path: ".codereview/agents"}) {
		t.Fatalf("tree calls = %#v, want no repo agent load from pinned review base SHA", provider.treeCalls)
	}
	assertFileContains(t, result.Artifacts.DiffPatch, "diff --git a/main.go b/main.go")
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "Current PR discussion omitted because this review is pinned to explicit base/head SHAs.")
}

func TestDryRunWithPinnedReviewSHAsRejectsForkHeads(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.ReviewBaseSHA = strings.Repeat("1", 40)
	req.ReviewHeadSHA = strings.Repeat("2", 40)
	provider.pr.Head.Owner = "fork-owner"
	provider.pr.Head.Repo = "codereview-cli-fork"

	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-pinned-fork" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err == nil || !strings.Contains(err.Error(), "does not support fork PR heads") {
		t.Fatalf("DryRun error = %v, want clear fork-head rejection", err)
	}
	if len(provider.diffBetweenCalls) != 0 {
		t.Fatalf("diff between calls = %#v, want rejection before compare", provider.diffBetweenCalls)
	}
}

func TestSelectionOnlyRunsSingleSelectionPhaseWithoutReviewArtifacts(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	provider.threads = []gitprovider.InlineThread{{
		ID:          "thread-1",
		Resolved:    false,
		Path:        "main.go",
		Side:        review.DiffSideRight,
		Line:        2,
		SubjectType: review.AnchorKindLine,
	}}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, []threadSummary{{path: "main.go", line: 2, status: "unresolved", summary: "Open thread at main.go:2"}}), 8, 2))
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	artifactDir := t.TempDir()

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, artifactDir))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}

	expectedArtifacts := ArtifactPathsFromDir(artifactDir)
	if !reflect.DeepEqual(result.Artifacts, expectedArtifacts) {
		t.Fatalf("artifacts = %#v, want %#v", result.Artifacts, expectedArtifacts)
	}
	if len(adapter.Requests()) != 2 {
		t.Fatalf("adapter requests = %d, want dossier summary + selection only", len(adapter.Requests()))
	}
	if len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter resumes = %#v, want none", adapter.Resumes())
	}
	expectedPRKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	if result.PRKey != expectedPRKey {
		t.Fatalf("PRKey = %q, want %q", result.PRKey, expectedPRKey)
	}
	if !reflect.DeepEqual(result.PR, provider.pr) {
		t.Fatalf("PR = %#v, want %#v", result.PR, provider.pr)
	}
	if len(result.Catalog.Agents) != 1 || result.Catalog.Agents[0].ID != "harness:reviewer" {
		t.Fatalf("catalog agents = %#v, want harness:reviewer", result.Catalog.Agents)
	}
	if len(result.Selection.SelectedAgents) != 1 || result.Selection.SelectedAgents[0].AgentID != "harness:reviewer" {
		t.Fatalf("selection = %#v, want harness:reviewer", result.Selection)
	}
	if len(result.ParsedDiff.Patches) != 1 || result.ParsedDiff.Patches[0].Path != "main.go" {
		t.Fatalf("parsed diff = %#v, want main.go patch", result.ParsedDiff.Patches)
	}
	if !reflect.DeepEqual(result.ChangedFiles, []string{"main.go"}) {
		t.Fatalf("changed files = %#v, want main.go", result.ChangedFiles)
	}
	if len(result.Threads) != 1 || result.Threads[0].ID != "thread-1" {
		t.Fatalf("threads = %#v, want thread-1", result.Threads)
	}
	wantCaps := reviewplan.ProviderCaps{NativeFileLevelComments: true, ThreadResolution: true}
	if !reflect.DeepEqual(result.EffectiveCaps, wantCaps) {
		t.Fatalf("EffectiveCaps = %#v, want %#v", result.EffectiveCaps, wantCaps)
	}
	if result.AgentDefsChanged {
		t.Fatal("AgentDefsChanged = true, want false")
	}
	if result.CurrentBaseSHA != provider.pr.Base.SHA || result.CurrentHeadSHA != provider.pr.Head.SHA ||
		result.ReviewBaseSHA != provider.pr.Base.SHA || result.ReviewHeadSHA != provider.pr.Head.SHA {
		t.Fatalf("result SHAs = current %s/%s review %s/%s, want provider PR SHAs", result.CurrentBaseSHA, result.CurrentHeadSHA, result.ReviewBaseSHA, result.ReviewHeadSHA)
	}
	if result.SelectionSession.ProviderSessionID != "selection-session" || result.SelectionSession.Model != "claude-sonnet-4-6" || result.SelectionSession.Effort != "medium" {
		t.Fatalf("selection session = %#v, want selection-session claude-sonnet-4-6/medium", result.SelectionSession)
	}
	assertDossierIndexArtifact(t, result.Artifacts.DossierDir, "final/change-map.md")
	expectedLog, err := expectedArtifacts.AgentLog("orchestrator-selection")
	if err != nil {
		t.Fatalf("AgentLog: %v", err)
	}
	request := adapter.Requests()[1]
	if request.LogPath != expectedLog {
		t.Fatalf("selection log path = %q, want %q", request.LogPath, expectedLog)
	}
	if info, err := os.Stat(expectedArtifacts.AgentLogsDir); err != nil || !info.IsDir() {
		t.Fatalf("agent logs dir stat = (%v, %v), want existing dir", info, err)
	}
	if _, err := os.Stat(result.Artifacts.FindingsJSON); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("findings artifact stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(result.Artifacts.RollupMarkdown); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollup artifact stat error = %v, want not exist", err)
	}
	assertFileContains(t, filepath.Join(result.Artifacts.DossierDir, "final", "discussion.md"), "main.go:2")
	assertDossierIndexArtifact(t, result.Artifacts.DossierDir, "raw/pr-context.json")
}

func TestSelectionOnlyAllowsThreadActionsWithThreadContext(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	provider.threads = []gitprovider.InlineThread{
		crSettledReviewThread(t, "thread-1", "main.go", 2, req.PostingIdentity, human, "Cached settled summary"),
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [{
			"agent_id": "harness:reviewer",
			"rationale": "go file changed",
			"files": ["main.go"]
		}],
		"thread_actions": [{
			"thread_id": "thread-1",
			"decision": "summarize_only",
			"summary": "Thread remains settled"
		}],
		"reasoning": "select reviewer and keep cached thread context"
	}`, 10, 2))

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, t.TempDir()))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}
	if len(result.Selection.ThreadActions) != 1 {
		t.Fatalf("thread actions = %#v, want one", result.Selection.ThreadActions)
	}
	got := result.Selection.ThreadActions[0]
	if got.ThreadID != "thread-1" || got.Decision != review.ThreadDecisionSummarizeOnly || got.Summary != "Thread remains settled" {
		t.Fatalf("thread action = %#v, want decoded action for normalized thread context", got)
	}
}

func TestSelectionOnlyRejectsInvalidSelection(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("missing:agent", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("selection-session-retry", selectionJSON("missing:agent", "main.go"), 10, 2))
	artifactDir := t.TempDir()

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, artifactDir))
	if err == nil || !strings.Contains(err.Error(), "structured output invalid after retry") || !strings.Contains(err.Error(), "unknown selected agent") {
		t.Fatalf("SelectionOnly error = %v, want retry-wrapped unknown selected agent", err)
	}
	if !errors.Is(err, ErrStructuredOutputInvalidAfterRetry) {
		t.Fatalf("SelectionOnly error = %v, want %v", err, ErrStructuredOutputInvalidAfterRetry)
	}
	if !reflect.DeepEqual(result.Artifacts, ArtifactPathsFromDir(artifactDir)) {
		t.Fatalf("artifacts = %#v, want caller-owned dir %q", result.Artifacts, artifactDir)
	}
	if result.SelectionSession.ProviderSessionID != "selection-session-retry" {
		t.Fatalf("selection session = %#v, want retry session id", result.SelectionSession)
	}
	if got := string(result.SelectionSession.Response.StructuredOutput); !strings.Contains(got, `"missing:agent"`) {
		t.Fatalf("selection response = %q, want raw invalid retry payload", got)
	}
	if !reflect.DeepEqual(result.Selection, llm.Selection{}) {
		t.Fatalf("selection = %#v, want zero value on invalid output", result.Selection)
	}
	requests := adapter.Requests()
	if len(requests) != 2 {
		t.Fatalf("adapter requests = %#v, want initial start plus retry", requests)
	}
	if !strings.Contains(requests[1].Prompt, "failed validation") || !strings.Contains(requests[1].Prompt, "unknown selected agent") {
		t.Fatalf("retry prompt = %q, want validation retry details", requests[1].Prompt)
	}
	if len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter resumes = %#v, want none", adapter.Resumes())
	}
}

func TestSelectionOnlyRequiresPostingIdentity(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	selectionReq := selectionRequestFromReview(req, t.TempDir())
	selectionReq.PostingIdentity = gitprovider.Identity{}

	_, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Now:      fixedNow,
	}, selectionReq)
	if err == nil || !strings.Contains(err.Error(), "selection posting identity is required") {
		t.Fatalf("SelectionOnly error = %v, want posting identity required", err)
	}
}

func TestSelectionOnlyExplicitMaxCannotOmitRepoAgents(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	addRepoAgentFixture(provider)
	rulesPath := ".codereview/agents/repo/rules"
	provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents/repo"}] = append(provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents/repo"}], gitprovider.TreeEntry{Path: rulesPath, Type: "tree"})
	provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: rulesPath + "/index.yaml"}] = []byte("name: rules\ndescription: repo rules desc\nmodel_tier: medium\neffort: medium\n")
	provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: rulesPath + "/prompt.md"}] = []byte("Review repository rules.")
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [
			{"agent_id":"repo:guidance","rationale":"repo","files":["main.go"]}
		],
		"thread_actions": [],
		"reasoning": "selected the highest-priority reviewer within the cap"
	}`, 10, 2))

	selectionReq := selectionRequestFromReview(req, t.TempDir())
	selectionReq.MaxAgents = 1
	_, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionReq)
	if err == nil || !strings.Contains(err.Error(), "smaller than the 2 required reviewers") {
		t.Fatalf("SelectionOnly error = %v, want required reviewer cap error", err)
	}
	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %#v, want one selection request", requests)
	}
	if !strings.Contains(requests[0].Prompt, `"max_selected_agents": 1`) {
		t.Fatalf("selection prompt = %q, want max_selected_agents", requests[0].Prompt)
	}
}

func TestSelectionOnlyDefaultCapKeepsRepoAgentsPlusFiveShared(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	addRepoAgentFixture(provider)
	dir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
		writeAgent(t, dir, "harness", name, name+" desc", "Review "+name+" files.")
	}
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	var warnings bytes.Buffer
	adapter.Queue(fakeLLMResult("selection-session", selectionJSONForAgents(
		"main.go",
		"harness:alpha",
		"harness:beta",
		"harness:gamma",
		"harness:delta",
		"harness:epsilon",
		"harness:zeta",
		"repo:guidance",
	), 10, 2))

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
		Warnings: &warnings,
	}, selectionRequestFromReview(req, t.TempDir()))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}
	var got []string
	for _, selected := range result.Selection.SelectedAgents {
		got = append(got, selected.AgentID)
	}
	want := []string{"repo:guidance", "harness:alpha", "harness:beta", "harness:gamma", "harness:delta", "harness:epsilon"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected agents = %#v, want %#v", got, want)
	}
	if got := warnings.String(); !strings.Contains(got, "orchestrator selected 6 shared agents; using first 5 due to default shared-agent limit") {
		t.Fatalf("warnings = %q, want default shared-agent cap warning", got)
	}
	requests := adapter.Requests()
	if len(requests) != 1 || !strings.Contains(requests[0].Prompt, `"max_selected_agents": 6`) || !strings.Contains(requests[0].Prompt, `"required_if_applicable": true`) {
		t.Fatalf("selection prompt = %#v, want repo-required metadata and repo-plus-shared ceiling", requests)
	}
}

func TestRequiredOnMatchAgentIsInjectedWhenOrchestratorOmitsIt(t *testing.T) {
	catalog := agents.Catalog{Agents: []agents.Agent{
		{ID: "shared:optional", Provenance: agents.Provenance{Kind: agents.SourceProfile}},
		{ID: "shared:go", FileGlobs: []string{"**/*.go"}, RequiredOnMatch: true, Provenance: agents.Provenance{Kind: agents.SourceProfile}},
	}}
	selection := llm.Selection{
		SelectedAgents: []llm.SelectedAgent{{AgentID: "shared:optional", Rationale: "optional", Files: []string{"main.go"}}},
		Reasoning:      "Selected optional reviewer.",
	}

	got := ensureRequiredAgents(selection, catalog, []string{"main.go", "internal/app/main.go"})
	if len(got.SelectedAgents) != 2 {
		t.Fatalf("selected agents = %#v, want injected required reviewer", got.SelectedAgents)
	}
	required := got.SelectedAgents[1]
	if required.AgentID != "shared:go" || required.Rationale != "required_on_match matched changed files" {
		t.Fatalf("required reviewer = %#v", required)
	}
	wantFiles := []string{"main.go", "internal/app/main.go"}
	if !reflect.DeepEqual(required.Files, wantFiles) || !reflect.DeepEqual(required.AllowedFiles, wantFiles) {
		t.Fatalf("required assignment = %#v, want matched files %#v", required, wantFiles)
	}
}

func TestRequiredOnMatchAgentsHaveCapPriorityAndCannotBeTruncated(t *testing.T) {
	catalog := agents.Catalog{Agents: []agents.Agent{
		{ID: "shared:optional", Provenance: agents.Provenance{Kind: agents.SourceProfile}},
		{ID: "shared:go", FileGlobs: []string{"**/*.go"}, RequiredOnMatch: true, Provenance: agents.Provenance{Kind: agents.SourceProfile}},
		{ID: "shared:all-go", FileGlobs: []string{"*.go"}, RequiredOnMatch: true, Provenance: agents.Provenance{Kind: agents.SourceFlag}},
	}}
	selection := ensureRequiredAgents(llm.Selection{
		SelectedAgents: []llm.SelectedAgent{{AgentID: "shared:optional", Files: []string{"main.go"}}},
	}, catalog, []string{"main.go"})

	capped, err := (Options{}).capSelectionAgents(selection, catalog, []string{"main.go"}, 2)
	if err != nil {
		t.Fatalf("capSelectionAgents: %v", err)
	}
	got := []string{capped.SelectedAgents[0].AgentID, capped.SelectedAgents[1].AgentID}
	want := []string{"shared:go", "shared:all-go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capped agents = %#v, want required agents %#v", got, want)
	}
	if _, err := (Options{}).capSelectionAgents(selection, catalog, []string{"main.go"}, 1); err == nil || !strings.Contains(err.Error(), "smaller than the 2 required reviewers") {
		t.Fatalf("cap error = %v, want required reviewer conflict", err)
	}
}

func TestReviewerProgressReportsWinningSourcesAndFinalAssignments(t *testing.T) {
	recorder := &reviewerProgressRecorder{}
	opts := Options{ReviewProgress: recorder}
	catalog := agents.Catalog{
		Agents: []agents.Agent{
			{ID: "shared:go", FileGlobs: []string{"**/*.go"}, RequiredOnMatch: true, Provenance: agents.Provenance{Kind: agents.SourceProfile}},
			{ID: "repo:rules", Provenance: agents.Provenance{Kind: agents.SourceRepo, Ref: "refs/heads/main", SHA: "abc123"}},
		},
		Sources: []agents.SourceInfo{{Kind: agents.SourceRepo, Status: agents.SourceStatusAvailable}},
	}
	opts.emitReviewerCatalog(catalog, []string{"main.go"})
	opts.emitReviewerSelection(catalog, llm.Selection{
		SelectedAgents: []llm.SelectedAgent{{
			AgentID: "shared:go", Rationale: "required_on_match matched changed files", Files: []string{"main.go"}, AllowedFiles: []string{"main.go"},
		}},
		Reasoning: "Go reviewer is required.",
	})

	if len(recorder.catalogs) != 1 || recorder.catalogs[0].RepoStatus != string(agents.SourceStatusAvailable) || recorder.catalogs[0].OfferedCount != 2 {
		t.Fatalf("catalog progress = %#v", recorder.catalogs)
	}
	if !recorder.catalogs[0].Reviewers[0].RequiredIfApplicable || recorder.catalogs[0].Reviewers[0].SourceKind != string(agents.SourceProfile) {
		t.Fatalf("profile required reviewer progress = %#v", recorder.catalogs[0].Reviewers[0])
	}
	if len(recorder.selections) != 1 || recorder.selections[0].Reasoning != "Go reviewer is required." || len(recorder.selections[0].Reviewers) != 1 {
		t.Fatalf("selection progress = %#v", recorder.selections)
	}
	if got := recorder.selections[0].Reviewers[0]; got.AgentID != "shared:go" || got.Rationale != "required_on_match matched changed files" || !reflect.DeepEqual(got.AllowedFiles, []string{"main.go"}) {
		t.Fatalf("assignment progress = %#v", got)
	}
}

type reviewerProgressRecorder struct {
	catalogs   []ReviewerCatalogProgress
	selections []ReviewerSelectionProgress
}

func (r *reviewerProgressRecorder) ReviewersResolved(event ReviewerCatalogProgress) {
	r.catalogs = append(r.catalogs, event)
}

func (r *reviewerProgressRecorder) ReviewersSelected(event ReviewerSelectionProgress) {
	r.selections = append(r.selections, event)
}

func TestSelectionOnlyContextBudgetFailure(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "reviewer", strings.Repeat("large ", 80), "prompt")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

	_, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
		Budget:   ContextBudget{MaxPromptBytes: 100},
	}, selectionRequestFromReview(req, t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "context budget exceeded for selection model claude-sonnet-4-6") {
		t.Fatalf("SelectionOnly error = %v, want selection budget failure", err)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %#v, want no LLM call after budget failure", adapter.Requests())
	}
}

func TestSelectionOnlyNoDiffSkipsLLMAndReturnsPreparedContext(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	provider.diff = gitprovider.UnifiedDiff{}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	artifactDir := t.TempDir()

	result, err := selectionOnlyForTest(ctx, Options{
		Provider: provider,
		Adapter:  adapter,
		Now:      fixedNow,
	}, selectionRequestFromReview(req, artifactDir))
	if err != nil {
		t.Fatalf("SelectionOnly: %v", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
	if !reflect.DeepEqual(result.Artifacts, ArtifactPathsFromDir(artifactDir)) {
		t.Fatalf("artifacts = %#v, want caller-owned dir %q", result.Artifacts, artifactDir)
	}
	if len(result.ParsedDiff.Patches) != 0 || len(result.ChangedFiles) != 0 {
		t.Fatalf("parsed diff = %#v changed files = %#v, want empty", result.ParsedDiff.Patches, result.ChangedFiles)
	}
	if !reflect.DeepEqual(result.Selection, llm.Selection{}) || !reflect.DeepEqual(result.SelectionSession, SelectionSession{}) {
		t.Fatalf("selection result = %#v session = %#v, want zero values", result.Selection, result.SelectionSession)
	}
	if len(result.Catalog.Agents) != 1 || result.Catalog.Agents[0].ID != "harness:reviewer" {
		t.Fatalf("catalog agents = %#v, want harness:reviewer", result.Catalog.Agents)
	}
}

func TestDryRunNoDiffDoesNotResolveUnmappedModelTier(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	req.Profile.LLM = config.LLMConfig{
		Provider: config.LLMProviderAnthropic,
		Auth:     config.LLMAuthAPIKey,
		Adapter:  config.LLMAdapterAnthropicAPI,
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-no-diff-unmapped" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
	if result.Plan.Outcome != reviewplan.OutcomeNothingToReview {
		t.Fatalf("Plan.Outcome = %q, want %q", result.Plan.Outcome, reviewplan.OutcomeNothingToReview)
	}
}

func TestDryRunNoDiffWithMissingRepoGuidanceStillReturnsNothingToReview(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	provider.diff = gitprovider.UnifiedDiff{}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-no-diff-missing-repo-guidance" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
	if result.Plan.Outcome != reviewplan.OutcomeNothingToReview {
		t.Fatalf("Plan.Outcome = %q, want %q", result.Plan.Outcome, reviewplan.OutcomeNothingToReview)
	}
	gotKinds := []reviewplan.ActionKind{result.Plan.Actions[0].Kind}
	if !reflect.DeepEqual(gotKinds, []reviewplan.ActionKind{reviewplan.ActionKindRollupComment}) {
		t.Fatalf("action kinds = %#v, want rollup only", gotKinds)
	}
}

func TestDryRunAgentModelTierUsesProfileModelMapOverride(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.Profile.LLM.ModelMap = config.ModelMap{"medium": "profile-medium-model"}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-model-map-override" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	for _, request := range adapter.Requests() {
		if request.Model != "profile-medium-model" || request.Effort != "medium" {
			t.Fatalf("request = model:%q effort:%q, want profile-medium-model/medium", request.Model, request.Effort)
		}
	}
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	for _, session := range sessions {
		if session.Model != "profile-medium-model" {
			t.Fatalf("session.Model = %q, want profile-medium-model", session.Model)
		}
	}
}

func TestDryRunReviewerBaselineTierRaisesReviewerModelFloor(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.Profile.LLM.ModelMap = config.ModelMap{"large": "profile-large-model"}
	req.Profile.LLM.ReviewerModelTier = config.ModelTierLarge
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-baseline-large" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "profile-large-model", "claude-sonnet-4-6"}
	for i, request := range adapter.Requests() {
		if request.Model != wantModels[i] {
			t.Fatalf("request[%d].Model = %q, want %q", i, request.Model, wantModels[i])
		}
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      "medium",
		BaselineTier:   "large",
		EffectiveTier:  "large",
		ResolvedModel:  "profile-large-model",
		ModelMapSource: config.ModelMapSourceConfig,
	})
}

func TestDryRunMissingRepoGuidanceFallsBackToProfileAgents(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-missing-repo-guidance" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	source, ok := repoGuidanceSource(result.Catalog.Sources)
	if !ok || source.Status != agents.SourceStatusMissing {
		t.Fatalf("repo source = %#v, want missing provenance", result.Catalog.Sources)
	}
	if len(result.Selection.SelectedAgents) != 1 || result.Selection.SelectedAgents[0].AgentID != "harness:reviewer" {
		t.Fatalf("selection = %#v, want profile fallback reviewer", result.Selection)
	}
	if result.Plan.Outcome != reviewplan.OutcomeComment {
		t.Fatalf("outcome = %q, want normal rollup comment", result.Plan.Outcome)
	}
}

func TestDryRunEmptyMergedCatalogFailsWithoutReviewerExecution(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	req.Profile.AgentSources = nil
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-empty-catalog" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err == nil || !strings.Contains(err.Error(), "no reviewer agents available") {
		t.Fatalf("DryRun error = %v, want empty catalog error", err)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %#v, want no LLM execution", adapter.Requests())
	}
}

func TestDryRunInvalidOrUnreadableRepoGuidanceForcesRequestChangesWithoutReviewerExecution(t *testing.T) {
	tests := []struct {
		name      string
		setupRepo func(t *testing.T, provider *readOnlyProvider)
		wantText  string
		wantState agents.SourceStatus
	}{
		{
			name: "unreadable",
			setupRepo: func(t *testing.T, provider *readOnlyProvider) {
				t.Helper()
				removeRepoAgentFixture(provider)
				categoryPath := ".codereview/agents/cat"
				provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents"}] = []gitprovider.TreeEntry{{Path: "cat", Type: "tree"}}
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: categoryPath + "/index.yaml"}] = []byte("name: cat\ndescription: cat category\nowner: owner\n")
				provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: categoryPath}] = []gitprovider.TreeEntry{{Path: categoryPath + "/agent", Type: "tree"}}
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: categoryPath + "/agent/index.yaml"}] = []byte("name: agent\ndescription: desc\nmodel_tier: medium\neffort: medium\n")
			},
			wantText:  "Base branch `.codereview/agents/` could not be read as trusted review guidance.",
			wantState: agents.SourceStatusUnreadable,
		},
		{
			name: "invalid",
			setupRepo: func(t *testing.T, provider *readOnlyProvider) {
				t.Helper()
				removeRepoAgentFixture(provider)
				categoryPath := ".codereview/agents/cat"
				provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents"}] = []gitprovider.TreeEntry{{Path: "cat", Type: "tree"}}
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: categoryPath + "/index.yaml"}] = []byte("name: other\ndescription: cat category\nowner: owner\n")
			},
			wantText:  "Base branch `.codereview/agents/` was invalid and could not be used as trusted review guidance.",
			wantState: agents.SourceStatusInvalid,
		},
		{
			name: "empty root tree",
			setupRepo: func(t *testing.T, provider *readOnlyProvider) {
				t.Helper()
				removeRepoAgentFixture(provider)
				provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents"}] = []gitprovider.TreeEntry{}
			},
			wantText:  "Base branch `.codereview/agents/` was invalid and could not be used as trusted review guidance.",
			wantState: agents.SourceStatusInvalid,
		},
		{
			name: "empty category",
			setupRepo: func(t *testing.T, provider *readOnlyProvider) {
				t.Helper()
				removeRepoAgentFixture(provider)
				categoryPath := ".codereview/agents/cat"
				provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents"}] = []gitprovider.TreeEntry{{Path: "cat", Type: "tree"}}
				provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: categoryPath + "/index.yaml"}] = []byte("name: cat\ndescription: cat category\nowner: owner\n")
				provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: categoryPath}] = []gitprovider.TreeEntry{}
			},
			wantText:  "Base branch `.codereview/agents/` was invalid and could not be used as trusted review guidance.",
			wantState: agents.SourceStatusInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			tt.setupRepo(t, provider)
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

			result, err := dryRunForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewRunID:        func() string { return "run-repo-guidance-" + tt.name },
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			source, ok := repoGuidanceSource(result.Catalog.Sources)
			if !ok || source.Status != tt.wantState {
				t.Fatalf("repo source = %#v, want %q", result.Catalog.Sources, tt.wantState)
			}
			if result.Plan.Outcome != reviewplan.OutcomeRequestChanges {
				t.Fatalf("outcome = %q, want request_changes", result.Plan.Outcome)
			}
			gotKinds := []reviewplan.ActionKind{result.Plan.Actions[0].Kind}
			if !reflect.DeepEqual(gotKinds, []reviewplan.ActionKind{reviewplan.ActionKindSubmitReview}) {
				t.Fatalf("action kinds = %#v, want submit only", gotKinds)
			}
			if submit := result.Plan.Actions[0].SubmitReview; submit == nil || submit.Event != review.ReviewEventRequestChanges || submit.Body != result.Plan.RollupMarkdown {
				t.Fatalf("submit review = %#v, want request_changes with rollup body", result.Plan.Actions[0].SubmitReview)
			}
			if !strings.Contains(result.Plan.RollupMarkdown, tt.wantText) {
				t.Fatalf("rollup = %q, want %q", result.Plan.RollupMarkdown, tt.wantText)
			}
			for _, request := range adapter.Requests() {
				if strings.Contains(request.Prompt, `"schema": "selection"`) || strings.Contains(request.Prompt, `"schema": "rollup"`) || strings.Contains(request.Prompt, `"task": "review files and return findings JSON only"`) {
					t.Fatalf("unexpected review pipeline prompt: %q", request.Prompt)
				}
			}
		})
	}
}

func TestDryRunSelectionOverridesApplyOnlyToSelection(t *testing.T) {
	tests := []struct {
		name           string
		modelOverride  string
		effortOverride string
		wantModels     []string
		wantEfforts    []string
	}{
		{
			name:           "model and effort",
			modelOverride:  "bench-model",
			effortOverride: "high",
			wantModels:     []string{"bench-model", "claude-sonnet-4-6", "claude-sonnet-4-6"},
			wantEfforts:    []string{"high", "medium", "medium"},
		},
		{
			name:          "model only",
			modelOverride: "bench-model",
			wantModels:    []string{"bench-model", "claude-sonnet-4-6", "claude-sonnet-4-6"},
			wantEfforts:   []string{"medium", "medium", "medium"},
		},
		{
			name:           "effort only",
			effortOverride: "high",
			wantModels:     []string{"claude-sonnet-4-6", "claude-sonnet-4-6", "claude-sonnet-4-6"},
			wantEfforts:    []string{"high", "medium", "medium"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			req.SelectionModelOverride = tt.modelOverride
			req.SelectionEffortOverride = tt.effortOverride
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
			adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
			adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
			adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

			result, err := dryRunForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewRunID:        func() string { return "run-override-" + strings.ReplaceAll(tt.name, " ", "-") },
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}

			requests := adapter.Requests()
			if len(requests) != 3 {
				t.Fatalf("requests len = %d, want delegated selection/reviewer/rollup", len(requests))
			}
			for i, request := range requests {
				if request.Model != tt.wantModels[i] || request.Effort != tt.wantEfforts[i] {
					t.Fatalf("request[%d] = model:%q effort:%q, want %s/%s", i, request.Model, request.Effort, tt.wantModels[i], tt.wantEfforts[i])
				}
			}
			sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
			if err != nil {
				t.Fatalf("ListSessionsForRun: %v", err)
			}
			if len(sessions) != 4 {
				t.Fatalf("sessions len = %d, want selection/two reviewers/rollup", len(sessions))
			}
			wantSessionModels := []string{tt.wantModels[0], "claude-sonnet-4-6", tt.wantModels[1], tt.wantModels[2]}
			wantSessionEfforts := []string{tt.wantEfforts[0], "medium", tt.wantEfforts[1], tt.wantEfforts[2]}
			for i, session := range sessions {
				if session.Model != wantSessionModels[i] || session.Effort == nil || *session.Effort != wantSessionEfforts[i] {
					t.Fatalf("session[%d] = model:%q effort:%v, want %s/%s", i, session.Model, session.Effort, wantSessionModels[i], wantSessionEfforts[i])
				}
			}
			data, err := os.ReadFile(result.Artifacts.AgentSourcesJSON) // #nosec G304 -- test reads artifact paths returned by the pipeline under t.TempDir.
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", result.Artifacts.AgentSourcesJSON, err)
			}
			if tt.modelOverride != "" && strings.Contains(string(data), tt.modelOverride) {
				t.Fatalf("agent source artifact contains runtime override model: %s", data)
			}
			assertAgentSourcesArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer")
		})
	}
}

func TestDryRunReviewerOverridesApplyOnlyToReviewers(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.ReviewerModelOverride = "bench-reviewer-model"
	req.ReviewerEffortOverride = "low"
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-override" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "bench-reviewer-model", "bench-reviewer-model", "claude-sonnet-4-6"}
	wantEfforts := []string{"medium", "low", "low", "medium"}
	wantRequestModels := []string{"claude-sonnet-4-6", "bench-reviewer-model", "claude-sonnet-4-6"}
	wantRequestEfforts := []string{"medium", "low", "medium"}
	requests := adapter.Requests()
	for i, request := range requests {
		if request.Model != wantRequestModels[i] || request.Effort != wantRequestEfforts[i] {
			t.Fatalf("request[%d] = model:%q effort:%q, want %s/%s", i, request.Model, request.Effort, wantRequestModels[i], wantRequestEfforts[i])
		}
	}
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	for i, session := range sessions {
		if session.Model != wantModels[i] || session.Effort == nil || *session.Effort != wantEfforts[i] {
			t.Fatalf("session[%d] = model:%q effort:%v, want %s/%s", i, session.Model, session.Effort, wantModels[i], wantEfforts[i])
		}
	}
}

func TestDryRunReviewerFailureIsolation(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgent(t, req.Profile.AgentSources[0], "harness", "alpha", "alpha desc", "Review alpha.")
	writeAgent(t, req.Profile.AgentSources[0], "harness", "beta", "beta desc", "Review beta.")
	writeAgent(t, req.Profile.AgentSources[0], "harness", "gamma", "gamma desc", "Review gamma.")
	adapter := &reviewerIsolationAdapter{supportsResume: true, reviewerBarrier: newReviewerStartBarrier(3)}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-isolation" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  3,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(result.Findings) != 2 {
		t.Fatalf("findings len = %d, want 2 successful reviewer findings", len(result.Findings))
	}
	if len(result.ReviewerFailures) != 1 || result.ReviewerFailures[0].AgentID != "harness:beta" {
		t.Fatalf("reviewer failures = %#v findings = %#v, want isolated beta failure", result.ReviewerFailures, result.Findings)
	}
	if len(result.Sessions) != 6 {
		t.Fatalf("sessions len = %d, want selection, four reviewers, beta retry, and rollup", len(result.Sessions))
	}
	betaMeta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(result.Artifacts), reviewerTaskID("harness:beta"))
	if err != nil || !ok {
		t.Fatalf("read beta task metadata = ok %v err %v", ok, err)
	}
	if betaMeta.Status != llmTaskStatusFailedIsolated || len(betaMeta.Attempts) != 2 {
		t.Fatalf("beta metadata = %#v, want failed_isolated with initial and retry attempts", betaMeta)
	}
	if !adapter.BetaRetrySawCleanWorkspace() {
		t.Fatalf("beta retry reused dirty reviewer workspace; want clean workspace for validation retry")
	}
	for _, attempt := range betaMeta.Attempts {
		if attempt.DecodeError == "" {
			t.Fatalf("beta attempt %#v missing decode error", attempt)
		}
		assertTaskPayloadContains(t, attempt.RawOutputPath, `"agent_id": "harness:beta"`)
	}
	for _, agentID := range []string{"harness:alpha", "harness:gamma"} {
		meta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(result.Artifacts), reviewerTaskID(agentID))
		if err != nil || !ok {
			t.Fatalf("read %s task metadata = ok %v err %v", agentID, ok, err)
		}
		if meta.Status != llmTaskStatusSucceeded {
			t.Fatalf("%s metadata status = %q, want succeeded", agentID, meta.Status)
		}
		assertTaskPayloadContains(t, meta.ValidatedOutputPath, agentID)
	}
	if got := adapter.ReviewerStartedCount(); got != 3 {
		t.Fatalf("reviewer starts = %d, want all three reviewers to start before release", got)
	}
	for _, agentID := range []string{"harness:alpha", "harness:beta", "harness:gamma"} {
		encoded := statepaths.Encode(agentID)
		if _, err := os.Stat(filepath.Join(result.Artifacts.WorkbenchDir, "reviewers", encoded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reviewer workspace %s stat err = %v, want cleaned", agentID, err)
		}
		if _, err := os.Stat(filepath.Join(result.Artifacts.WorkbenchScratch, encoded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reviewer scratch %s stat err = %v, want cleaned", agentID, err)
		}
	}
	requests := adapter.Requests()
	if len(requests) != 6 {
		t.Fatalf("requests len = %d, want selection, three reviewers, beta retry, rollup", len(requests))
	}
	rollupPrompt := requests[len(requests)-1].Prompt
	if !strings.Contains(rollupPrompt, `"reviewer_failures"`) || !strings.Contains(rollupPrompt, `"agent_id": "harness:beta"`) {
		t.Fatalf("rollup prompt missing isolated reviewer failure context: %s", rollupPrompt)
	}
	if result.Plan.Outcome != reviewplan.OutcomeComment {
		t.Fatalf("plan outcome = %q, want comment with isolated reviewer failure", result.Plan.Outcome)
	}
	if !strings.Contains(result.Plan.RollupMarkdown, "### Reviewer Diagnostics") || !strings.Contains(result.Plan.RollupMarkdown, "harness:beta") {
		t.Fatalf("rollup markdown missing reviewer diagnostic:\n%s", result.Plan.RollupMarkdown)
	}
}

func TestDryRunReviewerProviderFailureIsolation(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgent(t, req.Profile.AgentSources[0], "harness", "alpha", "alpha desc", "Review alpha.")
	writeAgent(t, req.Profile.AgentSources[0], "harness", "beta", "beta desc", "Review beta.")
	writeAgent(t, req.Profile.AgentSources[0], "harness", "gamma", "gamma desc", "Review gamma.")
	betaErr := errors.New("provider wait failed")
	adapter := &reviewerIsolationAdapter{betaProviderErr: betaErr, reviewerBarrier: newReviewerStartBarrier(3)}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-provider-isolation" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  3,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if len(result.Findings) != 2 {
		t.Fatalf("findings len = %d, want 2 successful reviewer findings", len(result.Findings))
	}
	if len(result.ReviewerFailures) != 1 || result.ReviewerFailures[0].AgentID != "harness:beta" {
		t.Fatalf("reviewer failures = %#v, want isolated beta failure", result.ReviewerFailures)
	}
	betaMeta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(result.Artifacts), reviewerTaskID("harness:beta"))
	if err != nil || !ok {
		t.Fatalf("read beta task metadata = ok %v err %v", ok, err)
	}
	if betaMeta.Status != llmTaskStatusFailedIsolated || betaMeta.ProviderSessionID != "beta-provider-session" {
		t.Fatalf("beta metadata = %#v, want isolated failure with provider session", betaMeta)
	}
	if len(betaMeta.Attempts) != 0 {
		t.Fatalf("beta attempts = %#v, want none for provider wait failure", betaMeta.Attempts)
	}
	if !strings.Contains(betaMeta.Error, "provider wait failed") {
		t.Fatalf("beta error = %q, want provider diagnostic", betaMeta.Error)
	}
	for _, agentID := range []string{"harness:alpha", "harness:gamma"} {
		meta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(result.Artifacts), reviewerTaskID(agentID))
		if err != nil || !ok {
			t.Fatalf("read %s task metadata = ok %v err %v", agentID, ok, err)
		}
		assertTaskPayloadContains(t, meta.ValidatedOutputPath, agentID)
	}
	if got := adapter.ReviewerStartedCount(); got != 3 {
		t.Fatalf("reviewer starts = %d, want all three reviewers to start before release", got)
	}
	requests := adapter.Requests()
	if len(requests) != 5 {
		t.Fatalf("requests len = %d, want selection, three reviewers, rollup", len(requests))
	}
	rollupPrompt := requests[len(requests)-1].Prompt
	if !strings.Contains(rollupPrompt, `"reviewer_failures"`) || !strings.Contains(rollupPrompt, `"agent_id": "harness:beta"`) {
		t.Fatalf("rollup prompt missing isolated reviewer failure context: %s", rollupPrompt)
	}
	if result.Plan.Outcome != reviewplan.OutcomeComment {
		t.Fatalf("plan outcome = %q, want comment with isolated reviewer failure", result.Plan.Outcome)
	}
}

func TestLiveResumeCompletedSelectionAndReviewersRerunsOnlyRollup(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	if err := store.UpsertNamedSession(ctx, namedSessionForRequest(req, "stored-session")); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	run := allocateLiveRun(t, store, provider, req, "run-rollup-resume")
	firstAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	firstAdapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	firstAdapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	firstAdapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"missing-finding"}), 30, 6))
	firstAdapter.Queue(fakeLLMResult("rollup-retry-session", rollupJSON("comment", []string{"missing-finding"}), 30, 6))
	findingID := func() (review.FindingID, error) { return review.FindingID("finding-1"), nil }

	_, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         firstAdapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingID,
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err == nil || !errors.Is(err, ErrStructuredOutputInvalidAfterRetry) {
		t.Fatalf("first Live error = %v, want invalid rollup after retry", err)
	}
	stored, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Outcome != nil {
		t.Fatalf("run outcome = %#v, want pipeline to leave live outcome ownership to reviewrun", stored.Outcome)
	}
	selectionMeta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(ArtifactPathsFromDir(run.ArtifactPath)), orchestratorSelectionStage)
	if err != nil || !ok || selectionMeta.Status != llmTaskStatusSucceeded {
		t.Fatalf("selection metadata = %#v ok %v err %v, want succeeded", selectionMeta, ok, err)
	}
	reviewerMeta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(ArtifactPathsFromDir(run.ArtifactPath)), reviewerTaskID("harness:reviewer"))
	if err != nil || !ok || reviewerMeta.Status != llmTaskStatusSucceeded {
		t.Fatalf("reviewer metadata = %#v ok %v err %v, want succeeded", reviewerMeta, ok, err)
	}
	rollupMeta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(ArtifactPathsFromDir(run.ArtifactPath)), orchestratorRollupStage)
	if err != nil || !ok || rollupMeta.Status != llmTaskStatusFailedBlocking {
		t.Fatalf("rollup metadata = %#v ok %v err %v, want failed_blocking", rollupMeta, ok, err)
	}

	secondAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	secondAdapter.Queue(fakeLLMResult("rollup-fixed-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         secondAdapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("resume-session"),
		NewFindingID:    findingID,
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("second Live: %v", err)
	}
	if len(secondAdapter.Requests()) != 0 {
		t.Fatalf("second adapter starts = %#v, want no fresh selection/reviewer/rollup start", secondAdapter.Requests())
	}
	resumes := secondAdapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "rollup-retry-session" {
		t.Fatalf("second adapter resumes = %#v, want only failed rollup retry session", resumes)
	}
	if len(result.Findings) != 1 || result.Findings[0].ID != "finding-1" {
		t.Fatalf("result findings = %#v, want loaded reviewer finding", result.Findings)
	}
	if result.Rollup.ReviewEvent != review.ReviewEventComment {
		t.Fatalf("rollup = %#v, want rerun rollup result", result.Rollup)
	}
	if len(result.PlannedActions) == 0 {
		t.Fatal("planned actions len = 0, want resumed pipeline to synthesize actions")
	}
}

func TestRunStructuredTaskRejectsAdapterMismatchBeforeRetry(t *testing.T) {
	ctx := context.Background()
	artifacts := ArtifactPathsFromDir(t.TempDir())
	spec := llmTaskSpec{
		runID:            "run-adapter-mismatch",
		taskID:           orchestratorRollupStage,
		phase:            "rollup",
		inputFingerprint: "fingerprint",
		artifacts:        artifacts,
		role:             ledger.SessionRoleOrchestrator,
		model:            "model",
		effort:           "medium",
		prompt:           "prompt",
	}
	meta := llmlifecycle.Metadata{
		SchemaVersion:     llmlifecycle.SchemaVersion,
		TaskID:            spec.taskID,
		Phase:             spec.phase,
		InputFingerprint:  spec.inputFingerprint,
		Adapter:           "old-llm",
		Status:            llmTaskStatusFailedBlocking,
		SessionRowID:      "session-row",
		ProviderSessionID: "old-provider-session",
	}
	if err := llmlifecycle.WriteMetadata(lifecyclePaths(artifacts), meta); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "new-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("new-session", `"ok"`, 1, 1))

	_, _, _, err := runStructuredTask[string](ctx, Options{Adapter: adapter}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if err == nil || !strings.Contains(err.Error(), `adapter = "old-llm", want "new-llm"`) {
		t.Fatalf("runStructuredTask error = %v, want adapter mismatch", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter invoked despite mismatch: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
}

func TestRunStructuredTaskRejectsDependencyTaskIDMismatchBeforeRetry(t *testing.T) {
	ctx := context.Background()
	artifacts := ArtifactPathsFromDir(t.TempDir())
	spec := llmTaskSpec{
		runID:             "run-dependency-mismatch",
		taskID:            orchestratorSelectionStage,
		phase:             "selection",
		dependencyTaskIDs: []string{dossierSummaryTaskID},
		inputFingerprint:  "fingerprint",
		artifacts:         artifacts,
		role:              ledger.SessionRoleOrchestrator,
		model:             "model",
		effort:            "medium",
		prompt:            "prompt",
	}
	meta := llmlifecycle.Metadata{
		SchemaVersion:     llmlifecycle.SchemaVersion,
		TaskID:            spec.taskID,
		Phase:             spec.phase,
		DependencyTaskIDs: nil,
		InputFingerprint:  spec.inputFingerprint,
		Adapter:           "fake-llm",
		Status:            llmTaskStatusFailedBlocking,
		SessionRowID:      "session-row",
		ProviderSessionID: "provider-session",
	}
	if err := llmlifecycle.WriteMetadata(lifecyclePaths(artifacts), meta); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("new-session", `"ok"`, 1, 1))

	_, _, _, err := runStructuredTask[string](ctx, Options{Adapter: adapter}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if err == nil || !strings.Contains(err.Error(), "dependency task ids") {
		t.Fatalf("runStructuredTask error = %v, want dependency task id mismatch", err)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter invoked despite dependency mismatch: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
}

func TestRunStructuredTaskReviewerStartFailureIsBlocking(t *testing.T) {
	ctx := context.Background()
	artifacts := ArtifactPathsFromDir(t.TempDir())
	startErr := errors.New("auth failed")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{StartErr: startErr})
	spec := llmTaskSpec{
		runID:            "run-reviewer-start-failure",
		taskID:           reviewerTaskID("harness:beta"),
		phase:            "reviewer",
		inputFingerprint: "fingerprint",
		artifacts:        artifacts,
		role:             ledger.SessionRoleReviewer,
		model:            "model",
		effort:           "medium",
		prompt:           "prompt",
		llmFailureStatus: llmTaskStatusFailedIsolated,
	}

	_, _, _, err := runStructuredTask[string](ctx, Options{Adapter: adapter}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if !errors.Is(err, startErr) || !errors.Is(err, errLLMTaskFailedBlocking) {
		t.Fatalf("runStructuredTask error = %v, want blocking start error wrapping %v", err, startErr)
	}
	meta, ok, readErr := llmlifecycle.ReadMetadata(lifecyclePaths(artifacts), spec.taskID)
	if readErr != nil || !ok {
		t.Fatalf("read task metadata = ok %v err %v", ok, readErr)
	}
	if meta.Status != llmTaskStatusFailedBlocking || meta.ProviderSessionID != "" {
		t.Fatalf("metadata = %#v, want failed_blocking without provider session", meta)
	}
}

func TestRunStructuredTaskReportsProgressOnExecution(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	artifacts := ArtifactPathsFromDir(t.TempDir())
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	allocatePipelineRun(t, store, layout, "run-progress", ledger.PostModeDryRun, fixedNow())
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("task-session", `"ok"`, 5, 3))
	progress := &fakeTaskProgress{}
	spec := llmTaskSpec{
		runID:            "run-progress",
		taskID:           orchestratorRollupStage,
		phase:            "rollup",
		inputFingerprint: "fingerprint",
		artifacts:        artifacts,
		role:             ledger.SessionRoleOrchestrator,
		model:            "gpt-5.5",
		effort:           "high",
		logPath:          filepath.Join(t.TempDir(), "rollup.jsonl"),
		prompt:           "prompt",
	}

	value, _, _, err := runStructuredTask[string](ctx, Options{
		Adapter:         adapter,
		Store:           store,
		TaskProgress:    progress,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
	}, spec, func(data []byte) (string, error) {
		return string(data), nil
	})
	if err != nil {
		t.Fatalf("runStructuredTask: %v", err)
	}
	if value != `"ok"` {
		t.Fatalf("value = %q, want ok payload", value)
	}
	if len(progress.starts) != 1 || len(progress.ends) != 1 {
		t.Fatalf("progress starts=%d ends=%d, want 1/1", len(progress.starts), len(progress.ends))
	}
	if progress.starts[0].TaskID != orchestratorRollupStage || progress.starts[0].Phase != "rollup" || progress.starts[0].Source != "execute" {
		t.Fatalf("start = %#v, want rollup execute event", progress.starts[0])
	}
	if progress.starts[0].Model != spec.model || progress.starts[0].Effort != spec.effort || progress.starts[0].LogPath != spec.logPath {
		t.Fatalf("start fields = %#v, want model/effort/logPath from spec", progress.starts[0])
	}
	if progress.ends[0].result.Status != string(llmTaskStatusSucceeded) || progress.ends[0].result.ProviderSessionID != "task-session" || progress.ends[0].result.Cached {
		t.Fatalf("end result = %#v, want succeeded uncached task-session", progress.ends[0].result)
	}
}

func assertTaskPayloadContains(t *testing.T, path, want string) {
	t.Helper()
	if strings.TrimSpace(path) == "" {
		t.Fatalf("task payload path is empty, want file containing %q", want)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test reads artifact paths produced by the pipeline under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("task payload %s = %s, want %q", path, data, want)
	}
}

type fakeTaskProgress struct {
	mu     sync.Mutex
	starts []LLMTaskProgressEvent
	ends   []fakeTaskProgressEnd
	loads  []fakeTaskProgressLoad
}

type fakeTaskProgressSpan struct {
	parent *fakeTaskProgress
	event  LLMTaskProgressEvent
}

type fakeTaskProgressEnd struct {
	event  LLMTaskProgressEvent
	err    error
	result LLMTaskProgressResult
}

type fakeTaskProgressLoad struct {
	event  LLMTaskProgressEvent
	result LLMTaskProgressResult
}

func (f *fakeTaskProgress) StartLLMTask(event LLMTaskProgressEvent) LLMTaskProgressSpan {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, event)
	return fakeTaskProgressSpan{parent: f, event: event}
}

func (f *fakeTaskProgress) LoadLLMTask(event LLMTaskProgressEvent, result LLMTaskProgressResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, fakeTaskProgressLoad{event: event, result: result})
}

func (s fakeTaskProgressSpan) End(err error, result LLMTaskProgressResult) {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	s.parent.ends = append(s.parent.ends, fakeTaskProgressEnd{event: s.event, err: err, result: result})
}

func TestDryRunReviewerModelTierOverrideAppliesOnlyToReviewers(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.Profile.LLM.ModelMap = config.ModelMap{"large": "profile-large-model"}
	req.ReviewerModelTierOverride = "large"
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-reviewer-tier-override" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "profile-large-model", "claude-sonnet-4-6"}
	for i, request := range adapter.Requests() {
		if request.Model != wantModels[i] {
			t.Fatalf("request[%d].Model = %q, want %q", i, request.Model, wantModels[i])
		}
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      "medium",
		BaselineTier:   "large",
		EffectiveTier:  "large",
		ResolvedModel:  "profile-large-model",
		ModelMapSource: config.ModelMapSourceConfig,
	})
}

func TestDryRunAgentModelIDBypassesModelMapForReviewer(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgentModelID(t, req.Profile.AgentSources[0], "harness", "reviewer", "agent-provider-model")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-model-id" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	requests := adapter.Requests()
	if len(requests) != 3 {
		t.Fatalf("requests len = %d, want selection/reviewer/rollup", len(requests))
	}
	wantModels := []string{"claude-sonnet-4-6", "agent-provider-model", "claude-sonnet-4-6"}
	for i, request := range requests {
		if request.Model != wantModels[i] || request.Effort != "medium" {
			t.Fatalf("request[%d] = model:%q effort:%q, want %s/medium", i, request.Model, request.Effort, wantModels[i])
		}
	}
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	modelCounts := map[string]int{}
	for _, session := range sessions {
		modelCounts[session.Model]++
	}
	if !reflect.DeepEqual(modelCounts, map[string]int{"claude-sonnet-4-6": 3, "agent-provider-model": 1}) {
		t.Fatalf("session model counts = %#v, want three default and one agent-specific", modelCounts)
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:          "exact_model",
		ResolvedModel: "agent-provider-model",
	})
}

func TestDryRunReviewerBaselineDoesNotAffectAgentModelID(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgentModelID(t, req.Profile.AgentSources[0], "harness", "reviewer", "agent-provider-model")
	req.Profile.LLM.ReviewerModelTier = config.ModelTierLarge
	req.Profile.LLM.ModelMap = config.ModelMap{"large": "profile-large-model"}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-model-id-baseline" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "agent-provider-model", "claude-sonnet-4-6"}
	for i, request := range adapter.Requests() {
		if request.Model != wantModels[i] {
			t.Fatalf("request[%d].Model = %q, want %q", i, request.Model, wantModels[i])
		}
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:          "exact_model",
		ResolvedModel: "agent-provider-model",
	})
}

func TestDryRunReviewerFloorsResolveIndependentlyPerAgent(t *testing.T) {
	provider, req := dryRunHarness(t)
	_ = provider
	writeAgentWithModelTier(t, req.Profile.AgentSources[0], "harness", "senior", "large")
	catalog, err := agents.Load(context.Background(), agents.LoadOptions{
		ProfileDirs: req.Profile.AgentSources,
	})
	if err != nil {
		t.Fatalf("agents.Load: %v", err)
	}
	got := reviewerRuntimeArtifact(req, catalog, llm.Selection{
		SelectedAgents: []llm.SelectedAgent{
			{AgentID: "harness:reviewer", Files: []string{"main.go"}},
			{AgentID: "harness:senior", Files: []string{"main.go"}},
		},
	}, "")
	if got == nil {
		t.Fatal("reviewerRuntimeArtifact = nil, want selected reviewer runtime metadata")
	}
	if runtime := got["harness:reviewer"]; runtime != (reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      "medium",
		BaselineTier:   "small",
		EffectiveTier:  "medium",
		ResolvedModel:  "claude-sonnet-4-6",
		ModelMapSource: config.ModelMapSourceBuiltIn,
	}) {
		t.Fatalf("reviewer runtime = %#v", runtime)
	}
	if runtime := got["harness:senior"]; runtime != (reviewerRuntimeResolution{
		Mode:           "tier_floor",
		FloorTier:      "large",
		BaselineTier:   "small",
		EffectiveTier:  "large",
		ResolvedModel:  "claude-opus-4-8",
		ModelMapSource: config.ModelMapSourceBuiltIn,
	}) {
		t.Fatalf("senior runtime = %#v", runtime)
	}
}

func TestDryRunReviewerModelOverrideBypassesAgentModelID(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	writeAgentModelID(t, req.Profile.AgentSources[0], "harness", "reviewer", "agent-provider-model")
	req.ReviewerModelOverride = "override-model"
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-model-id-override" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	wantModels := []string{"claude-sonnet-4-6", "override-model", "claude-sonnet-4-6"}
	for i, request := range adapter.Requests() {
		if request.Model != wantModels[i] || request.Effort != "medium" {
			t.Fatalf("request[%d] = model:%q effort:%q, want %s/medium", i, request.Model, request.Effort, wantModels[i])
		}
	}
	data, err := os.ReadFile(result.Artifacts.AgentSourcesJSON) // #nosec G304 -- test reads artifact paths returned by the pipeline under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", result.Artifacts.AgentSourcesJSON, err)
	}
	if strings.Contains(string(data), "override-model") {
		t.Fatalf("agent source artifact contains runtime override model: %s", data)
	}
}

func TestDryRunFastAppliesOnlyToReviewerAndRecordsArtifact(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.ReviewerModelOverride = "claude-opus-4-8"
	req.ReviewerFast = true
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	reviewerResult := fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4)
	reviewerResult.Response.Usage.Speed = "standard"
	adapter.Queue(reviewerResult)
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-fast" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	requests := adapter.Requests()
	if len(requests) != 3 || requests[0].Fast || !requests[1].Fast || requests[2].Fast {
		t.Fatalf("requests = %#v, want fast only on reviewer", requests)
	}
	assertReviewerRuntimeArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer", reviewerRuntimeResolution{
		Mode:          "override",
		ResolvedModel: "claude-opus-4-8",
		Fast:          true,
		FastDelivered: "standard",
	})
}

func TestReviewerFastDeliveryDegradesConservatively(t *testing.T) {
	session := func(speed string) sessionDraft {
		return sessionDraft{Response: llm.Response{Usage: llm.Usage{Speed: speed}}}
	}
	for _, tt := range []struct {
		name      string
		requested bool
		sessions  []sessionDraft
		want      string
	}{
		{name: "not requested", sessions: []sessionDraft{session("fast")}, want: ""},
		{name: "all fast", requested: true, sessions: []sessionDraft{session("fast"), session("fast")}, want: "fast"},
		{name: "standard wins", requested: true, sessions: []sessionDraft{session("fast"), session("standard")}, want: "standard"},
		{name: "unknown degrades fast", requested: true, sessions: []sessionDraft{session("fast"), session("")}, want: "unknown"},
		{name: "no sessions", requested: true, want: "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewerFastDelivery(tt.requested, tt.sessions); got != tt.want {
				t.Fatalf("reviewerFastDelivery = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDryRunFastRejectsUnsupportedRuntimeBeforeLLM(t *testing.T) {
	tests := []struct {
		name string
		llm  config.LLMConfig
		want string
	}{
		{
			name: "adapter",
			llm:  config.LLMConfig{Provider: config.LLMProviderOpenAI, Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterCodexCLI},
			want: "pipeline: --fast is unsupported for runtime openai/subscription/codex_cli: adapter has no fast-mode mechanism",
		},
		{
			name: "model",
			llm:  config.LLMConfig{Provider: config.LLMProviderAnthropic, Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterClaudeCLI},
			want: `pipeline: --fast is unsupported for runtime anthropic/subscription/claude_cli: reviewer "harness:reviewer" resolves to model "claude-sonnet-4-6"; supported models: claude-opus-4-8, claude-opus-4-7`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			req.Profile.LLM = tt.llm
			req.ReviewerFast = true
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

			_, err := dryRunForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewRunID:        func() string { return "run-fast-unsupported" },
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DryRun error = %v, want %q", err, tt.want)
			}
			if len(adapter.Requests()) != 0 {
				t.Fatalf("LLM requests = %#v, want none", adapter.Requests())
			}
		})
	}
}

func TestDryRunPrunesConfiguredRetentionBeforeFetch(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 1, 1))
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	oldLive := allocatePipelineRun(t, store, layout, "old-live", ledger.PostModeLive, fixedNow().Add(-31*24*time.Hour))
	newLive := allocatePipelineRun(t, store, layout, "new-live", ledger.PostModeLive, fixedNow().Add(-29*24*time.Hour))
	oldDryRun := allocatePipelineRun(t, store, layout, "old-dry", ledger.PostModeDryRun, fixedNow().Add(-8*24*time.Hour))
	provider.onGetPR = func() {
		if _, err := store.GetRun(ctx, oldLive.RunID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("expired live run before provider GetPR error = %v, want ErrNotFound", err)
		}
		if _, err := store.GetRun(ctx, oldDryRun.RunID); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("expired dry-run before provider GetPR error = %v, want ErrNotFound", err)
		}
		if _, err := store.GetRun(ctx, newLive.RunID); err != nil {
			t.Fatalf("fresh live run before provider GetPR error = %v, want nil", err)
		}
	}

	if _, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-1" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
		Retention:       datalifecycle.RetentionPolicy{LiveMaxAge: 30 * 24 * time.Hour},
	}, req); err != nil {
		t.Fatalf("DryRun: %v", err)
	}
}

func TestDryRunManualOnlySkipsRetentionBeforeFetch(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 1, 1))
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	oldLive := allocatePipelineRun(t, store, layout, "old-live", ledger.PostModeLive, fixedNow().Add(-365*24*time.Hour))
	oldDryRun := allocatePipelineRun(t, store, layout, "old-dry", ledger.PostModeDryRun, fixedNow().Add(-8*24*time.Hour))
	provider.onGetPR = func() {
		if _, err := store.GetRun(ctx, oldLive.RunID); err != nil {
			t.Fatalf("live run before provider GetPR error = %v, want nil", err)
		}
		if _, err := store.GetRun(ctx, oldDryRun.RunID); err != nil {
			t.Fatalf("dry-run before provider GetPR error = %v, want nil", err)
		}
	}

	if _, err := dryRunForTest(ctx, Options{
		Provider:            provider,
		Adapter:             adapter,
		Store:               store,
		Layout:              layout,
		Now:                 fixedNow,
		NewRunID:            func() string { return "run-1" },
		NewSessionRowID:     sequence("session"),
		NewFindingID:        findingSequence("finding"),
		NewActionID:         actionSequence(),
		MaxConcurrency:      1,
		RetentionManualOnly: true,
	}, req); err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if _, err := store.GetRun(ctx, oldLive.RunID); err != nil {
		t.Fatalf("live run after DryRun error = %v, want nil", err)
	}
	if _, err := store.GetRun(ctx, oldDryRun.RunID); err != nil {
		t.Fatalf("dry-run after DryRun error = %v, want nil", err)
	}
}

func TestLivePlansPendingActionsWithoutCompletingRun(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           "run-live",
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "run-live"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.Run.RunID != run.RunID || result.Run.PostMode != ledger.PostModeLive {
		t.Fatalf("run = %#v, want supplied live run", result.Run)
	}
	storedRun, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if storedRun.Outcome != nil {
		t.Fatalf("stored outcome = %#v, want incomplete until outbox", storedRun.Outcome)
	}
	if len(result.PlannedActions) != 2 {
		t.Fatalf("planned actions len = %d, want inline/submit", len(result.PlannedActions))
	}
	for _, action := range result.PlannedActions {
		if action.Status != ledger.PlannedActionPending {
			t.Fatalf("action status = %q, want pending for %#v", action.Status, action)
		}
		payload, err := action.Payload()
		if err != nil {
			t.Fatalf("live payload: %v", err)
		}
		if strings.Contains(fmt.Sprint(payload), "<!-- codereview:") {
			t.Fatalf("live payload contains marker before outbox: %#v", payload)
		}
	}
	sessions, err := store.ListSessionsForRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	if len(sessions) != 4 {
		t.Fatalf("sessions len = %d, want selection/two reviewers/rollup", len(sessions))
	}
	assertFileContains(t, result.Artifacts.RollupMarkdown, "Automated PR Review")
	assertAgentSourcesArtifact(t, result.Artifacts.AgentSourcesJSON, "harness:reviewer")
}

func TestLiveRejectsStageRuntimeOverrides(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "selection model", mutate: func(req *Request) { req.SelectionModelOverride = "bench-model" }},
		{name: "selection effort", mutate: func(req *Request) { req.SelectionEffortOverride = "high" }},
		{name: "selection prompt", mutate: func(req *Request) { req.SelectionPromptInstructions = "Use applies_when." }},
		{name: "reviewer model", mutate: func(req *Request) { req.ReviewerModelOverride = "bench-model" }},
		{name: "reviewer effort", mutate: func(req *Request) { req.ReviewerEffortOverride = "high" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, req := dryRunHarness(t)
			tt.mutate(&req)
			run := allocateLiveRun(t, store, provider, req, "run-live-override-"+strings.ReplaceAll(tt.name, " ", "-"))
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}

			_, err := liveForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req, run)
			if err == nil {
				t.Fatal("Live error = nil, want stage override rejection")
			}
			if !strings.Contains(err.Error(), "selection and reviewer overrides require dry-run review") {
				t.Fatalf("Live error = %v, want stage override rejection", err)
			}
			if len(adapter.Requests()) != 0 {
				t.Fatalf("adapter requests = %#v, want none", adapter.Requests())
			}
		})
	}
}

func TestDefaultSessionNameUsesPRProfileAndPostingIdentityOnly(t *testing.T) {
	_, req := dryRunHarness(t)
	want := "default:github.com_open-cli-collective_codereview-cli_29__home__review-bot"

	first, err := defaultSessionName(req)
	if err != nil {
		t.Fatalf("defaultSessionName: %v", err)
	}
	req.ReviewBaseSHA = strings.Repeat("a", 40)
	req.ReviewHeadSHA = strings.Repeat("b", 40)
	second, err := defaultSessionName(req)
	if err != nil {
		t.Fatalf("defaultSessionName with changed SHAs: %v", err)
	}
	if first != want || second != want {
		t.Fatalf("default session names = %q / %q, want %q", first, second, want)
	}
}

func TestDefaultSessionPersistsFromDryRunAndResumesLiveRerun(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	actionIDs := actionSequence()
	dryAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	dryAdapter.Queue(fakeLLMResult("selection-dry", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	dryAdapter.Queue(fakeLLMResult("reviewer-dry", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	dryAdapter.Queue(fakeLLMResult("rollup-dry", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	dryResult, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         dryAdapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          layout,
		Now:             fixedNow,
		NewRunID:        func() string { return "run-default-dry" },
		NewSessionRowID: sequence("session-dry"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionIDs,
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dryResult.NamedSessionCandidate == nil {
		t.Fatal("dry-run candidate = nil")
	}
	stored, err := store.GetNamedSession(ctx, dryResult.NamedSessionCandidate.Name)
	if err != nil {
		t.Fatalf("GetNamedSession after dry-run: %v", err)
	}
	if stored.ProviderSessionID != "rollup-dry" {
		t.Fatalf("stored provider session = %q, want rollup-dry", stored.ProviderSessionID)
	}

	req.Rerun = true
	run := allocateLiveRun(t, store, provider, req, "run-default-live")
	liveAdapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	liveAdapter.Queue(fakeLLMResult("selection-live", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	liveAdapter.Queue(fakeLLMResult("reviewer-live", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	liveAdapter.Queue(fakeLLMResult("rollup-live", rollupJSON("comment", []string{"live-finding-1"}), 30, 6))

	liveResult, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         liveAdapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          layout,
		Now:             fixedNow,
		NewSessionRowID: sequence("session-live"),
		NewFindingID:    findingSequence("live-finding"),
		NewActionID:     actionIDs,
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	resumes := liveAdapter.Resumes()
	if len(resumes) != 2 || resumes[0].SessionID != "rollup-dry" || resumes[1].SessionID != "selection-live" {
		t.Fatalf("live resumes = %#v, want persisted dry-run session then within-run selection", resumes)
	}
	if liveResult.NamedSessionCandidate == nil || liveResult.NamedSessionCandidate.Name != stored.Name {
		t.Fatalf("live candidate = %#v, want shared default key %q", liveResult.NamedSessionCandidate, stored.Name)
	}
}

func TestFreshSessionSkipsStoredDefaultWithoutChangingItsKey(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	name, err := defaultSessionName(req)
	if err != nil {
		t.Fatalf("defaultSessionName: %v", err)
	}
	stored := namedSessionForRequest(req, "stored-session")
	stored.Name = name
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	req.FreshSession = true
	run := allocateLiveRun(t, store, provider, req, "run-default-fresh")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-fresh", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-fresh", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-fresh", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if resumes := adapter.Resumes(); len(resumes) != 1 || resumes[0].SessionID != "selection-fresh" {
		t.Fatalf("resumes = %#v, want no stored-session resume", resumes)
	}
	if result.NamedSessionCandidate == nil || result.NamedSessionCandidate.Name != name || result.NamedSessionCandidate.ProviderSessionID != "rollup-fresh" {
		t.Fatalf("candidate = %#v, want fresh replacement for %q", result.NamedSessionCandidate, name)
	}
	gotStored, err := store.GetNamedSession(ctx, name)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if gotStored.ProviderSessionID != "stored-session" {
		t.Fatalf("stored provider session = %q, want unchanged until caller commits candidate", gotStored.ProviderSessionID)
	}
}

func TestFreshSessionSkipsStoredNamedSession(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	_, req := dryRunHarness(t)
	req.SessionName = "daily"
	req.FreshSession = true
	if err := store.UpsertNamedSession(ctx, namedSessionForRequest(req, "stored-session")); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	state, err := prepareNamedSession(ctx, Options{
		Adapter:       &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true},
		NamedSessions: store,
	}, req, true, "claude-sonnet-4-6", fixedNow())
	if err != nil {
		t.Fatalf("prepareNamedSession: %v", err)
	}
	if state.active.Name != "daily" || state.resumeID() != "" {
		t.Fatalf("fresh named state = %#v resume %q, want daily without stored resume", state, state.resumeID())
	}
}

func TestLiveNamedSessionResumesOrchestratorOnlyAndReturnsCandidate(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named")
	stored := namedSessionForRequest(req, "stored-session")
	stored.CreatedAt = fixedNow().Add(-time.Hour)
	stored.LastUsedAt = fixedNow().Add(-time.Minute)
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}

	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}

	resumes := adapter.Resumes()
	if len(resumes) != 2 {
		t.Fatalf("resumes = %#v, want selection and rollup resumes", resumes)
	}
	if resumes[0].SessionID != "stored-session" || resumes[1].SessionID != "selection-new" {
		t.Fatalf("resume session ids = %#v, want stored-session then selection-new", resumes)
	}
	requests := adapter.Requests()
	if len(requests) != 1 || !strings.Contains(requests[0].Prompt, `"schema": "findings"`) {
		t.Fatalf("fresh starts = %#v, want reviewer only", requests)
	}
	if result.NamedSessionCandidate == nil {
		t.Fatal("NamedSessionCandidate = nil, want rollup candidate")
	}
	wantCandidate := stored
	wantCandidate.ProviderSessionID = "rollup-new"
	wantCandidate.LastUsedAt = fixedNow()
	if !reflect.DeepEqual(*result.NamedSessionCandidate, wantCandidate) {
		t.Fatalf("candidate = %#v, want %#v", *result.NamedSessionCandidate, wantCandidate)
	}
	gotStored, err := store.GetNamedSession(ctx, req.SessionName)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if gotStored.ProviderSessionID != "stored-session" {
		t.Fatalf("stored provider session = %q, want unchanged stored-session", gotStored.ProviderSessionID)
	}
}

func TestLiveNamedSessionMissingRowStartsFreshAndReturnsCandidate(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-first")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "selection-new" {
		t.Fatalf("resumes = %#v, want rollup resume from fresh selection", resumes)
	}
	requests := adapter.Requests()
	if len(requests) < 1 || !requests[0].DurableSession {
		t.Fatalf("requests = %#v, want durable selection start on first named-session run", requests)
	}
	for i := 1; i < len(requests); i++ {
		if requests[i].DurableSession {
			t.Fatalf("requests[%d] = %#v, do not want durable non-selection requests", i, requests[i])
		}
	}
	if result.NamedSessionCandidate == nil {
		t.Fatal("NamedSessionCandidate = nil, want first-run candidate")
	}
	wantCandidate := namedSessionForRequest(req, "rollup-new")
	if !reflect.DeepEqual(*result.NamedSessionCandidate, wantCandidate) {
		t.Fatalf("candidate = %#v, want %#v", *result.NamedSessionCandidate, wantCandidate)
	}
	if _, err := store.GetNamedSession(ctx, req.SessionName); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetNamedSession error = %v, want pipeline not to persist candidate", err)
	}
}

func TestLiveNamedSessionScopeMismatchRefusesBeforeLLM(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ledger.NamedSession)
		wantErr string
	}{
		{name: "profile", mutate: func(s *ledger.NamedSession) { s.Profile = "other" }, wantErr: "profile mismatch"},
		{name: "provider", mutate: func(s *ledger.NamedSession) { s.Provider = "openai" }, wantErr: "provider mismatch"},
		{name: "adapter", mutate: func(s *ledger.NamedSession) { s.Adapter = "other-adapter" }, wantErr: "adapter mismatch"},
		{name: "model", mutate: func(s *ledger.NamedSession) { s.Model = "claude-opus-4-8" }, wantErr: "model mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			req.SessionName = "daily"
			run := allocateLiveRun(t, store, provider, req, "run-live-named-mismatch-"+tt.name)
			stored := namedSessionForRequest(req, "stored-session")
			tt.mutate(&stored)
			if err := store.UpsertNamedSession(ctx, stored); err != nil {
				t.Fatalf("UpsertNamedSession: %v", err)
			}
			adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}

			_, err := liveForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				NamedSessions:   store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req, run)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Live error = %v, want %q", err, tt.wantErr)
			}
			if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
				t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
			}
			gotStored, getErr := store.GetNamedSession(ctx, req.SessionName)
			if getErr != nil {
				t.Fatalf("GetNamedSession: %v", getErr)
			}
			if !reflect.DeepEqual(gotStored, stored) {
				t.Fatalf("stored named session = %#v, want unchanged %#v", gotStored, stored)
			}
		})
	}
}

func TestLiveDefaultSessionScopeMismatchStartsFresh(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ledger.NamedSession)
		wantErr string
	}{
		{name: "profile", mutate: func(s *ledger.NamedSession) { s.Profile = "other" }, wantErr: "profile mismatch"},
		{name: "provider", mutate: func(s *ledger.NamedSession) { s.Provider = "openai" }, wantErr: "provider mismatch"},
		{name: "adapter", mutate: func(s *ledger.NamedSession) { s.Adapter = "other-adapter" }, wantErr: "adapter mismatch"},
		{name: "model", mutate: func(s *ledger.NamedSession) { s.Model = "claude-opus-4-8" }, wantErr: "model mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			name, err := defaultSessionName(req)
			if err != nil {
				t.Fatalf("defaultSessionName: %v", err)
			}
			run := allocateLiveRun(t, store, provider, req, "run-live-default-mismatch-"+tt.name)
			stored := namedSessionForRequest(req, "stored-session")
			stored.Name = name
			tt.mutate(&stored)
			if err := store.UpsertNamedSession(ctx, stored); err != nil {
				t.Fatalf("UpsertNamedSession: %v", err)
			}
			adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
			adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
			adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
			adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))
			var warnings bytes.Buffer

			result, err := liveForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				NamedSessions:   store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Warnings:        &warnings,
				Now:             fixedNow,
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				MaxConcurrency:  1,
			}, req, run)
			if err != nil {
				t.Fatalf("Live: %v", err)
			}
			if len(adapter.Resumes()) != 1 || adapter.Resumes()[0].SessionID != "selection-new" {
				t.Fatalf("resumes = %#v, want only within-invocation rollup resume", adapter.Resumes())
			}
			if !strings.Contains(warnings.String(), tt.wantErr) || !strings.Contains(warnings.String(), "starting fresh") {
				t.Fatalf("warnings = %q, want %q and fresh fallback", warnings.String(), tt.wantErr)
			}
			if result.NamedSessionCandidate == nil || result.NamedSessionCandidate.Name != name || result.NamedSessionCandidate.ProviderSessionID != "rollup-new" {
				t.Fatalf("candidate = %#v, want fresh rollup session", result.NamedSessionCandidate)
			}
		})
	}
}

func TestLiveNamedSessionResumeFailureLeavesStoredSessionUnchanged(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-resume-failure")
	stored := namedSessionForRequest(req, "stored-session")
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	resumeErr := errors.New("resume failed")
	adapter.Queue(llm.FakeResult{StartErr: resumeErr})

	_, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if !errors.Is(err, resumeErr) {
		t.Fatalf("Live error = %v, want resume failure", err)
	}
	if resumes := adapter.Resumes(); len(resumes) != 1 || resumes[0].SessionID != "stored-session" {
		t.Fatalf("resumes = %#v, want one stored-session resume", resumes)
	}
	gotStored, err := store.GetNamedSession(ctx, req.SessionName)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if !reflect.DeepEqual(gotStored, stored) {
		t.Fatalf("stored named session = %#v, want unchanged %#v", gotStored, stored)
	}
}

func TestLiveNamedSessionCrossHostWarnsAndContinues(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-host")
	stored := namedSessionForRequest(req, "stored-session")
	stored.Host = "github.enterprise.example"
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	var warnings bytes.Buffer

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Warnings:        &warnings,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.NamedSessionCandidate == nil || result.NamedSessionCandidate.Host != req.PRRef.Host {
		t.Fatalf("candidate = %#v, want active host", result.NamedSessionCandidate)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 2 || resumes[0].SessionID != "stored-session" || resumes[1].SessionID != "selection-new" {
		t.Fatalf("resumes = %#v, want stored-session then selection-new", resumes)
	}
	if !strings.Contains(warnings.String(), "host mismatch") || !strings.Contains(warnings.String(), "continuing") {
		t.Fatalf("warnings = %q, want host mismatch warning", warnings.String())
	}
}

func TestLiveNamedSessionUnsupportedResumeStartsFreshAndReturnsCandidate(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-unsupported")
	stored := namedSessionForRequest(req, "stored-session")
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-fresh", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-fresh", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	var warnings bytes.Buffer

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Warnings:        &warnings,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(adapter.Resumes()) != 0 {
		t.Fatalf("resumes = %#v, want none for unsupported adapter", adapter.Resumes())
	}
	if len(adapter.Requests()) != 3 {
		t.Fatalf("starts = %d, want selection/reviewer/rollup", len(adapter.Requests()))
	}
	for i, request := range adapter.Requests() {
		if request.DurableSession {
			t.Fatalf("requests[%d] = %#v, do not want durable sessions for unsupported adapter", i, request)
		}
	}
	if result.NamedSessionCandidate == nil || result.NamedSessionCandidate.ProviderSessionID != "rollup-fresh" {
		t.Fatalf("candidate = %#v, want rollup-fresh", result.NamedSessionCandidate)
	}
	if !strings.Contains(warnings.String(), "does not support resume") {
		t.Fatalf("warnings = %q, want unsupported resume warning", warnings.String())
	}
}

func TestLiveNamedSessionLegacyStoredRowStartsFreshAndReturnsDurableCandidate(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-legacy")
	stored := namedSessionForRequest(req, "stored-session")
	stored.Adapter = "codex_cli"
	stored.DurableSession = false
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "codex_cli", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	var warnings bytes.Buffer

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Warnings:        &warnings,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(adapter.Resumes()) != 1 || adapter.Resumes()[0].SessionID != "selection-new" {
		t.Fatalf("resumes = %#v, want only rollup resume after fresh durable selection", adapter.Resumes())
	}
	requests := adapter.Requests()
	if len(requests) < 1 || !requests[0].DurableSession {
		t.Fatalf("requests = %#v, want durable fresh selection start", requests)
	}
	if result.NamedSessionCandidate == nil || !result.NamedSessionCandidate.DurableSession {
		t.Fatalf("candidate = %#v, want durable named-session candidate", result.NamedSessionCandidate)
	}
	if !strings.Contains(warnings.String(), "predates durable resume support") {
		t.Fatalf("warnings = %q, want legacy durability warning", warnings.String())
	}
}

func TestLiveNamedSessionLegacyNonCodexRowStillResumes(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-legacy-claude")
	stored := namedSessionForRequest(req, "stored-session")
	stored.Adapter = "claude_cli"
	stored.DurableSession = false
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "claude_cli", SupportsResumeValue: true}
	adapter.Queue(fakeLLMResult("selection-new", selectionJSON("harness:reviewer", "main.go"), 10, 2))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 20, 4))
	adapter.Queue(fakeLLMResult("rollup-new", rollupJSON("comment", []string{"finding-1"}), 30, 6))
	var warnings bytes.Buffer

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Warnings:        &warnings,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 2 || resumes[0].SessionID != "stored-session" || resumes[1].SessionID != "selection-new" {
		t.Fatalf("resumes = %#v, want stored-session then selection-new", resumes)
	}
	if len(adapter.Requests()) != 1 || !strings.Contains(adapter.Requests()[0].Prompt, `"schema": "findings"`) {
		t.Fatalf("fresh starts = %#v, want reviewer only", adapter.Requests())
	}
	if result.NamedSessionCandidate == nil || result.NamedSessionCandidate.ProviderSessionID != "rollup-new" || !result.NamedSessionCandidate.DurableSession {
		t.Fatalf("candidate = %#v, want durable rollup-new candidate", result.NamedSessionCandidate)
	}
	if strings.Contains(warnings.String(), "predates durable resume support") {
		t.Fatalf("warnings = %q, do not want legacy durability warning", warnings.String())
	}
}

func TestLiveNamedSessionNoDiffLeavesCandidateEmpty(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff = gitprovider.UnifiedDiff{}
	req.SessionName = "daily"
	run := allocateLiveRun(t, store, provider, req, "run-live-named-nodiff")
	stored := namedSessionForRequest(req, "stored-session")
	if err := store.UpsertNamedSession(ctx, stored); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	var warnings bytes.Buffer

	result, err := liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		NamedSessions:   store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Warnings:        &warnings,
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if result.NamedSessionCandidate != nil {
		t.Fatalf("NamedSessionCandidate = %#v, want nil", result.NamedSessionCandidate)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
	gotStored, err := store.GetNamedSession(ctx, req.SessionName)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if !reflect.DeepEqual(gotStored, stored) {
		t.Fatalf("stored named session = %#v, want unchanged %#v", gotStored, stored)
	}
	if !strings.Contains(warnings.String(), "no orchestrator session was produced") {
		t.Fatalf("warnings = %q, want no-orchestrator warning", warnings.String())
	}
}

func TestLiveMarksRunIncompleteAfterBlockingLLMTaskError(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           "run-live-failed",
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "run-live-failed"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}

	_, err = liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if err == nil || !strings.Contains(err.Error(), "no queued result") {
		t.Fatalf("Live error = %v, want fake LLM planning error", err)
	}
	plannerErr := err
	storedRun, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if storedRun.Outcome != nil {
		t.Fatalf("stored outcome = %#v, want pipeline to leave live outcome ownership to reviewrun", storedRun.Outcome)
	}
	if got := ClassifyFailure(plannerErr); got != FailureDurableBlocking {
		t.Fatalf("failure kind = %v, want durable blocking", got)
	}
}

func TestLiveLeavesRunIncompleteAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           "run-live-canceled",
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), "run-live-canceled"),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}

	_, err = liveForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req, run)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Live error = %v, want context.Canceled", err)
	}
	storedRun, err := store.GetRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if storedRun.Outcome != nil {
		t.Fatalf("stored outcome = %#v, want incomplete after cancellation", storedRun.Outcome)
	}
}

func TestDryRunNoResolveThreadsKeepsSummaryReplyOnly(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	human := gitprovider.Identity{Login: "human", ID: "human-id"}
	provider.threads = []gitprovider.InlineThread{
		markedReviewThread(t, "thread-1", "main.go", 2, req.PostingIdentity, human),
	}
	provider.caps.ThreadResolution = true
	req.NoResolveThreads = true
	req.Profile.AgentSources = nil
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("dossier-summary-session", discussionSummaryJSON(nil, nil), 1, 1))
	adapter.Queue(fakeLLMResult("selection-session", `{
		"schema_version": 1,
		"selected_agents": [],
		"thread_actions": [],
		"reasoning": "thread cleanup"
	}`, 1, 1))
	adapter.Queue(fakeLLMResult("thread-analysis-session", `{
		"schema_version": 1,
		"thread_id": "thread-1",
		"decision": "summarize",
		"reply_body": "",
		"summary": "Summary only",
		"resolve": true,
		"rationale": "safe"
	}`, 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("approve", nil), 1, 1))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-threads" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.EffectiveCaps.ThreadResolution {
		t.Fatal("EffectiveCaps.ThreadResolution = true, want disabled by request")
	}
	requests := adapter.Requests()
	if len(requests) != 4 {
		t.Fatalf("adapter requests = %d, want dossier/selection/thread-analysis/rollup", len(requests))
	}
	if !strings.Contains(requests[1].Prompt, `"status": "pending_human_reply"`) || strings.Contains(requests[1].Prompt, "<!-- codereview:") {
		t.Fatalf("selection prompt did not use sanitized normalized thread context:\n%s", requests[1].Prompt)
	}
	meta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(result.Artifacts), "thread-analysis-thread-1")
	if err != nil || !ok {
		t.Fatalf("read thread analysis metadata ok=%t err=%v", ok, err)
	}
	if meta.Status != llmTaskStatusSucceeded || meta.Phase != string(stagemodel.StageThreadAnalysis) {
		t.Fatalf("thread analysis metadata = %#v, want succeeded thread_analysis", meta)
	}
	var sawReply, sawResolve bool
	for _, action := range result.Plan.Actions {
		switch action.Kind {
		case reviewplan.ActionKindThreadReply:
			sawReply = true
		case reviewplan.ActionKindResolveThread:
			sawResolve = true
		case reviewplan.ActionKindInlineComment, reviewplan.ActionKindRollupComment, reviewplan.ActionKindSubmitReview:
		}
	}
	if !sawReply || sawResolve {
		t.Fatalf("actions = %#v, want thread reply and no resolve action", result.Plan.Actions)
	}
}

func TestDryRunMultiAgentSessionsMapFindingsToReviewerSessions(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	writeAgent(t, dir, "harness", "beta", "beta desc", "Review beta files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	provider.diff.Raw = smallDiff("main.go") + smallDiff("other.go")
	adapter := &promptAwareAdapter{}

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-multi-agent" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  2,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	requests := adapter.Requests()
	if len(requests) != 4 {
		t.Fatalf("adapter requests = %d, want selection, two reviewers, rollup", len(requests))
	}
	var reviewerPrompts int
	var selectionPrompts int
	for _, request := range requests {
		assertPromptOmitsLocalAgentSourceProvenance(t, request.Prompt, result.Catalog.Sources)
		if !strings.Contains(request.Prompt, `"output_contract"`) {
			t.Fatalf("prompt missing output contract: %s", request.Prompt)
		}
		if strings.Contains(request.Prompt, `"schema": "selection"`) {
			selectionPrompts++
			if !strings.Contains(request.Prompt, `"agent_id"`) ||
				!strings.Contains(request.Prompt, `"thread_actions"`) ||
				!strings.Contains(request.Prompt, `"schema_version"`) {
				t.Fatalf("selection prompt missing output schema fields: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"applies_when"`) {
				t.Fatalf("selection prompt missing applies_when routing metadata: %s", request.Prompt)
			}
			for _, forbidden := range []string{"Review alpha files.", "Review beta files.", `"prompt"`, `"provenance"`, `"overridden"`} {
				if strings.Contains(request.Prompt, forbidden) {
					t.Fatalf("selection prompt leaked reviewer execution instructions %q: %s", forbidden, request.Prompt)
				}
			}
		}
		if strings.Contains(request.Prompt, `"schema": "findings"`) {
			reviewerPrompts++
			if !strings.Contains(request.Prompt, `"agent"`) || !strings.Contains(request.Prompt, `"assignment"`) || !strings.Contains(request.Prompt, `"dossier"`) || !strings.Contains(request.Prompt, `"workbench"`) {
				t.Fatalf("reviewer prompt missing checkout-native context: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"file_path"`) ||
				!strings.Contains(request.Prompt, `"inspected_files"`) ||
				!strings.Contains(request.Prompt, `"skipped_files"`) ||
				!strings.Contains(request.Prompt, `"anchor"`) ||
				!strings.Contains(request.Prompt, `"Do not provide finding_id`) {
				t.Fatalf("reviewer prompt missing output schema fields: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"prompt"`) {
				t.Fatalf("reviewer prompt missing agent prompt field: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, "Review alpha files.") && !strings.Contains(request.Prompt, "Review beta files.") {
				t.Fatalf("reviewer prompt missing prompt.md body text: %s", request.Prompt)
			}
			for _, forbidden := range []string{`"diff"`, `"base_content"`, `"head_content"`, `"needs_full_file_content"`, `"provenance"`, `"overridden"`, `"model_tier"`, `"model_id"`, `"effort"`} {
				if strings.Contains(request.Prompt, forbidden) {
					t.Fatalf("reviewer prompt leaked unsupported or stuffed field %q: %s", forbidden, request.Prompt)
				}
			}
		}
		if strings.Contains(request.Prompt, `"schema": "rollup"`) &&
			(!strings.Contains(request.Prompt, `"review_event"`) ||
				!strings.Contains(request.Prompt, `"dedupe_log"`) ||
				!strings.Contains(request.Prompt, `"ordered_findings"`)) {
			t.Fatalf("rollup prompt missing output schema fields: %s", request.Prompt)
		}
		if strings.Contains(request.Prompt, `"schema": "rollup"`) {
			if strings.Contains(request.Prompt, `"anchor"`) {
				t.Fatalf("rollup prompt leaked finding anchors: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"location"`) {
				t.Fatalf("rollup prompt missing finding location context: %s", request.Prompt)
			}
			if !strings.Contains(request.Prompt, `"Use finding location only to distinguish findings during dedupe; do not include finding fields such as severity, file_path, location, body, anchor, or finding_id in the response."`) {
				t.Fatalf("rollup prompt missing explicit finding-object rejection: %s", request.Prompt)
			}
		}
	}
	if reviewerPrompts != 2 {
		t.Fatalf("reviewer prompts = %d, want 2", reviewerPrompts)
	}
	if selectionPrompts != 1 {
		t.Fatalf("selection prompts = %d, want 1", selectionPrompts)
	}

	storedFindings, err := store.ListFindings(ctx, "run-multi-agent")
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	sessionAgents := map[string]string{}
	for _, session := range result.Sessions {
		if session.AgentID != nil {
			sessionAgents[session.SessionRowID] = *session.AgentID
		}
	}
	got := map[string]string{}
	for _, finding := range storedFindings {
		got[finding.FilePath] = sessionAgents[finding.SessionRowID]
	}
	want := map[string]string{
		"main.go":  "harness:alpha",
		"other.go": "harness:beta",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finding session agents = %#v, want %#v", got, want)
	}
}

func TestDryRunPlanSummaryNamesWorkstreamsInSelectionOrder(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "alpha", "alpha desc", "Review alpha files.")
	writeAgent(t, dir, "harness", "beta", "beta desc", "Review beta files.")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{dir}
	req.ToolVersion = "0.0.0-test"
	provider.diff.Raw = smallDiff("main.go") + smallDiff("other.go")

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         &promptAwareAdapter{},
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-summary" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  2,
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	summary := result.Plan.Summary
	var workstreamNames []string
	for _, workstream := range summary.Run.Workstreams {
		workstreamNames = append(workstreamNames, workstream.Name)
	}
	wantNames := []string{"orchestrator-selection", "harness:alpha", "harness:beta", "repo:guidance", "orchestrator-rollup"}
	if !reflect.DeepEqual(workstreamNames, wantNames) {
		t.Fatalf("workstream names = %#v, want %#v", workstreamNames, wantNames)
	}
	if !reflect.DeepEqual(summary.Run.SelectedReviewers, []string{"harness:alpha", "harness:beta", "repo:guidance"}) {
		t.Fatalf("selected reviewers = %#v", summary.Run.SelectedReviewers)
	}
	if summary.Run.ToolVersion != "0.0.0-test" || summary.Run.PostingIdentity == "" {
		t.Fatalf("run summary identity = %#v", summary.Run)
	}
	if summary.Run.WallDurationMS == nil {
		t.Fatalf("wall duration missing: %#v", summary.Run)
	}
	reviewerCounts := map[string]int{}
	for _, reviewer := range summary.Reviewers {
		reviewerCounts[reviewer.Name] = reviewer.Findings
	}
	if reviewerCounts["harness:alpha"] != 1 || reviewerCounts["harness:beta"] != 1 {
		t.Fatalf("reviewer counts = %#v, want one finding each", summary.Reviewers)
	}
	for _, want := range []string{"| Reviewer | Findings |", "Per-workstream usage", "| orchestrator-selection |"} {
		if !strings.Contains(result.Plan.RollupMarkdown, want) {
			t.Fatalf("rollup markdown missing %q:\n%s", want, result.Plan.RollupMarkdown)
		}
	}
}

func TestDryRunUsagePopulatesRollupAndLedgerSessions(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	req.ToolVersion = "0.0.0-test"
	systemTemp := filepath.Join(t.TempDir(), "system-temp")
	if err := os.MkdirAll(systemTemp, 0o700); err != nil {
		t.Fatalf("mkdir system temp: %v", err)
	}
	t.Setenv("TMPDIR", systemTemp)
	adapter := &providerOriginUsageAdapter{name: "codex_cli"}
	adapter.Queue(newCodexUsageScriptAdapter(t, "selection-session", selectionJSON("harness:reviewer", "main.go"), llm.Usage{
		TokensIn:  intPtr(25475),
		TokensOut: intPtr(812),
		CacheRead: intPtr(19712),
	}))
	adapter.Queue(newClaudeTranscriptScriptAdapter(t, "reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Finding"), llm.Usage{
		TokensIn:    intPtr(13),
		TokensOut:   intPtr(4069),
		CacheRead:   intPtr(861774),
		CacheCreate: intPtr(180377),
	}))
	adapter.Queue(newCodexUsageScriptAdapter(t, "rollup-session", rollupJSON("comment", []string{"finding-1"}), llm.Usage{
		TokensIn:  intPtr(11324),
		TokensOut: intPtr(129),
		CacheRead: intPtr(4480),
	}))

	result, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-usage" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
	}, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	workstreamByName := map[string]reviewplan.WorkstreamUsage{}
	for _, workstream := range result.Plan.Summary.Run.Workstreams {
		workstreamByName[workstream.Name] = workstream
	}
	selection := workstreamByName[orchestratorSelectionStage]
	if selection.TokensIn == nil || *selection.TokensIn != 25475 ||
		selection.TokensOut == nil || *selection.TokensOut != 812 ||
		selection.CacheRead == nil || *selection.CacheRead != 19712 {
		t.Fatalf("selection workstream = %#v, want Codex usage values", selection)
	}
	reviewer := workstreamByName["harness:reviewer"]
	if reviewer.TokensIn == nil || *reviewer.TokensIn != 13 ||
		reviewer.TokensOut == nil || *reviewer.TokensOut != 4069 ||
		reviewer.CacheRead == nil || *reviewer.CacheRead != 861774 ||
		reviewer.CacheCreate == nil || *reviewer.CacheCreate != 180377 {
		t.Fatalf("reviewer workstream = %#v, want Claude-style cache values", reviewer)
	}
	assertRollupUsageRow(t, result.Artifacts.RollupMarkdown, orchestratorSelectionStage, false)
	assertRollupUsageRow(t, result.Artifacts.RollupMarkdown, "harness:reviewer", true)
	assertRollupUsageRow(t, result.Artifacts.RollupMarkdown, orchestratorRollupStage, false)
	sessions, err := store.ListSessionsForRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("ListSessionsForRun: %v", err)
	}
	sessionByProviderID := map[string]ledger.Session{}
	for _, session := range sessions {
		sessionByProviderID[session.ProviderSessionID] = session
	}
	if got := sessionByProviderID["selection-session"]; got.TokensIn == nil || *got.TokensIn != 25475 ||
		got.TokensOut == nil || *got.TokensOut != 812 ||
		got.CacheRead == nil || *got.CacheRead != 19712 {
		t.Fatalf("persisted selection session = %#v, want parsed Codex usage", got)
	}
	if got := sessionByProviderID["reviewer-session"]; got.TokensIn == nil || *got.TokensIn != 13 ||
		got.TokensOut == nil || *got.TokensOut != 4069 ||
		got.CacheRead == nil || *got.CacheRead != 861774 ||
		got.CacheCreate == nil || *got.CacheCreate != 180377 {
		t.Fatalf("persisted reviewer session = %#v, want transcript-derived Claude usage", got)
	}
	if got := sessionByProviderID["rollup-session"]; got.TokensIn == nil || *got.TokensIn != 11324 ||
		got.TokensOut == nil || *got.TokensOut != 129 ||
		got.CacheRead == nil || *got.CacheRead != 4480 {
		t.Fatalf("persisted rollup session = %#v, want parsed Codex rollup usage", got)
	}
}

func TestSharedWorkstreamModel(t *testing.T) {
	ws := func(name, model string) reviewplan.WorkstreamUsage {
		return reviewplan.WorkstreamUsage{Name: name, Model: model}
	}
	cases := []struct {
		name        string
		workstreams []reviewplan.WorkstreamUsage
		want        string
	}{
		{"all same", []reviewplan.WorkstreamUsage{ws("a:x", "sonnet"), ws("b:y", "sonnet")}, "sonnet"},
		{
			"orchestrators excluded so reviewer model is the headline",
			[]reviewplan.WorkstreamUsage{
				ws("orchestrator-selection", "sonnet"),
				ws("policies:conventions", "opus"),
				ws("structure:repo-health", "opus"),
				ws("orchestrator-rollup", "sonnet"),
			},
			"opus",
		},
		{
			"mixed reviewer models are joined in first-seen order",
			[]reviewplan.WorkstreamUsage{ws("a:x", "opus"), ws("b:y", "sonnet")},
			"opus, sonnet",
		},
		{"empty reviewer model is skipped", []reviewplan.WorkstreamUsage{ws("a:x", ""), ws("b:y", "sonnet")}, "sonnet"},
		{
			"falls back to orchestrator model when there are no reviewers",
			[]reviewplan.WorkstreamUsage{ws("orchestrator-selection", "sonnet")},
			"sonnet",
		},
		{"none", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharedWorkstreamModel(tc.workstreams); got != tc.want {
				t.Fatalf("sharedWorkstreamModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkstreamUsageEstimatesCostWhenAdapterReportsNone(t *testing.T) {
	in, out := 1_000_000, 1_000_000

	// Known model, adapter reported no cost → estimate is filled and marked.
	draft := sessionDraft{
		Model:    "claude-sonnet-4-6",
		Response: llm.Response{Usage: llm.Usage{TokensIn: &in, TokensOut: &out}},
	}
	w := workstreamUsage("policies:conventions", draft)
	if w.CostUSD == nil || !w.CostEstimated {
		t.Fatalf("expected estimated cost; got CostUSD=%v estimated=%v", w.CostUSD, w.CostEstimated)
	}
	if want := 18.0; *w.CostUSD < want-1e-6 || *w.CostUSD > want+1e-6 { // 1M*$3 + 1M*$15
		t.Fatalf("cost = %v, want %v", *w.CostUSD, want)
	}

	// Unknown model (any agent's model) → no estimate, cost stays unavailable.
	draft.Model = "vendor/unknown-model"
	w = workstreamUsage("x:y", draft)
	if w.CostUSD != nil || w.CostEstimated {
		t.Fatalf("unknown model should not be estimated; got CostUSD=%v estimated=%v", w.CostUSD, w.CostEstimated)
	}

	// Adapter reported a real cost → passes through, not marked estimated.
	realCost := 9.99
	draft.Model = "claude-sonnet-4-6"
	draft.Response.Usage.CostUSD = &realCost
	w = workstreamUsage("z:w", draft)
	if w.CostUSD == nil || *w.CostUSD != realCost || w.CostEstimated {
		t.Fatalf("real cost should pass through unmarked; got CostUSD=%v estimated=%v", w.CostUSD, w.CostEstimated)
	}
}

func TestBuildRunSummaryWorkstreamBoundaries(t *testing.T) {
	agentID := "harness:alpha"
	inputs := planRunInputs{
		hasRun:    true,
		selection: sessionDraft{Adapter: "fake", Model: "sonnet", Response: llm.Response{DurationMS: 0}},
		reviewers: []sessionDraft{{RowID: "row-1", AgentID: &agentID, Model: "sonnet", Response: llm.Response{DurationMS: 25}}},
		rollup:    sessionDraft{Model: "sonnet", StartedAt: fixedNow(), CompletedAt: fixedNow().Add(2 * time.Second)},
		selectedAgents: []llm.SelectedAgent{
			{AgentID: agentID},
			{AgentID: "harness:missing-draft"},
		},
		findingSessions: map[review.FindingID]string{"f-1": "row-1", "f-2": "row-unknown"},
		startedAt:       fixedNow(),
	}
	summary, findingReviewers := Options{Now: fixedNow}.buildRunSummary(Request{ToolVersion: "t"}, inputs)

	var names []string
	for _, workstream := range summary.Workstreams {
		names = append(names, workstream.Name)
	}
	want := []string{"orchestrator-selection", agentID, "orchestrator-rollup"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("workstreams = %#v, want missing-draft agent skipped: %#v", names, want)
	}
	if !reflect.DeepEqual(summary.SelectedReviewers, []string{agentID, "harness:missing-draft"}) {
		t.Fatalf("selected reviewers = %#v, must keep the draft-less agent", summary.SelectedReviewers)
	}
	if summary.Workstreams[0].DurationMS != nil {
		t.Fatalf("zero duration must render unavailable, got %v", *summary.Workstreams[0].DurationMS)
	}
	if summary.Workstreams[1].DurationMS == nil || *summary.Workstreams[1].DurationMS != 25 {
		t.Fatalf("reported duration lost: %#v", summary.Workstreams[1])
	}
	if summary.Workstreams[2].DurationMS == nil || *summary.Workstreams[2].DurationMS != 2000 {
		t.Fatalf("start/complete fallback duration missing: %#v", summary.Workstreams[2])
	}
	if !reflect.DeepEqual(findingReviewers, map[review.FindingID]string{"f-1": agentID}) {
		t.Fatalf("finding reviewers = %#v, want unknown session unattributed", findingReviewers)
	}
}

func TestDryRunRejectsUnsafeProfileAgentSourcesBeforeRunAllocation(t *testing.T) {
	tests := []struct {
		name       string
		source     func(t *testing.T) (string, string)
		wantDetail string
	}{
		{name: "relative", source: relativeAgentSource, wantDetail: "relative"},
		{name: "temp", source: tempAgentSource, wantDetail: "OS temp"},
		{name: "same invocation worktree", source: gitWorktreeAgentSource, wantDetail: "current invocation worktree"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			source, invocationRoot := tt.source(t)
			req.Profile.AgentSources = []string{source}

			_, err := dryRunForTest(ctx, Options{
				Provider: provider,
				Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
				Store:    store,
				Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:      fixedNow,
				ResolveRepoRoot: func(context.Context) (string, error) {
					return invocationRoot, nil
				},
				NewRunID: func() string {
					t.Fatal("NewRunID called before unsafe source rejection")
					return ""
				},
			}, req)
			if !errors.Is(err, agents.ErrUnsafeSource) || !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("DryRun error = %v, want ErrUnsafeSource with %q", err, tt.wantDetail)
			}
		})
	}
}

func TestDryRunAllowsSiblingGitCatalogOutsideInvocationRoot(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.diff.Raw = ""
	source, _ := siblingGitCatalogSource(t)
	req.Profile.AgentSources = []string{source}

	result, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:    store,
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:      fixedNow,
		ResolveRepoRoot: func(context.Context) (string, error) {
			return provider.fixtureRepoDir, nil
		},
		NewRunID: func() string { return "run-allow-sibling" },
	}, req)
	if err != nil {
		t.Fatalf("DryRun sibling catalog: %v", err)
	}
	if result.Artifacts.Dir == "" {
		t.Fatal("artifacts dir empty, want allocated dry-run artifacts")
	}
}

func TestDryRunRejectsSameRootProfileAgentSourceWithRealGitResolver(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req := dryRunHarness(t)
	provider.fixtureRepoDir = ""
	workspace := t.TempDir()
	trustCurrentTempFixtures(t)
	repoRoot := filepath.Join(workspace, "review-repo")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll review repo: %v", err)
	}
	initGitRepoForPipelineTest(t, repoRoot)
	source := filepath.Join(repoRoot, "nested", "agents")
	writeAgent(t, source, "harness", "reviewer", "reviewer desc", "Review carefully.")
	req.Profile.AgentSources = []string{source}
	t.Chdir(repoRoot)

	_, err := dryRunForTest(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		Store:    store,
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:      fixedNow,
		NewRunID: func() string {
			t.Fatal("NewRunID called before unsafe source rejection")
			return ""
		},
	}, req)
	if !errors.Is(err, agents.ErrUnsafeSource) || !strings.Contains(err.Error(), "current invocation worktree") {
		t.Fatalf("DryRun real resolver error = %v, want ErrUnsafeSource with invocation worktree detail", err)
	}
}

func TestPrepareSelectionContextRejectsSameRootProfileSourceDuringLoad(t *testing.T) {
	ctx := context.Background()
	provider, req := dryRunHarness(t)
	source, invocationRoot := gitWorktreeAgentSource(t)
	req.Profile.AgentSources = []string{source}

	_, err := prepareSelectionContext(ctx, Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{NameValue: "fake-llm"},
		ResolveRepoRoot: func(context.Context) (string, error) {
			return invocationRoot, nil
		},
	}, selectionSetupRequest{
		PRRef:           req.PRRef,
		Profile:         req.Profile,
		PostingIdentity: req.PostingIdentity,
		ResolveArtifacts: func(gitprovider.PR) (ArtifactPaths, error) {
			return ArtifactPathsFromDir(t.TempDir()), nil
		},
	})
	if !errors.Is(err, agents.ErrUnsafeSource) || !strings.Contains(err.Error(), "current invocation worktree") {
		t.Fatalf("prepareSelectionContext error = %v, want ErrUnsafeSource with invocation worktree detail", err)
	}
}

func TestDryRunMarksRunFailedAfterPostAllocationError(t *testing.T) {
	ctx := context.Background()
	inner := openPipelineStore(t)
	defer closeStore(t, inner)
	storeErr := errors.New("insert planned action failed")
	store := &failingStore{Store: inner, insertPlannedActionErr: storeErr}
	provider, req := dryRunHarness(t)
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(fakeLLMResult("selection-session", selectionJSON("harness:reviewer", "main.go"), 1, 1))
	adapter.Queue(fakeLLMResult("reviewer-session", findingsJSON("harness:reviewer", "main.go", "major", 2, "Fix this"), 1, 1))
	adapter.Queue(fakeLLMResult("rollup-session", rollupJSON("comment", []string{"finding-1"}), 1, 1))

	_, err := dryRunForTest(ctx, Options{
		Provider:        provider,
		Adapter:         adapter,
		Store:           store,
		Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
		Now:             fixedNow,
		NewRunID:        func() string { return "run-failed-after-allocation" },
		NewSessionRowID: sequence("session"),
		NewFindingID:    findingSequence("finding"),
		NewActionID:     actionSequence(),
		MaxConcurrency:  1,
	}, req)
	if !errors.Is(err, storeErr) {
		t.Fatalf("DryRun error = %v, want planned-action failure", err)
	}
	run, err := inner.GetRun(ctx, "run-failed-after-allocation")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Outcome == nil || *run.Outcome != ledger.OutcomeFailed {
		t.Fatalf("run outcome = %#v, want failed", run.Outcome)
	}
}

func TestDryRunRejectsSelfReviewWhenReviewerCredentialsMatchAuthor(t *testing.T) {
	provider, req := dryRunHarness(t)
	req.Profile.ReviewerCredentials = &config.ReviewerCredentials{AuthMode: config.GitAuthModePAT, Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/reviewer"}}
	req.PostingIdentity = provider.pr.Author
	adapter := &llm.FakeAdapter{QuotaErr: errors.New("quota should not be called")}

	_, err := DryRun(context.Background(), Options{
		Provider: provider,
		Adapter:  adapter,
		Store:    &noopStore{},
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
	}, req)
	if err == nil {
		t.Fatal("DryRun error = nil, want self-review guard")
	}
	if !strings.Contains(err.Error(), "--allow-self-review") {
		t.Fatalf("DryRun error = %v, want allow-self-review guidance", err)
	}
	if provider.diffCalls != 0 || provider.threadCalls != 0 || len(provider.treeCalls) != 0 {
		t.Fatalf("provider side effects = diff:%d threads:%d tree:%#v, want early self-review rejection before diff/thread/catalog work", provider.diffCalls, provider.threadCalls, provider.treeCalls)
	}
	if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
		t.Fatalf("adapter was invoked: starts=%#v resumes=%#v", adapter.Requests(), adapter.Resumes())
	}
}

func TestBuildReviewerCoverageStatuses(t *testing.T) {
	broad := buildReviewerCoverage(
		[]llm.SelectedAgent{{AgentID: "harness:broad", Files: []string{"main.go", "other.go"}}},
		[]llm.Findings{{AgentID: "harness:broad", InspectedFiles: []string{"main.go", "other.go"}}},
		nil,
		[]string{"main.go", "other.go"},
	)
	if len(broad) != 1 || broad[0].Status != reviewerCoverageCompleteBroad {
		t.Fatalf("broad coverage = %#v, want complete broad", broad)
	}

	selected := []llm.SelectedAgent{
		{AgentID: "harness:scoped", Files: []string{"db.sql"}, AllowedFiles: []string{"db.sql"}},
		{AgentID: "harness:skipped", Files: []string{"api.go"}, AllowedFiles: []string{"api.go"}},
		{AgentID: "harness:failed", Files: []string{"worker.go"}, AllowedFiles: []string{"worker.go"}},
	}
	results := []llm.Findings{
		{AgentID: "harness:scoped", InspectedFiles: []string{"db.sql"}, Constraints: []string{"SQL-only review"}},
		{AgentID: "harness:skipped", InspectedFiles: []string{"api.go"}, SkippedFiles: []string{"api.go"}},
	}
	failures := []ReviewerFailure{{AgentID: "harness:failed", Error: "model failed"}}

	got := buildReviewerCoverage(selected, results, failures, []string{"api.go", "db.sql", "unassigned.go", "worker.go"})
	byAgent := map[string]reviewplan.ReviewerCoverageSummary{}
	for _, entry := range got {
		byAgent[entry.AgentID] = entry
	}

	if byAgent["harness:scoped"].Status != reviewerCoverageCompleteConstrained {
		t.Fatalf("scoped coverage = %#v", byAgent["harness:scoped"])
	}
	if byAgent["harness:skipped"].Status != reviewerCoverageIncompleteSkipped {
		t.Fatalf("skipped coverage = %#v", byAgent["harness:skipped"])
	}
	if byAgent["harness:failed"].Status != reviewerCoverageIncompleteFailed || byAgent["harness:failed"].Diagnostic != "model failed" {
		t.Fatalf("failed coverage = %#v", byAgent["harness:failed"])
	}
	if byAgent["unassigned"].Status != reviewerCoverageIncompleteUnassigned ||
		!reflect.DeepEqual(byAgent["unassigned"].SkippedFiles, []string{"unassigned.go"}) {
		t.Fatalf("unassigned coverage = %#v", byAgent["unassigned"])
	}
}

func TestBuildReviewerCoverageMarksAssignedScopeMissing(t *testing.T) {
	got := buildReviewerCoverage(
		[]llm.SelectedAgent{{AgentID: "harness:reviewer", AllowedFiles: []string{"main.go", "other.go"}}},
		[]llm.Findings{{AgentID: "harness:reviewer", InspectedFiles: []string{"main.go"}}},
		nil,
		[]string{"main.go", "other.go"},
	)
	if len(got) != 1 {
		t.Fatalf("coverage entries = %#v", got)
	}
	if got[0].Status != reviewerCoverageIncompleteSkipped ||
		!strings.Contains(got[0].Diagnostic, "other.go") {
		t.Fatalf("coverage = %#v, want incomplete missing other.go", got[0])
	}
}

func TestReviewerScopesSeparateReadAccessFromExpectedCoverage(t *testing.T) {
	changed := []string{"api.go", "main.go", "schema.sql"}
	if got := reviewerAssignmentScope(llm.SelectedAgent{
		Files:        []string{"main.go"},
		AllowedFiles: []string{"schema.sql"},
	}, changed); !reflect.DeepEqual(got, []string{"schema.sql"}) {
		t.Fatalf("allowed-files scope = %#v, want allowed files", got)
	}
	if got := reviewerAssignmentScope(llm.SelectedAgent{Files: []string{"main.go"}}, changed); !reflect.DeepEqual(got, []string{"main.go"}) {
		t.Fatalf("broad coverage scope = %#v, want selected files", got)
	}
	assignmentScope := reviewerAssignmentScope(llm.SelectedAgent{AllowedFiles: []string{"schema.sql"}}, changed)
	contract := findingsOutputContract("agent-1", assignmentScope)
	contractAllowedValues, ok := contract.AllowedValues.(map[string]any)
	if !ok {
		t.Fatalf("contract allowed values type = %T, want map", contract.AllowedValues)
	}
	allowedValues, ok := contractAllowedValues["changed_files"].([]string)
	if !ok {
		t.Fatalf("contract changed_files type = %T, want []string", contractAllowedValues["changed_files"])
	}
	if !reflect.DeepEqual(allowedValues, []string{"schema.sql"}) {
		t.Fatalf("contract changed_files = %#v, want assignment scope", allowedValues)
	}
	_, err := llm.DecodeFindings([]byte(`{
		"schema_version": 1,
		"agent_id": "agent-1",
		"inspected_files": ["schema.sql"],
		"findings": [{
			"severity": "major",
			"file_path": "api.go",
			"anchor": {"kind": "file"},
			"body": "outside assignment"
		}]
	}`), llm.FindingsOptions{
		KnownAgents:  map[string]bool{"agent-1": true},
		ChangedFiles: stringSet(assignmentScope),
		NewFindingID: findingSequence("scope"),
	})
	if err == nil || !strings.Contains(err.Error(), "not in changed files") {
		t.Fatalf("DecodeFindings outside assignment error = %v, want not in changed files", err)
	}
}

func TestBuildReviewerCoverageAllowsBroadReviewerSplitAssignments(t *testing.T) {
	got := buildReviewerCoverage(
		[]llm.SelectedAgent{
			{AgentID: "harness:alpha", Files: []string{"main.go"}},
			{AgentID: "harness:beta", Files: []string{"other.go"}},
		},
		[]llm.Findings{
			{AgentID: "harness:alpha", InspectedFiles: []string{"main.go"}},
			{AgentID: "harness:beta", InspectedFiles: []string{"other.go"}},
		},
		nil,
		[]string{"main.go", "other.go"},
	)
	if len(got) != 2 {
		t.Fatalf("coverage entries = %#v, want two reviewer entries", got)
	}
	for _, entry := range got {
		if entry.Status != reviewerCoverageCompleteBroad {
			t.Fatalf("coverage entry = %#v, want complete broad", entry)
		}
	}
}

func TestBuildReviewerCoverageIgnoresSkippedFilesOutsideAssignmentScope(t *testing.T) {
	got := buildReviewerCoverage(
		[]llm.SelectedAgent{{AgentID: "harness:reviewer", Files: []string{"main.go"}}},
		[]llm.Findings{{AgentID: "harness:reviewer", InspectedFiles: []string{"main.go"}, SkippedFiles: []string{"other.go"}}},
		nil,
		[]string{"main.go", "other.go"},
	)
	if len(got) != 2 {
		t.Fatalf("coverage entries = %#v, want reviewer plus unassigned other.go", got)
	}
	if got[0].AgentID != "harness:reviewer" || got[0].Status != reviewerCoverageCompleteBroad {
		t.Fatalf("reviewer coverage = %#v, want complete broad for assigned scope", got[0])
	}
	if len(got[0].SkippedFiles) != 0 {
		t.Fatalf("reviewer skipped files = %#v, want outside-assignment skipped file filtered", got[0].SkippedFiles)
	}
	if got[1].AgentID != "unassigned" || got[1].Status != reviewerCoverageIncompleteUnassigned {
		t.Fatalf("unassigned coverage = %#v, want other.go unassigned", got[1])
	}
}

func TestDryRunContextBudgetFailures(t *testing.T) {
	tests := []struct {
		name   string
		budget int
		mutate func(t *testing.T, provider *readOnlyProvider, req *Request, adapter *llm.FakeAdapter)
		want   string
		runID  string
		queue  func(adapter *llm.FakeAdapter)
	}{
		{
			name:   "selection default model",
			budget: 100,
			mutate: func(t *testing.T, _ *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				dir := t.TempDir()
				writeAgent(t, dir, "harness", "reviewer", strings.Repeat("large ", 80), "prompt")
				trustCurrentTempFixtures(t)
				req.Profile.AgentSources = []string{dir}
			},
			want:  "context budget exceeded for selection model claude-sonnet-4-6",
			runID: "run-budget-selection-default",
		},
		{
			name:   "selection override model",
			budget: 100,
			mutate: func(t *testing.T, _ *readOnlyProvider, req *Request, _ *llm.FakeAdapter) {
				t.Helper()
				dir := t.TempDir()
				writeAgent(t, dir, "harness", "reviewer", strings.Repeat("large ", 80), "prompt")
				trustCurrentTempFixtures(t)
				req.Profile.AgentSources = []string{dir}
				req.SelectionModelOverride = "bench-model"
			},
			want:  "context budget exceeded for selection model bench-model",
			runID: "run-budget-selection-override",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := dryRunHarness(t)
			adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
			if tt.mutate != nil {
				tt.mutate(t, provider, &req, adapter)
			}
			if tt.queue != nil {
				tt.queue(adapter)
			}
			_, err := dryRunForTest(ctx, Options{
				Provider:        provider,
				Adapter:         adapter,
				Store:           store,
				Layout:          statepaths.NewLayout(t.TempDir(), t.TempDir()),
				Now:             fixedNow,
				NewRunID:        func() string { return tt.runID },
				NewSessionRowID: sequence("session"),
				NewFindingID:    findingSequence("finding"),
				NewActionID:     actionSequence(),
				Budget:          ContextBudget{MaxPromptBytes: tt.budget},
				MaxConcurrency:  1,
			}, req)
			if err == nil {
				t.Fatal("DryRun error = nil, want budget failure")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DryRun error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestSessionRowIDForFindingRequiresReviewerSession(t *testing.T) {
	finding := reviewplan.AnchoredFinding{FindingID: "finding-1"}
	if got, err := sessionRowIDForFinding(finding, map[review.FindingID]string{"finding-1": "session-1"}); err != nil || got != "session-1" {
		t.Fatalf("sessionRowIDForFinding = %q, %v; want session-1 nil", got, err)
	}
	if _, err := sessionRowIDForFinding(finding, map[review.FindingID]string{"other": "session-1"}); err == nil {
		t.Fatal("sessionRowIDForFinding missing error = nil, want invariant failure")
	}
	if _, err := sessionRowIDForFinding(finding, map[review.FindingID]string{"finding-1": "  "}); err == nil {
		t.Fatal("sessionRowIDForFinding blank error = nil, want invariant failure")
	}
}

type readOnlyProvider struct {
	pr                gitprovider.PR
	diff              gitprovider.UnifiedDiff
	diffCalls         int
	diffBetween       gitprovider.UnifiedDiff
	diffBetweenCalls  []shaPair
	files             map[fileKey][]byte
	fileCalls         []fileKey
	trees             map[fileKey][]gitprovider.TreeEntry
	treeCalls         []fileKey
	threads           []gitprovider.InlineThread
	reviews           []gitprovider.Review
	issueComments     []gitprovider.IssueComment
	threadCalls       int
	reviewCalls       int
	issueCommentCalls int
	caps              gitprovider.ProviderCaps
	onGetPR           func()
	fixtureRepoDir    string
}

type shaPair struct {
	baseSHA string
	headSHA string
}

type promptAwareAdapter struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (a *promptAwareAdapter) Name() string {
	return "prompt-aware"
}

func (a *promptAwareAdapter) SupportsResume() bool {
	return false
}

func (a *promptAwareAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.ReviewerWorkspaceWrite
}

func (a *promptAwareAdapter) SupportsCacheAccounting() bool {
	return false
}

func (a *promptAwareAdapter) SupportsCostReporting() bool {
	return false
}

func (a *promptAwareAdapter) Quota(context.Context) (llm.Quota, bool, error) {
	return llm.Quota{}, false, nil
}

func (a *promptAwareAdapter) Resume(context.Context, string, llm.Request) (llm.Stream, error) {
	return nil, errors.New("resume unsupported")
}

func (a *promptAwareAdapter) Start(_ context.Context, req llm.Request) (llm.Stream, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()

	switch {
	case strings.Contains(req.Prompt, `"schema": "selection"`):
		return staticStream{sessionID: "selection-session", output: `{
			"schema_version": 1,
			"selected_agents": [
				{"agent_id":"harness:alpha","rationale":"main","files":["main.go"]},
				{"agent_id":"harness:beta","rationale":"other","files":["other.go"]}
			],
			"thread_actions": [],
			"reasoning": "two agents"
		}`}, nil
	case strings.Contains(req.Prompt, `"schema": "rollup"`):
		return staticStream{sessionID: "rollup-session", output: rollupJSON("comment", findingIDsFromPrompt(req.Prompt))}, nil
	case strings.Contains(req.Prompt, "harness:alpha"):
		return staticStream{sessionID: "alpha-session", output: findingsJSON("harness:alpha", "main.go", "major", 2, "Alpha finding")}, nil
	case strings.Contains(req.Prompt, "harness:beta"):
		return staticStream{sessionID: "beta-session", output: findingsJSON("harness:beta", "other.go", "major", 2, "Beta finding")}, nil
	default:
		return nil, fmt.Errorf("unexpected prompt: %s", req.Prompt)
	}
}

func findingIDsFromPrompt(prompt string) []string {
	matches := regexp.MustCompile(`finding-\d+`).FindAllString(prompt, -1)
	seen := map[string]bool{}
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		if seen[match] {
			continue
		}
		seen[match] = true
		ids = append(ids, match)
	}
	return ids
}

func (a *promptAwareAdapter) Requests() []llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Request(nil), a.requests...)
}

type reviewerIsolationAdapter struct {
	mu                         sync.Mutex
	requests                   []llm.Request
	supportsResume             bool
	betaAttempts               int
	betaProviderErr            error
	betaRetrySawCleanWorkspace bool
	reviewerBarrier            *reviewerStartBarrier
}

func (a *reviewerIsolationAdapter) Name() string {
	return "reviewer-isolation"
}

func (a *reviewerIsolationAdapter) SupportsResume() bool {
	return a.supportsResume
}

func (a *reviewerIsolationAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.ReviewerWorkspaceWrite
}

func (a *reviewerIsolationAdapter) SupportsCacheAccounting() bool {
	return false
}

func (a *reviewerIsolationAdapter) SupportsCostReporting() bool {
	return false
}

func (a *reviewerIsolationAdapter) Quota(context.Context) (llm.Quota, bool, error) {
	return llm.Quota{}, false, nil
}

func (a *reviewerIsolationAdapter) Resume(context.Context, string, llm.Request) (llm.Stream, error) {
	return nil, errors.New("unexpected reviewer resume")
}

func (a *reviewerIsolationAdapter) Start(_ context.Context, req llm.Request) (llm.Stream, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()

	switch {
	case strings.Contains(req.Prompt, `"schema": "selection"`):
		return staticStream{sessionID: "selection-session", output: selectionJSONForAgents("main.go", "harness:alpha", "harness:beta", "harness:gamma")}, nil
	case strings.Contains(req.Prompt, `"schema": "rollup"`):
		return staticStream{sessionID: "rollup-session", output: rollupJSON("comment", findingIDsFromPrompt(req.Prompt))}, nil
	case strings.Contains(req.Prompt, `"id": "harness:alpha"`):
		a.waitReviewerStart("harness:alpha")
		return staticStream{sessionID: "alpha-session", output: findingsJSON("harness:alpha", "main.go", "major", 2, "alpha finding")}, nil
	case strings.Contains(req.Prompt, `"id": "harness:beta"`):
		a.waitReviewerStart("harness:beta")
		if a.betaProviderErr != nil {
			return staticStream{sessionID: "beta-provider-session", err: a.betaProviderErr}, nil
		}
		a.mu.Lock()
		a.betaAttempts++
		attempt := a.betaAttempts
		a.mu.Unlock()
		if req.ReviewerWorkspace != nil {
			markerPath := filepath.Join(req.ReviewerWorkspace.RepoDir, "beta-attempt-marker")
			if attempt == 1 {
				if err := os.WriteFile(markerPath, []byte("dirty"), 0o600); err != nil {
					return nil, err
				}
			} else {
				_, err := os.Stat(markerPath)
				a.mu.Lock()
				a.betaRetrySawCleanWorkspace = errors.Is(err, os.ErrNotExist)
				a.mu.Unlock()
			}
		}
		sessionID := "beta-session"
		if attempt > 1 {
			sessionID = "beta-retry-session"
		}
		return staticStream{sessionID: sessionID, output: `{"schema_version": 1, "agent_id": "harness:beta", "findings": [`}, nil
	case strings.Contains(req.Prompt, `"id": "harness:gamma"`):
		a.waitReviewerStart("harness:gamma")
		return staticStream{sessionID: "gamma-session", output: findingsJSON("harness:gamma", "main.go", "minor", 2, "gamma finding")}, nil
	default:
		return nil, fmt.Errorf("unexpected prompt: %s", req.Prompt)
	}
}

func (a *reviewerIsolationAdapter) Requests() []llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Request(nil), a.requests...)
}

func (a *reviewerIsolationAdapter) waitReviewerStart(agentID string) {
	if a.reviewerBarrier != nil {
		a.reviewerBarrier.wait(agentID)
	}
}

func (a *reviewerIsolationAdapter) ReviewerStartedCount() int {
	if a.reviewerBarrier == nil {
		return 0
	}
	return a.reviewerBarrier.startedCount()
}

func (a *reviewerIsolationAdapter) BetaRetrySawCleanWorkspace() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.betaRetrySawCleanWorkspace
}

type reviewerStartBarrier struct {
	mu      sync.Mutex
	want    int
	started map[string]bool
	release chan struct{}
	closed  bool
}

func newReviewerStartBarrier(want int) *reviewerStartBarrier {
	return &reviewerStartBarrier{
		want:    want,
		started: map[string]bool{},
		release: make(chan struct{}),
	}
}

func (b *reviewerStartBarrier) wait(agentID string) {
	b.mu.Lock()
	if !b.started[agentID] {
		b.started[agentID] = true
	}
	if !b.closed && len(b.started) >= b.want {
		close(b.release)
		b.closed = true
	}
	release := b.release
	b.mu.Unlock()
	<-release
}

func (b *reviewerStartBarrier) startedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.started)
}

type staticStream struct {
	sessionID string
	output    string
	err       error
}

func (s staticStream) SessionID() string {
	return s.sessionID
}

func (s staticStream) Wait(context.Context) (llm.Response, error) {
	if s.err != nil {
		return llm.Response{DurationMS: 1}, s.err
	}
	return llm.Response{StructuredOutput: []byte(s.output), DurationMS: 1}, nil
}

type failingStore struct {
	*ledger.Store
	insertPlannedActionErr error
}

func (s *failingStore) InsertPlanningResult(ctx context.Context, findings []ledger.Finding, actions []ledger.PlannedAction) error {
	if s.insertPlannedActionErr != nil {
		return s.insertPlannedActionErr
	}
	return s.Store.InsertPlanningResult(ctx, findings, actions)
}

type completeFailingStore struct {
	*ledger.Store
	err error
}

func (s *completeFailingStore) CompleteRun(context.Context, string, ledger.Outcome, time.Time) error {
	return s.err
}

type fileKey struct {
	gitRef string
	path   string
}

func (p *readOnlyProvider) GetPR(context.Context, gitprovider.PRRef) (gitprovider.PR, error) {
	if p.onGetPR != nil {
		p.onGetPR()
	}
	return p.pr, nil
}

func (p *readOnlyProvider) GetDiff(context.Context, gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	p.diffCalls++
	return p.diff, nil
}

func (p *readOnlyProvider) GetDiffBetweenRefs(_ context.Context, _ gitprovider.PRRef, baseSHA, headSHA string) (gitprovider.UnifiedDiff, error) {
	p.diffBetweenCalls = append(p.diffBetweenCalls, shaPair{baseSHA: baseSHA, headSHA: headSHA})
	return p.diffBetween, nil
}

func (p *readOnlyProvider) GetFileAtRef(_ context.Context, _ gitprovider.PRRef, gitRef string, path string) ([]byte, error) {
	p.fileCalls = append(p.fileCalls, fileKey{gitRef: gitRef, path: path})
	data, ok := p.files[fileKey{gitRef: gitRef, path: path}]
	if !ok {
		return nil, gitprovider.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (p *readOnlyProvider) ListTreeAtRef(_ context.Context, _ gitprovider.PRRef, gitRef string, path string) ([]gitprovider.TreeEntry, error) {
	p.treeCalls = append(p.treeCalls, fileKey{gitRef: gitRef, path: path})
	entries, ok := p.trees[fileKey{gitRef: gitRef, path: path}]
	if !ok {
		return nil, gitprovider.ErrNotFound
	}
	return append([]gitprovider.TreeEntry(nil), entries...), nil
}

func (p *readOnlyProvider) ListInlineThreads(context.Context, gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	p.threadCalls++
	return append([]gitprovider.InlineThread(nil), p.threads...), nil
}

func (p *readOnlyProvider) ListReviews(context.Context, gitprovider.PRRef) ([]gitprovider.Review, error) {
	p.reviewCalls++
	return append([]gitprovider.Review(nil), p.reviews...), nil
}

func (p *readOnlyProvider) ListIssueComments(context.Context, gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	p.issueCommentCalls++
	return append([]gitprovider.IssueComment(nil), p.issueComments...), nil
}

func (p *readOnlyProvider) Capabilities() gitprovider.ProviderCaps {
	return p.caps
}

func containsFileCall(calls []fileKey, want fileKey) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

type workbenchGitFixture struct {
	repoDir string
	baseSHA string
	headSHA string
	pr      gitprovider.PR
}

func newWorkbenchGitFixture(t *testing.T) workbenchGitFixture {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 370}
	return newWorkbenchGitFixtureForRef(t, ref)
}

func newWorkbenchGitFixtureForRef(t *testing.T, ref gitprovider.PRRef) workbenchGitFixture {
	t.Helper()
	repoDir := t.TempDir()
	gitCommandMustSucceed(t, repoDir, "init", "-b", "main")
	gitCommandMustSucceed(t, repoDir, "config", "user.name", "Workbench Test")
	gitCommandMustSucceed(t, repoDir, "config", "user.email", "workbench@example.com")
	gitCommandMustSucceed(t, repoDir, "remote", "add", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = false\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "other.go"), []byte("package main\n\nvar helper = true\n"), 0o600); err != nil {
		t.Fatalf("write other.go: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "add", "main.go", "other.go")
	gitCommandMustSucceed(t, repoDir, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))
	gitCommandMustSucceed(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = true\n"), 0o600); err != nil {
		t.Fatalf("update main.go: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "commit", "-am", "head")
	headSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))

	return workbenchGitFixture{
		repoDir: repoDir,
		baseSHA: baseSHA,
		headSHA: headSHA,
		pr: gitprovider.PR{
			Ref:   ref,
			Title: "Workbench fixture",
			URL:   prURL(ref),
			State: gitprovider.PRStateOpen,
			Base: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "main",
				Ref:   "refs/heads/main",
				SHA:   baseSHA,
			},
			Head: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "feature",
				Ref:   "refs/heads/feature",
				SHA:   headSHA,
			},
		},
	}
}

func newPinnedReviewFixtureForRef(t *testing.T, ref gitprovider.PRRef) (workbenchGitFixture, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	gitCommandMustSucceed(t, repoDir, "init", "-b", "main")
	gitCommandMustSucceed(t, repoDir, "config", "user.name", "Workbench Test")
	gitCommandMustSucceed(t, repoDir, "config", "user.email", "workbench@example.com")
	gitCommandMustSucceed(t, repoDir, "remote", "add", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = false\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "add", "main.go")
	gitCommandMustSucceed(t, repoDir, "commit", "-m", "base")
	reviewBaseSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = maybe\n"), 0o600); err != nil {
		t.Fatalf("update main.go for review head: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "commit", "-am", "review head")
	reviewHeadSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))
	gitCommandMustSucceed(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nvar changed = true\n"), 0o600); err != nil {
		t.Fatalf("update main.go for current head: %v", err)
	}
	gitCommandMustSucceed(t, repoDir, "commit", "-am", "current head")
	headSHA := strings.TrimSpace(gitCommandOutput(t, repoDir, "rev-parse", "HEAD"))

	return workbenchGitFixture{
		repoDir: repoDir,
		baseSHA: reviewHeadSHA,
		headSHA: headSHA,
		pr: gitprovider.PR{
			Ref:   ref,
			Title: "Pinned review fixture",
			URL:   prURL(ref),
			State: gitprovider.PRStateOpen,
			Base: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "main",
				Ref:   "refs/heads/main",
				SHA:   reviewHeadSHA,
			},
			Head: gitprovider.PRBranchRef{
				Host:  ref.Host,
				Owner: ref.Owner,
				Repo:  ref.Repo,
				Name:  "feature",
				Ref:   "refs/heads/feature",
				SHA:   headSHA,
			},
		},
	}, reviewBaseSHA, reviewHeadSHA
}

func gitCommandMustSucceed(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitCommandOutput(t, dir, args...))
}

func gitCommandOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func configureWorkbenchFixtureForTest(_ context.Context, opts *Options, ref gitprovider.PRRef) {
	if opts.ResolveRepoRoot != nil && opts.GitCommand != nil {
		return
	}
	provider, ok := opts.Provider.(*readOnlyProvider)
	if !ok || strings.TrimSpace(provider.fixtureRepoDir) == "" {
		return
	}
	repoDir := provider.fixtureRepoDir
	if opts.ResolveRepoRoot == nil {
		opts.ResolveRepoRoot = func(context.Context) (string, error) {
			return repoDir, nil
		}
	}
	if opts.GitCommand == nil {
		opts.GitCommand = workbenchGitCommandForTest(ref, repoDir)
	}
}

func workbenchGitCommandForTest(ref gitprovider.PRRef, repoDir string) func(context.Context, string, ...string) ([]byte, error) {
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmdArgs := append([]string(nil), args...)
		if len(cmdArgs) >= 3 && cmdArgs[0] == "fetch" && cmdArgs[2] == fmt.Sprintf("https://%s/%s/%s.git", ref.Host, ref.Owner, ref.Repo) {
			cmdArgs[2] = repoDir
		}
		cmd := exec.CommandContext(ctx, "git", cmdArgs...) // #nosec G204 -- tests invoke git with fixed command names and structured arguments.
		if strings.TrimSpace(dir) != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			message := strings.TrimSpace(string(out))
			if message == "" {
				message = err.Error()
			}
			return nil, fmt.Errorf("git %s: %s", strings.Join(cmdArgs, " "), message)
		}
		return out, nil
	}
}

func dryRunHarness(t *testing.T) (*readOnlyProvider, Request) {
	t.Helper()
	ref := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}
	fixture := newWorkbenchGitFixtureForRef(t, ref)
	pr := fixture.pr
	pr.Title = "CR-20 dry-run"
	pr.Body = "Default PR body."
	pr.Author = gitprovider.Identity{Login: "author", ID: "author-id"}
	dir := t.TempDir()
	writeAgent(t, dir, "harness", "reviewer", "reviewer desc", "Review carefully.")
	trustCurrentTempFixtures(t)
	provider := &readOnlyProvider{
		pr:             pr,
		diff:           gitprovider.UnifiedDiff{Raw: smallDiff("main.go")},
		files:          map[fileKey][]byte{},
		trees:          map[fileKey][]gitprovider.TreeEntry{},
		caps:           gitprovider.ProviderCaps{NativeFileLevelComments: true, ThreadResolution: true},
		fixtureRepoDir: fixture.repoDir,
	}
	addRepoAgentFixture(provider)
	req := Request{
		PRRef:           ref,
		PRURL:           pr.URL,
		ProfileName:     "home",
		Profile:         testProfile(dir),
		PostingIdentity: gitprovider.Identity{Login: "review-bot", ID: "bot-id"},
	}
	return provider, req
}

func addRepoAgentFixture(provider *readOnlyProvider) {
	categoryPath := ".codereview/agents/repo"
	agentPath := categoryPath + "/guidance"
	provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents"}] = []gitprovider.TreeEntry{{Path: "repo", Type: "tree"}}
	provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: categoryPath + "/index.yaml"}] = []byte("name: repo\ndescription: repo guidance category\nowner: owner\n")
	provider.trees[fileKey{gitRef: provider.pr.Base.SHA, path: categoryPath}] = []gitprovider.TreeEntry{{Path: agentPath, Type: "tree"}}
	provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: agentPath + "/index.yaml"}] = []byte("name: guidance\ndescription: repo guidance desc\nmodel_tier: medium\neffort: medium\n")
	provider.files[fileKey{gitRef: provider.pr.Base.SHA, path: agentPath + "/prompt.md"}] = []byte("Review carefully.")
}

func removeRepoAgentFixture(provider *readOnlyProvider) {
	delete(provider.trees, fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents"})
	delete(provider.trees, fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents/repo"})
	delete(provider.files, fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents/repo/index.yaml"})
	delete(provider.files, fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents/repo/guidance/index.yaml"})
	delete(provider.files, fileKey{gitRef: provider.pr.Base.SHA, path: ".codereview/agents/repo/guidance/prompt.md"})
}

func selectionRequestFromReview(req Request, artifactDir string) SelectionRequest {
	return SelectionRequest{
		PRRef:                       req.PRRef,
		ProfileName:                 req.ProfileName,
		Profile:                     req.Profile,
		PostingIdentity:             req.PostingIdentity,
		AgentDirs:                   append([]string(nil), req.AgentDirs...),
		ArtifactDir:                 artifactDir,
		ReviewBaseSHA:               req.ReviewBaseSHA,
		ReviewHeadSHA:               req.ReviewHeadSHA,
		SelectionModelOverride:      req.SelectionModelOverride,
		SelectionEffortOverride:     req.SelectionEffortOverride,
		SelectionPromptInstructions: req.SelectionPromptInstructions,
	}
}

func trustCurrentTempFixtures(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "system-temp"))
}

func initGitRepoForPipelineTest(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil { // #nosec G204 -- tests invoke git with fixed arguments.
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
}

func allocateLiveRun(t *testing.T, store *ledger.Store, provider *readOnlyProvider, req Request, runID string) ledger.Run {
	t.Helper()
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           runID,
		SHA:             provider.pr.Head.SHA,
		BaseSHA:         provider.pr.Base.SHA,
		Profile:         req.ProfileName,
		PostingIdentity: req.PostingIdentity.Login,
		PostMode:        ledger.PostModeLive,
		StartedAt:       fixedNow(),
		ArtifactPath:    filepath.Join(t.TempDir(), runID),
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func namedSessionForRequest(req Request, providerSessionID string) ledger.NamedSession {
	return ledger.NamedSession{
		Name:              req.SessionName,
		Profile:           req.ProfileName,
		Provider:          string(req.Profile.LLM.Provider),
		Adapter:           "fake-llm",
		Model:             "claude-sonnet-4-6",
		Host:              req.PRRef.Host,
		ProviderSessionID: providerSessionID,
		DurableSession:    true,
		CreatedAt:         fixedNow(),
		LastUsedAt:        fixedNow(),
	}
}

func testProfile(agentSource string) config.Profile {
	profile := config.Profile{
		Git: config.GitConfig{
			Host:       "github.com",
			AuthMode:   config.GitAuthModePAT,
			Credential: config.CredentialLocation{Store: config.LocalOSCredentialStoreID, Name: "codereview/home"},
		},
		LLM: config.LLMConfig{
			Provider: config.LLMProviderAnthropic,
			Auth:     config.LLMAuthSubscription,
			Adapter:  config.LLMAdapterClaudeCLI,
		},
		ReviewPolicy: config.ReviewPolicy{MajorEvent: config.ReviewMajorEventComment},
	}
	if agentSource != "" {
		profile.AgentSources = []string{agentSource}
	}
	return profile
}

func openPipelineStore(t *testing.T) *ledger.Store {
	t.Helper()
	store, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	return store
}

func closeStore(t *testing.T, store *ledger.Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
}

func sessionWithProviderID(sessions []ledger.Session, providerSessionID string) (ledger.Session, bool) {
	for _, session := range sessions {
		if session.ProviderSessionID == providerSessionID {
			return session, true
		}
	}
	return ledger.Session{}, false
}

type promptValidation struct {
	stage            string
	wants            []string
	requireWorkspace bool
}

type promptValidatingAdapter struct {
	base *llm.FakeAdapter

	mu          sync.Mutex
	validations []promptValidation
}

func newPromptValidatingAdapter(base *llm.FakeAdapter, validations ...promptValidation) *promptValidatingAdapter {
	return &promptValidatingAdapter{
		base:        base,
		validations: append([]promptValidation(nil), validations...),
	}
}

func (a *promptValidatingAdapter) Name() string {
	return a.base.Name()
}

func (a *promptValidatingAdapter) SupportsResume() bool {
	return a.base.SupportsResume()
}

func (a *promptValidatingAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return a.base.ReviewerWorkspaceMode()
}

func (a *promptValidatingAdapter) SupportsCacheAccounting() bool {
	return a.base.SupportsCacheAccounting()
}

func (a *promptValidatingAdapter) SupportsCostReporting() bool {
	return a.base.SupportsCostReporting()
}

func (a *promptValidatingAdapter) Quota(ctx context.Context) (llm.Quota, bool, error) {
	return a.base.Quota(ctx)
}

func (a *promptValidatingAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	validation, ok := a.nextValidation()
	if !ok {
		return nil, fmt.Errorf("unexpected LLM request with prompt:\n%s", req.Prompt)
	}
	if err := validation.validate(req); err != nil {
		return nil, err
	}
	return a.base.Start(ctx, req)
}

func (a *promptValidatingAdapter) Resume(ctx context.Context, sessionID string, req llm.Request) (llm.Stream, error) {
	return a.base.Resume(ctx, sessionID, req)
}

func (a *promptValidatingAdapter) Requests() []llm.Request {
	return a.base.Requests()
}

func (a *promptValidatingAdapter) AssertConsumed(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.validations) != 0 {
		t.Fatalf("unused prompt validations = %#v", a.validations)
	}
}

func (a *promptValidatingAdapter) nextValidation() (promptValidation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.validations) == 0 {
		return promptValidation{}, false
	}
	validation := a.validations[0]
	a.validations = a.validations[1:]
	return validation, true
}

func (v promptValidation) validate(req llm.Request) error {
	var missing []string
	if v.requireWorkspace && req.ReviewerWorkspace == nil {
		missing = append(missing, "reviewer workspace")
	}
	for _, want := range v.wants {
		if !strings.Contains(req.Prompt, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("%s prompt missing %s:\n%s", v.stage, strings.Join(missing, ", "), req.Prompt)
	}
	return nil
}

func assertPromptContains(t *testing.T, prompt string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func assertRollupUsageRow(t *testing.T, path string, workstream string, wantCacheCreate bool) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads an artifact path produced under t.TempDir.
	if err != nil {
		t.Fatalf("read rollup markdown: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		cells := markdownTableCells(line)
		if len(cells) < 8 || cells[0] != workstream {
			continue
		}
		for _, idx := range []int{2, 3, 4} {
			if cells[idx] == "" || cells[idx] == "unavailable" {
				t.Fatalf("rollup usage row %q cell %d = %q, want populated token/cache value in line %q", workstream, idx, cells[idx], line)
			}
		}
		if wantCacheCreate && (cells[5] == "" || cells[5] == "unavailable") {
			t.Fatalf("rollup usage row %q cache create = %q, want populated value in line %q", workstream, cells[5], line)
		}
		if !wantCacheCreate && cells[5] != "unavailable" {
			t.Fatalf("rollup usage row %q cache create = %q, want unavailable when provider omitted it in line %q", workstream, cells[5], line)
		}
		return
	}
	t.Fatalf("rollup markdown %s missing usage row for %q:\n%s", path, workstream, data)
}

func markdownTableCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}

func allocatePipelineRun(t *testing.T, store *ledger.Store, layout statepaths.Layout, runID string, mode ledger.PostMode, started time.Time) ledger.Run {
	t.Helper()
	artifactPath := filepath.Join(layout.DataRoot, "runs", "github_open-cli_codereview-cli_29", strings.Repeat("a", 40), strings.Repeat("b", 40), "home__review-bot", runID)
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           "github_open-cli_codereview-cli_29",
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/29",
		RunID:           runID,
		SHA:             strings.Repeat("a", 40),
		BaseSHA:         strings.Repeat("b", 40),
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        mode,
		StartedAt:       started,
		ArtifactPath:    artifactPath,
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func allocateDryRunForProvider(t *testing.T, store *ledger.Store, layout statepaths.Layout, provider *readOnlyProvider, req Request, runID string, started time.Time) ledger.Run {
	t.Helper()
	return allocateDryRunForSHAs(t, store, layout, req, runID, provider.pr.Head.SHA, provider.pr.Base.SHA, started)
}

func allocateDryRunForSHAs(t *testing.T, store *ledger.Store, layout statepaths.Layout, req Request, runID, headSHA, baseSHA string, started time.Time) ledger.Run {
	t.Helper()
	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	artifactPath := filepath.Join(layout.DataRoot, "runs", prKey, headSHA, baseSHA, statepaths.Encode(req.ProfileName)+"__"+statepaths.Encode(runlifecycle.PostingKey(req.PostingIdentity)), runID)
	run, err := store.AllocateRun(context.Background(), ledger.AllocateRunParams{
		PRKey:           prKey,
		PRURL:           req.PRURL,
		RunID:           runID,
		SHA:             headSHA,
		BaseSHA:         baseSHA,
		Profile:         req.ProfileName,
		PostingIdentity: runlifecycle.PostingKey(req.PostingIdentity),
		PostMode:        ledger.PostModeDryRun,
		StartedAt:       started,
		ArtifactPath:    artifactPath,
	})
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	if err := runartifact.WriteMarker(run.ArtifactPath, runartifact.KindReview, run.RunID); err != nil {
		t.Fatalf("WriteMarker review: %v", err)
	}
	return run
}

func removeReviewRunMarkerForTest(t *testing.T, artifactPath string) {
	t.Helper()
	if err := os.Remove(runartifact.MarkerPath(artifactPath, runartifact.KindReview)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove review marker: %v", err)
	}
}

func writeResponseRunMarkerForTest(t *testing.T, artifactPath, runID string) {
	t.Helper()
	if err := runartifact.WriteMarker(artifactPath, runartifact.KindThreadResponse, runID); err != nil {
		t.Fatalf("WriteMarker response: %v", err)
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}

func sequence(prefix string) func() string {
	var counter int
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("%s-%d", prefix, counter)
	}
}

func findingSequence(prefix string) func() (review.FindingID, error) {
	next := sequence(prefix)
	return func() (review.FindingID, error) {
		return review.FindingID(next()), nil
	}
}

func actionSequence() func(reviewplan.ActionKind) (string, error) {
	counters := map[reviewplan.ActionKind]int{}
	return func(kind reviewplan.ActionKind) (string, error) {
		counters[kind]++
		return fmt.Sprintf("%s-%d", kind, counters[kind]), nil
	}
}

func markedReviewThread(t *testing.T, id, path string, line int, bot, human gitprovider.Identity) gitprovider.InlineThread {
	t.Helper()
	action, err := marker.RenderAction(marker.ActionMarker{
		RunID:    "old-run",
		ActionID: "old-action",
		Kind:     marker.ActionKindInlineComment,
		SHA:      strings.Repeat("a", 40),
		BaseSHA:  strings.Repeat("b", 40),
	})
	if err != nil {
		t.Fatalf("RenderAction: %v", err)
	}
	threadID := gitprovider.ThreadID(id)
	created := fixedNow()
	return gitprovider.InlineThread{
		ID:          threadID,
		Resolved:    false,
		Path:        path,
		Side:        review.DiffSideRight,
		Line:        line,
		SubjectType: review.AnchorKindLine,
		CommitSHA:   strings.Repeat("a", 40),
		Comments: []gitprovider.ThreadComment{
			{
				ID:          gitprovider.CommentID(id + "-cr"),
				ThreadID:    threadID,
				Body:        action + "\nOriginal finding.",
				Author:      bot,
				CommitSHA:   strings.Repeat("a", 40),
				Path:        path,
				Side:        review.DiffSideRight,
				Line:        line,
				SubjectType: review.AnchorKindLine,
				CreatedAt:   created,
				UpdatedAt:   created,
			},
			{
				ID:          gitprovider.CommentID(id + "-human"),
				ThreadID:    threadID,
				Body:        "Human reply",
				Author:      human,
				CommitSHA:   strings.Repeat("a", 40),
				Path:        path,
				Side:        review.DiffSideRight,
				Line:        line,
				SubjectType: review.AnchorKindLine,
				CreatedAt:   created.Add(time.Minute),
				UpdatedAt:   created.Add(time.Minute),
			},
		},
	}
}

func crSettledReviewThread(t *testing.T, id, path string, line int, bot, human gitprovider.Identity, summary string) gitprovider.InlineThread {
	t.Helper()
	thread := markedReviewThread(t, id, path, line, bot, human)
	summaryMarker, err := marker.RenderThreadSummary(marker.ThreadSummaryMarker{
		RunID:    "response-run",
		ActionID: "summary-" + id,
	})
	if err != nil {
		t.Fatalf("RenderThreadSummary: %v", err)
	}
	created := fixedNow().Add(2 * time.Minute)
	thread.Comments = append(thread.Comments, gitprovider.ThreadComment{
		ID:          gitprovider.CommentID(id + "-summary"),
		ThreadID:    thread.ID,
		Body:        summaryMarker + "\n\n" + summary,
		Author:      bot,
		CommitSHA:   strings.Repeat("a", 40),
		Path:        path,
		Side:        review.DiffSideRight,
		Line:        line,
		SubjectType: review.AnchorKindLine,
		CreatedAt:   created,
		UpdatedAt:   created,
	})
	return thread
}

func fakeLLMResult(sessionID, structured string, tokensIn, tokensOut int) llm.FakeResult {
	return llm.FakeResult{
		SessionID: sessionID,
		Response: llm.Response{
			StructuredOutput: []byte(structured),
			Usage:            llm.Usage{TokensIn: intPtr(tokensIn), TokensOut: intPtr(tokensOut)},
			DurationMS:       123,
		},
	}
}

type providerOriginUsageAdapter struct {
	mu       sync.Mutex
	name     string
	adapters []llm.Adapter
}

func (a *providerOriginUsageAdapter) Queue(adapter llm.Adapter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.adapters = append(a.adapters, adapter)
}

func (a *providerOriginUsageAdapter) Name() string {
	if strings.TrimSpace(a.name) != "" {
		return a.name
	}
	return "provider_origin_usage"
}

func (a *providerOriginUsageAdapter) SupportsResume() bool { return false }

func (a *providerOriginUsageAdapter) SupportsCacheAccounting() bool { return false }

func (a *providerOriginUsageAdapter) SupportsCostReporting() bool { return false }

func (a *providerOriginUsageAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.ReviewerWorkspacePermissionBounded
}

func (a *providerOriginUsageAdapter) Quota(context.Context) (llm.Quota, bool, error) {
	return llm.Quota{}, false, nil
}

func (a *providerOriginUsageAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	a.mu.Lock()
	if len(a.adapters) == 0 {
		a.mu.Unlock()
		return nil, fmt.Errorf("provider origin usage adapter: no queued adapter")
	}
	adapter := a.adapters[0]
	a.adapters = a.adapters[1:]
	a.mu.Unlock()
	return adapter.Start(ctx, req)
}

func (a *providerOriginUsageAdapter) Resume(context.Context, string, llm.Request) (llm.Stream, error) {
	return nil, fmt.Errorf("provider origin usage adapter: resume unsupported")
}

func newCodexUsageScriptAdapter(t *testing.T, sessionID string, structured string, usage llm.Usage) llm.Adapter {
	t.Helper()
	script := writeExecutableScript(t, "codex-usage", codexUsageScript(t, sessionID, structured, usage))
	return llmadapters.NewCodexCLIAdapter(llmadapters.SubprocessOptions{
		Command:                script,
		Timeout:                5 * time.Second,
		AllowBestEffortNoTools: true,
	})
}

func newClaudeTranscriptScriptAdapter(t *testing.T, sessionID string, structured string, usage llm.Usage) llm.Adapter {
	t.Helper()
	configDir := t.TempDir()
	workDir := t.TempDir()
	transcriptPath := writeClaudeUsageTranscript(t, usage)
	state := map[string]any{
		"state":        "done",
		"sessionId":    sessionID,
		"linkScanPath": transcriptPath,
		"createdAt":    "2026-06-09T20:00:00Z",
	}
	stateJSON := mustMarshalJSON(t, state)
	script := writeExecutableScript(t, "claude-transcript", claudeTranscriptScript(sessionID, structured, stateJSON))
	return llmadapters.NewClaudeCLIAdapter(llmadapters.SubprocessOptions{
		Command: script,
		Env: []string{
			"CLAUDE_CONFIG_DIR=" + configDir,
			"CR_CLAUDE_BG_WORK_DIR=" + workDir,
		},
		Timeout: 5 * time.Second,
	})
}

func codexUsageScript(t *testing.T, sessionID string, structured string, usage llm.Usage) string {
	t.Helper()
	usageFields := []string{
		fmt.Sprintf(`"input_tokens":%d`, mustInt(t, usage.TokensIn, "TokensIn")),
		fmt.Sprintf(`"output_tokens":%d`, mustInt(t, usage.TokensOut, "TokensOut")),
	}
	if usage.CacheRead != nil {
		usageFields = append(usageFields, fmt.Sprintf(`"cached_input_tokens":%d`, *usage.CacheRead))
	}
	if usage.CacheCreate != nil {
		usageFields = append(usageFields, fmt.Sprintf(`"cache_create":%d`, *usage.CacheCreate))
	}
	return fmt.Sprintf(`#!/bin/sh
cat <<'JSONL'
{"type":"thread.started","thread_id":%s}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":%s}}
{"type":"turn.completed","usage":{%s,"reasoning_output_tokens":271}}
JSONL
`, mustMarshalJSON(t, sessionID), mustMarshalJSON(t, structured), strings.Join(usageFields, ","))
}

func claudeTranscriptScript(sessionID string, structured string, stateJSON string) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1" in
  stop|rm) exit 0 ;;
  agents) printf '[]'; exit 0 ;;
esac
add_dir=""
want_add_dir=0
for arg in "$@"; do
  if [ "$want_add_dir" = "1" ]; then
    if [ -z "$add_dir" ]; then add_dir="$arg"; fi
    want_add_dir=0
    continue
  fi
  if [ "$arg" = "--add-dir" ]; then want_add_dir=1; fi
done
job_id="job-%s"
mkdir -p "$CLAUDE_CONFIG_DIR/jobs/$job_id" "$add_dir"
cat > "$CLAUDE_CONFIG_DIR/jobs/$job_id/state.json" <<'STATE'
%s
STATE
cat > "$add_dir/cr-result.json" <<'RESULT'
%s
RESULT
printf 'backgrounded * %%s\n  claude attach %%s\n' "$job_id" "$job_id"
`, sessionID, stateJSON, structured)
}

func writeClaudeUsageTranscript(t *testing.T, usage llm.Usage) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-transcript.jsonl")
	line := map[string]any{
		"type":      "assistant",
		"timestamp": "2026-06-09T20:00:02Z",
		"message": map[string]any{
			"id": "message-1",
			"usage": map[string]any{
				"input_tokens":                mustInt(t, usage.TokensIn, "TokensIn"),
				"output_tokens":               mustInt(t, usage.TokensOut, "TokensOut"),
				"cache_read_input_tokens":     mustInt(t, usage.CacheRead, "CacheRead"),
				"cache_creation_input_tokens": mustInt(t, usage.CacheCreate, "CacheCreate"),
			},
		},
	}
	data := mustMarshalJSON(t, line) + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write Claude usage transcript: %v", err)
	}
	return path
}

func writeExecutableScript(t *testing.T, name string, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sh")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s script: %v", name, err)
	}
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- test helper script must be executable and lives under t.TempDir.
		t.Fatalf("chmod %s script: %v", name, err)
	}
	return path
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}

func mustInt(t *testing.T, value *int, name string) int {
	t.Helper()
	if value == nil {
		t.Fatalf("%s must be set", name)
	}
	return *value
}

func selectionJSON(agentID, file string) string {
	return fmt.Sprintf(`{
		"schema_version": 1,
		"selected_agents": [{
			"agent_id": %q,
			"rationale": "go file changed",
			"files": [%q]
		}],
		"thread_actions": [],
		"reasoning": "select reviewer"
	}`, agentID, file)
}

func selectionJSONForAgents(file string, agentIDs ...string) string {
	selected := make([]map[string]any, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		selected = append(selected, map[string]any{
			"agent_id":  agentID,
			"rationale": "go file changed",
			"files":     []string{file},
		})
	}
	payload := map[string]any{
		"schema_version":  1,
		"selected_agents": selected,
		"thread_actions":  []any{},
		"reasoning":       "select reviewers",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func findingsJSON(agentID, file, severity string, line int, body string) string {
	payload := map[string]any{
		"schema_version":  1,
		"agent_id":        agentID,
		"inspected_files": []string{file},
		"skipped_files":   []string{},
		"constraints":     []string{},
		"findings": []map[string]any{{
			"severity":  severity,
			"file_path": file,
			"anchor": map[string]any{
				"kind": "line",
				"side": "RIGHT",
				"line": line,
			},
			"body": body,
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func coverageOnlyJSON(agentID string, inspected, skipped []string, constraints ...string) string {
	payload := map[string]any{
		"schema_version":  1,
		"agent_id":        agentID,
		"inspected_files": inspected,
		"skipped_files":   skipped,
		"constraints":     constraints,
		"findings":        []map[string]any{},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func largeRollupFindings(count int, filePath, body string) []review.Finding {
	out := make([]review.Finding, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, review.Finding{
			ID:       review.FindingID(fmt.Sprintf("finding-%d", i+1)),
			Severity: review.SeverityMajor,
			FilePath: filePath,
			Anchor: review.Anchor{
				Kind: review.AnchorKindLine,
				Side: review.DiffSideRight,
				Line: i + 2,
			},
			Body: body,
		})
	}
	return out
}

func findingsFileAliasJSON(agentID, file, severity string, line int, body string) string {
	payload := map[string]any{
		"schema_version":  1,
		"agent_id":        agentID,
		"inspected_files": []string{file},
		"skipped_files":   []string{},
		"constraints":     []string{},
		"findings": []map[string]any{{
			"severity": severity,
			"file":     file,
			"anchor": map[string]any{
				"kind": "line",
				"side": "RIGHT",
				"line": line,
			},
			"body": body,
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func rollupJSON(event string, ordered []string) string {
	payload := map[string]any{
		"schema_version":         1,
		"review_event":           event,
		"review_event_rationale": "policy",
		"dedupe_log":             []any{},
		"ordered_findings":       ordered,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

type threadSummary struct {
	path       string
	side       string
	line       int
	anchorKind string
	resolved   bool
	status     string
	summary    string
}

func discussionSummaryJSON(topLevel []string, threads []threadSummary) string {
	topLevelPayload := make([]map[string]any, 0, len(topLevel))
	for _, summary := range topLevel {
		topLevelPayload = append(topLevelPayload, map[string]any{
			"kind":    "issue_comment",
			"author":  "reviewer",
			"summary": summary,
		})
	}
	threadPayload := make([]map[string]any, 0, len(threads))
	for _, thread := range threads {
		side := thread.side
		if side == "" {
			side = string(review.DiffSideRight)
		}
		anchorKind := thread.anchorKind
		if anchorKind == "" {
			anchorKind = string(review.AnchorKindLine)
		}
		threadPayload = append(threadPayload, map[string]any{
			"path":        thread.path,
			"side":        side,
			"line":        thread.line,
			"anchor_kind": anchorKind,
			"resolved":    thread.resolved,
			"status":      thread.status,
			"summary":     thread.summary,
		})
	}
	payload := map[string]any{
		"schema_version":     dossierSummarySchemaVersion,
		"top_level_comments": topLevelPayload,
		"inline_threads":     threadPayload,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func smallDiff(path string) string {
	return strings.Join([]string{
		"diff --git a/" + path + " b/" + path,
		"index 1111111..2222222 100644",
		"--- a/" + path,
		"+++ b/" + path,
		"@@ -1,2 +1,2 @@",
		" package main",
		"-var changed = false",
		"+var changed = true",
		"",
	}, "\n")
}

func largeDiff(path, body string) string {
	return strings.Join([]string{
		"diff --git a/" + path + " b/" + path,
		"index 1111111..2222222 100644",
		"--- a/" + path,
		"+++ b/" + path,
		"@@ -1,1 +1,400 @@",
		"-package main",
		body,
		"",
	}, "\n")
}

func writeAgent(t *testing.T, rootDir, category, agent, description, prompt string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, "index.yaml"), "name: "+category+"\ndescription: "+category+" category\nowner: owner\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), agentYAML(agent, description, false))
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), prompt)
}

func relativeAgentSource(t *testing.T) (string, string) {
	t.Helper()
	cwd := t.TempDir()
	source := filepath.Join(cwd, "agents")
	writeAgent(t, source, "harness", "reviewer", "reviewer desc", "Review carefully.")
	t.Chdir(cwd)
	return "agents", ""
}

func tempAgentSource(t *testing.T) (string, string) {
	t.Helper()
	tempRoot := os.TempDir()
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll temp root: %v", err)
	}
	source, err := os.MkdirTemp(tempRoot, "codereview-agents-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(source); err != nil {
			t.Fatalf("RemoveAll temp agent source: %v", err)
		}
	})
	writeAgent(t, source, "harness", "reviewer", "reviewer desc", "Review carefully.")
	return source, ""
}

func gitWorktreeAgentSource(t *testing.T) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	trustCurrentTempFixtures(t)
	repoRoot := filepath.Join(workspace, "review-repo")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll review repo: %v", err)
	}
	initGitRepoForPipelineTest(t, repoRoot)
	source := filepath.Join(repoRoot, "nested", "agents")
	writeAgent(t, source, "harness", "reviewer", "reviewer desc", "Review carefully.")
	return source, repoRoot
}

func siblingGitCatalogSource(t *testing.T) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	trustCurrentTempFixtures(t)
	reviewRoot := filepath.Join(workspace, "review-repo")
	if err := os.MkdirAll(reviewRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll review repo: %v", err)
	}
	initGitRepoForPipelineTest(t, reviewRoot)
	catalogRoot := filepath.Join(workspace, "catalog-repo")
	if err := os.MkdirAll(catalogRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll catalog repo: %v", err)
	}
	initGitRepoForPipelineTest(t, catalogRoot)
	source := filepath.Join(catalogRoot, "agents")
	writeAgent(t, source, "harness", "reviewer", "reviewer desc", "Review carefully.")
	return source, reviewRoot
}

func writeAgentFullContent(t *testing.T, rootDir, category, agent string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, "index.yaml"), "name: "+category+"\ndescription: "+category+" category\nowner: owner\n")
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), agentYAML(agent, "full content reviewer", true))
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), "Review full files.")
}

func writeAgentModelID(t *testing.T, rootDir, category, agent, modelID string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), fmt.Sprintf("name: %s\ndescription: %s desc\nmodel_id: %s\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n", agent, agent, modelID))
}

func writeAgentWithModelTier(t *testing.T, rootDir, category, agent, modelTier string) {
	t.Helper()
	writeFile(t, filepath.Join(rootDir, category, agent, "index.yaml"), fmt.Sprintf("name: %s\ndescription: %s desc\nmodel_tier: %s\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: false\n", agent, agent, modelTier))
	writeFile(t, filepath.Join(rootDir, category, agent, "prompt.md"), "Review carefully.")
}

func agentYAML(name, description string, needsFullContent bool) string {
	return fmt.Sprintf("name: %s\ndescription: %s\nmodel_tier: medium\neffort: medium\nfile_globs:\n  - '**/*.go'\napplies_when:\n  - Go files changed\nneeds_full_file_content: %t\n", name, description, needsFullContent)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test helper reads caller-provided paths under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("file %s = %q, want substring %q", path, data, want)
	}
}

func assertAgentSourcesArtifact(t *testing.T, path, wantAgent string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test helper reads caller-provided artifact paths under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var artifact agentSourcesArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("Unmarshal agent sources artifact: %v\n%s", err, data)
	}
	profileSource, ok := findArtifactSource(artifact.Sources, agents.SourceProfile)
	if !ok {
		t.Fatalf("artifact sources = %#v, want profile source", artifact.Sources)
	}
	if profileSource.Status != agents.SourceStatusAvailable || profileSource.Fingerprint == "" || profileSource.CanonicalPath == "" || len(profileSource.Warnings) != 0 {
		t.Fatalf("profile source = %#v, want trusted available source with fingerprint and canonical path", profileSource)
	}
	repoSource, ok := findArtifactSource(artifact.Sources, agents.SourceRepo)
	if !ok {
		t.Fatalf("artifact sources = %#v, want repo source", artifact.Sources)
	}
	if repoSource.Status != agents.SourceStatusAvailable || !repoSource.Present || repoSource.SHA == "" {
		t.Fatalf("repo source = %#v, want available repo source anchored to base SHA", repoSource)
	}
	for _, agent := range artifact.Agents {
		if agent.ID == wantAgent &&
			agent.Source.Fingerprint == profileSource.Fingerprint &&
			agent.Source.CanonicalPath == profileSource.CanonicalPath &&
			agent.Source.Status == agents.SourceStatusAvailable {
			return
		}
	}
	t.Fatalf("artifact agents = %#v, want %s with exact profile source provenance", artifact.Agents, wantAgent)
}

func assertReviewerRuntimeArtifact(t *testing.T, path, wantAgent string, want reviewerRuntimeResolution) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test helper reads caller-provided artifact paths under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var artifact agentSourcesArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("Unmarshal agent sources artifact: %v\n%s", err, data)
	}
	for _, agent := range artifact.Agents {
		if agent.ID != wantAgent {
			continue
		}
		if agent.ReviewerRuntime == nil {
			t.Fatalf("agent %s reviewer runtime = nil, want %#v", wantAgent, want)
		}
		if *agent.ReviewerRuntime != want {
			t.Fatalf("agent %s reviewer runtime = %#v, want %#v", wantAgent, *agent.ReviewerRuntime, want)
		}
		return
	}
	t.Fatalf("artifact agents = %#v, want reviewer runtime for %s", artifact.Agents, wantAgent)
}

func assertDossierIndexArtifact(t *testing.T, dir, wantPath string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "index.json")) // #nosec G304 -- test reads artifact path under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(dossier index): %v", err)
	}
	var index dossierIndexArtifact
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("Unmarshal dossier index: %v\n%s", err, data)
	}
	if index.HashAlgorithm != "sha256" {
		t.Fatalf("hash algorithm = %q, want sha256", index.HashAlgorithm)
	}
	if len(index.Files) == 0 {
		t.Fatal("dossier index files = 0, want artifacts")
	}
	wantHashes := map[string]string{}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Base(path) == "index.json" {
			return nil
		}
		fileData, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		wantHashes[filepath.ToSlash(path)] = sha256Hex(fileData)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(dossier): %v", err)
	}
	var saw bool
	for _, file := range index.Files {
		if file.Path == wantPath {
			saw = true
		}
		if file.Path == "" || file.SHA256 == "" {
			t.Fatalf("index file = %#v, want non-empty path/hash", file)
		}
		wantHash, ok := wantHashes[file.Path]
		if !ok {
			t.Fatalf("index file = %#v, want tracked dossier artifact", file)
		}
		if file.SHA256 != wantHash {
			t.Fatalf("index hash for %s = %q, want %q", file.Path, file.SHA256, wantHash)
		}
		delete(wantHashes, file.Path)
	}
	if !saw {
		t.Fatalf("dossier index files = %#v, want %q", index.Files, wantPath)
	}
	if len(wantHashes) != 0 {
		t.Fatalf("dossier index missing files = %#v", wantHashes)
	}
}

func assertFileOmits(t *testing.T, path string, forbidden ...string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads artifact path returned by pipeline under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	text := string(data)
	for _, needle := range forbidden {
		if strings.Contains(text, needle) {
			t.Fatalf("artifact %s contains forbidden substring %q:\n%s", path, needle, text)
		}
	}
}

func findArtifactSource(sources []agents.SourceInfo, kind agents.SourceKind) (agents.SourceInfo, bool) {
	for _, source := range sources {
		if source.Kind == kind {
			return source, true
		}
	}
	return agents.SourceInfo{}, false
}

func assertPromptOmitsLocalAgentSourceProvenance(t *testing.T, prompt string, sources []agents.SourceInfo) {
	t.Helper()
	for _, forbidden := range []string{
		"configured_path",
		"canonical_path",
		"Source warning",
		"OS temp directory",
		"Git worktree",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt contains local source provenance %q:\n%s", forbidden, prompt)
		}
	}
	for _, source := range sources {
		for _, forbidden := range []string{source.ConfiguredPath, source.CanonicalPath, source.Fingerprint} {
			if forbidden != "" && strings.Contains(prompt, forbidden) {
				t.Fatalf("prompt contains local source value %q:\n%s", forbidden, prompt)
			}
		}
		for _, warning := range source.Warnings {
			if warning != "" && strings.Contains(prompt, warning) {
				t.Fatalf("prompt contains local source warning %q:\n%s", warning, prompt)
			}
		}
	}
}

func prURL(ref gitprovider.PRRef) string {
	return fmt.Sprintf("https://%s/%s/%s/pull/%d", ref.Host, ref.Owner, ref.Repo, ref.Number)
}

func intPtr(value int) *int {
	return &value
}

func TestReadOnlyProviderDoesNotSatisfyGitProvider(t *testing.T) {
	var provider any = &readOnlyProvider{}
	if _, ok := provider.(gitprovider.GitProvider); ok {
		t.Fatal("readOnlyProvider unexpectedly satisfies gitprovider.GitProvider")
	}
	if _, ok := provider.(ReadProvider); !ok {
		t.Fatal("readOnlyProvider does not satisfy pipeline.ReadProvider")
	}
}

func TestDryRunFailsOnProviderReadError(t *testing.T) {
	errProvider := errors.New("provider failed")
	provider := &failingProvider{err: errProvider}
	_, err := DryRun(context.Background(), Options{
		Provider: provider,
		Adapter:  &llm.FakeAdapter{},
		Store:    &noopStore{},
		Layout:   statepaths.NewLayout(t.TempDir(), t.TempDir()),
	}, Request{
		PRRef:           gitprovider.PRRef{Host: "github.com", Owner: "o", Repo: "r", Number: 1},
		PRURL:           "https://github.com/o/r/pull/1",
		ProfileName:     "home",
		PostingIdentity: gitprovider.Identity{Login: "bot"},
	})
	if !errors.Is(err, errProvider) {
		t.Fatalf("DryRun error = %v, want provider read error", err)
	}
}

type failingProvider struct {
	err error
}

func (p *failingProvider) GetPR(context.Context, gitprovider.PRRef) (gitprovider.PR, error) {
	return gitprovider.PR{}, p.err
}

func (p *failingProvider) GetDiff(context.Context, gitprovider.PRRef) (gitprovider.UnifiedDiff, error) {
	return gitprovider.UnifiedDiff{}, p.err
}

func (p *failingProvider) GetFileAtRef(context.Context, gitprovider.PRRef, string, string) ([]byte, error) {
	return nil, p.err
}

func (p *failingProvider) ListTreeAtRef(context.Context, gitprovider.PRRef, string, string) ([]gitprovider.TreeEntry, error) {
	return nil, p.err
}

func (p *failingProvider) ListInlineThreads(context.Context, gitprovider.PRRef) ([]gitprovider.InlineThread, error) {
	return nil, p.err
}

func (p *failingProvider) ListReviews(context.Context, gitprovider.PRRef) ([]gitprovider.Review, error) {
	return nil, p.err
}

func (p *failingProvider) ListIssueComments(context.Context, gitprovider.PRRef) ([]gitprovider.IssueComment, error) {
	return nil, p.err
}

func (p *failingProvider) Capabilities() gitprovider.ProviderCaps {
	return gitprovider.ProviderCaps{}
}

type noopStore struct{}

func (noopStore) ListRuns(context.Context) ([]ledger.Run, error) {
	return nil, nil
}

func (noopStore) ListRunsForHeadScope(context.Context, ledger.ListRunsForHeadScopeParams) ([]ledger.Run, error) {
	return nil, nil
}

func (noopStore) DeleteRun(context.Context, string) error {
	return nil
}

func (noopStore) AllocateRun(context.Context, ledger.AllocateRunParams) (ledger.Run, error) {
	return ledger.Run{}, nil
}

func (noopStore) InsertSession(context.Context, ledger.Session) error {
	return nil
}

func (noopStore) GetSession(context.Context, string) (ledger.Session, error) {
	return ledger.Session{}, ledger.ErrNotFound
}

func (noopStore) InsertPlanningResult(context.Context, []ledger.Finding, []ledger.PlannedAction) error {
	return nil
}

func (noopStore) ListFindings(context.Context, string) ([]ledger.Finding, error) {
	return nil, nil
}

func (noopStore) ListPlannedActions(context.Context, string) ([]ledger.PlannedAction, error) {
	return nil, nil
}

func (noopStore) CompleteRun(context.Context, string, ledger.Outcome, time.Time) error {
	return nil
}
