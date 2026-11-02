package pipeline

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/runlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func requiredCohortCatalog() agents.Catalog {
	return agents.Catalog{Agents: []agents.Agent{
		{ID: "lang:go", ModelTier: "medium", Effort: "medium", FileGlobs: []string{"**/*.go"}, Provenance: agents.Provenance{Kind: agents.SourceProfile}},
		{ID: "privacy:data", ModelTier: "medium", Effort: "medium", FileGlobs: []string{"**/*", "!**/*.lock"}, RequiredOnMatch: true, Provenance: agents.Provenance{Kind: agents.SourceProfile}},
	}}
}

func goOnlyCohort() ledger.ReviewerCohort {
	return ledger.ReviewerCohort{
		Scope:   ledger.ReviewerCohortScope{PRKey: "github.com/o/r#1", Profile: "default", PostingIdentity: "bot"},
		Adapter: "fake-llm", CreatedAt: fixedNow(), UpdatedAt: fixedNow(),
		Members: []ledger.ReviewerCohortMember{{
			AgentID: "lang:go", AssignmentMode: ledger.ReviewerAssignmentScoped,
			Files: []string{"main.go"}, AllowedFiles: []string{"main.go"},
			Model: "claude-sonnet-5-5", Effort: "medium", ProviderSessionID: "go-session",
		}},
	}
}

func TestJoinRequiredReviewersAddsMissingRequiredReviewerAndGrowsCohort(t *testing.T) {
	catalog := requiredCohortCatalog()
	cohort := goOnlyCohort()
	req := Request{Profile: testProfile(""), ProfileName: "default"}
	changed := []string{"main.go", "tests/fixtures/loans.json", "Cargo.lock"}

	rebased, _, err := rebaseReviewerCohort(req, catalog, cohort, changed, 0, "fake-llm")
	if err != nil {
		t.Fatalf("rebaseReviewerCohort: %v", err)
	}
	selection, grown, err := joinRequiredReviewers(req, catalog, cohort, rebased, changed, 0, fixedNow())
	if err != nil {
		t.Fatalf("joinRequiredReviewers: %v", err)
	}

	var ids []string
	for _, selected := range selection.SelectedAgents {
		ids = append(ids, selected.AgentID)
	}
	if want := []string{"lang:go", "privacy:data"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("selected agents = %#v, want saved member then joined required reviewer %#v", ids, want)
	}
	joined := selection.SelectedAgents[1]
	wantFiles := []string{"main.go", "tests/fixtures/loans.json"}
	if !reflect.DeepEqual(joined.Files, wantFiles) || !reflect.DeepEqual(joined.AllowedFiles, wantFiles) {
		t.Fatalf("joined assignment = %#v, want every glob match %#v", joined, wantFiles)
	}
	if grown == nil {
		t.Fatal("grown cohort = nil, want the joined reviewer persisted")
	}
	if len(grown.Members) != 2 || !reflect.DeepEqual(grown.Members[0], cohort.Members[0]) {
		t.Fatalf("grown members = %#v, want the saved member unchanged first", grown.Members)
	}
	added := grown.Members[1]
	if added.AgentID != "privacy:data" || added.AssignmentMode != ledger.ReviewerAssignmentScoped ||
		added.Model != "claude-sonnet-5-5" || added.Effort != "medium" || added.ProviderSessionID != "" ||
		!reflect.DeepEqual(added.Files, wantFiles) {
		t.Fatalf("added member = %#v, want scoped resolved member without a session", added)
	}
	if grown.Scope != cohort.Scope || !grown.CreatedAt.Equal(cohort.CreatedAt) {
		t.Fatalf("grown cohort identity = %#v, want the saved cohort's scope and creation time", grown)
	}
}

func TestJoinRequiredReviewersWidensSavedRequiredMemberToNewMatches(t *testing.T) {
	catalog := requiredCohortCatalog()
	cohort := goOnlyCohort()
	cohort.Members = append(cohort.Members, ledger.ReviewerCohortMember{
		AgentID: "privacy:data", AssignmentMode: ledger.ReviewerAssignmentScoped,
		Files: []string{"main.go"}, AllowedFiles: []string{"main.go"},
		Model: "claude-sonnet-5-5", Effort: "medium", ProviderSessionID: "privacy-session",
	})
	req := Request{Profile: testProfile(""), ProfileName: "default"}
	// handler.go is new since the cohort was saved; rebasing hands it to the
	// first member whose globs match, which is not the required reviewer.
	changed := []string{"main.go", "handler.go"}

	rebased, _, err := rebaseReviewerCohort(req, catalog, cohort, changed, 0, "fake-llm")
	if err != nil {
		t.Fatalf("rebaseReviewerCohort: %v", err)
	}
	selection, grown, err := joinRequiredReviewers(req, catalog, cohort, rebased, changed, 0, fixedNow())
	if err != nil {
		t.Fatalf("joinRequiredReviewers: %v", err)
	}
	if grown != nil {
		t.Fatalf("grown cohort = %#v, want nil when no reviewer joined", grown)
	}
	for _, selected := range selection.SelectedAgents {
		if selected.AgentID == "privacy:data" {
			if !reflect.DeepEqual(selected.Files, changed) || !reflect.DeepEqual(selected.AllowedFiles, changed) {
				t.Fatalf("required member assignment = %#v, want every match %#v", selected, changed)
			}
			return
		}
	}
	t.Fatalf("selection = %#v, want the saved required member", selection.SelectedAgents)
}

func TestJoinRequiredReviewersLeavesSatisfiedCohortUnchanged(t *testing.T) {
	catalog := agents.Catalog{Agents: requiredCohortCatalog().Agents[:1]}
	cohort := goOnlyCohort()
	req := Request{Profile: testProfile(""), ProfileName: "default"}
	changed := []string{"main.go"}

	rebased, _, err := rebaseReviewerCohort(req, catalog, cohort, changed, 0, "fake-llm")
	if err != nil {
		t.Fatalf("rebaseReviewerCohort: %v", err)
	}
	selection, grown, err := joinRequiredReviewers(req, catalog, cohort, rebased, changed, 0, fixedNow())
	if err != nil {
		t.Fatalf("joinRequiredReviewers: %v", err)
	}
	if grown != nil || !reflect.DeepEqual(selection, rebased) {
		t.Fatalf("selection = %#v grown = %#v, want the rebased selection untouched", selection, grown)
	}
}

func TestJoinRequiredReviewersRejectsMaxAgentsBelowGrownCohort(t *testing.T) {
	catalog := requiredCohortCatalog()
	cohort := goOnlyCohort()
	req := Request{Profile: testProfile(""), ProfileName: "default"}
	changed := []string{"main.go"}

	rebased, _, err := rebaseReviewerCohort(req, catalog, cohort, changed, 1, "fake-llm")
	if err != nil {
		t.Fatalf("rebaseReviewerCohort: %v", err)
	}
	_, _, err = joinRequiredReviewers(req, catalog, cohort, rebased, changed, 1, fixedNow())
	if err == nil || !strings.Contains(err.Error(), "--max-agents 1") || !strings.Contains(err.Error(), "--fresh-session") {
		t.Fatalf("error = %v, want a max-agents conflict that points at --fresh-session", err)
	}
}

func TestCohortResumesRestoresSessionForReturningMember(t *testing.T) {
	cohort := goOnlyCohort()
	cohort.Members = append(cohort.Members, ledger.ReviewerCohortMember{AgentID: "privacy:data", ProviderSessionID: "privacy-session"})
	selection := llm.Selection{SelectedAgents: []llm.SelectedAgent{{AgentID: "lang:go"}, {AgentID: "privacy:data"}, {AgentID: "new:agent"}}}

	got := cohortResumes(cohort, selection, map[string]string{"lang:go": "go-session"})

	want := map[string]string{"lang:go": "go-session", "privacy:data": "privacy-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resumes = %#v, want %#v", got, want)
	}
}

// A required reviewer added to the catalog after a PR's first review must run
// on the next plain rerun, not only after --fresh-session, and must be saved
// into the cohort so its provider session can be recorded.
func TestPlainRerunRunsRequiredReviewerAddedAfterFirstReview(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	agentDir := t.TempDir()
	writeRequiredAgentForGlob(t, agentDir, "go", "**/*.go")
	trustCurrentTempFixtures(t)
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	req.Profile.AgentSources = []string{agentDir}
	layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
	actionIDs := actionSequence()
	options := func(adapter llm.Adapter, runID string) Options {
		return Options{
			Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
			NewRunID: func() string { return runID }, NewSessionRowID: sequence(runID + "-session"),
			NewFindingID: findingSequence(runID + "-finding"), NewActionID: actionIDs,
			MaxConcurrency: 1,
		}
	}

	first := &metadataHeavyAdapter{FakeAdapter: &llm.FakeAdapter{NameValue: "fake-llm"}}
	if _, err := dryRunForTest(ctx, options(first, "run-first"), req); err != nil {
		t.Fatalf("first DryRun: %v", err)
	}

	writeRequiredAgentForGlob(t, agentDir, "privacy", "**/*")
	second := &metadataHeavyAdapter{FakeAdapter: &llm.FakeAdapter{NameValue: "fake-llm"}}
	if _, err := dryRunForTest(ctx, options(second, "run-second"), req); err != nil {
		t.Fatalf("rerun DryRun: %v", err)
	}

	var schemas, reviewed []string
	for _, request := range second.Requests() {
		var payload struct {
			Schema     string                   `json:"schema"`
			Assignment reviewerPromptAssignment `json:"assignment"`
		}
		if err := json.Unmarshal([]byte(request.Prompt), &payload); err != nil {
			t.Fatalf("decode rerun prompt: %v", err)
		}
		schemas = append(schemas, payload.Schema)
		if payload.Schema == "findings" {
			reviewed = append(reviewed, payload.Assignment.AgentID)
		}
	}
	for _, schema := range schemas {
		if schema == "selection" {
			t.Fatalf("rerun schemas = %#v, want the saved cohort reused without a new selection", schemas)
		}
	}
	slices.Sort(reviewed)
	if want := []string{"harness:go", "harness:privacy"}; !reflect.DeepEqual(reviewed, want) {
		t.Fatalf("rerun reviewers = %#v, want saved member plus joined required reviewer %#v", reviewed, want)
	}

	prKey, err := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	cohort, err := store.GetReviewerCohort(ctx, ledger.ReviewerCohortScope{
		PRKey: prKey, Profile: req.ProfileName, PostingIdentity: runlifecycle.PostingKey(req.PostingIdentity),
	})
	if err != nil {
		t.Fatalf("GetReviewerCohort: %v", err)
	}
	var members []string
	for _, member := range cohort.Members {
		members = append(members, member.AgentID)
	}
	if want := []string{"harness:go", "harness:privacy"}; !reflect.DeepEqual(members, want) {
		t.Fatalf("cohort members = %#v, want the joined reviewer saved %#v", members, want)
	}
	if got := cohort.Members[1].ProviderSessionID; got != "metadata-heavy-reviewer-harness-privacy" {
		t.Fatalf("joined member session = %q, want the rerun's reviewer session recorded", got)
	}
}
