package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/dossier"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
	"github.com/open-cli-collective/codereview-cli/internal/symlinkmetadata"
)

func TestPinnedWithoutDiscussionRelocationsRepairWithoutPRSessions(t *testing.T) {
	ctx := context.Background()
	store := openPipelineStore(t)
	defer closeStore(t, store)
	provider, req, contextPath := newRelocationFixture(t, 2)
	req.ReviewBaseSHA, req.ReviewHeadSHA = provider.pr.Base.SHA, provider.pr.Head.SHA
	req.WithoutDiscussion = true
	provider.diffBetween = provider.diff
	provider.pr.Title = "Offline relocation replay"
	provider.pr.Body = "Keep the pinned relocation change visible."
	addExistingDiscussion(provider, req.PostingIdentity)
	sessionName, cohortScope := storePriorSessions(ctx, t, store, provider, req)
	cohort, err := store.GetReviewerCohort(ctx, cohortScope)
	if err != nil {
		t.Fatalf("seeded cohort: %v", err)
	}
	cohort.Members[0].AgentID = "harness:relocation"
	cohort.Members[0].Files = []string{"apps/components/shared/file-0000.go", "apps/components/shared/file-0001.go", "src/router.go", "scripts/build.sh", "workspace.yaml"}
	if err := store.ReplaceReviewerCohort(ctx, cohort); err != nil {
		t.Fatalf("seed relocation cohort: %v", err)
	}
	observedStore := &offlinePRSessionStore{Store: store, calls: map[string]int{}}
	adapter := &offlineRelocationAdapter{
		relocationWorkspaceAdapter: &relocationWorkspaceAdapter{
			FakeAdapter: &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true},
			contextPath: contextPath, omitPrimary: "src/router.go", repairResolve: true,
		},
		gitCommand: workbenchGitCommandForTest(req.PRRef, provider.fixtureRepoDir),
	}
	opts := withoutDiscussionOptions(t, provider, adapter, store, "run-offline-relocation-repair")
	opts.Store, opts.NamedSessions = observedStore, observedStore
	opts.KeepWorkbench = true
	withDiscussion := allocateDryRunForSHAs(t, store, opts.Layout, req, "run-pinned-with-discussion", req.ReviewHeadSHA, req.ReviewBaseSHA, fixedNow().Add(-time.Minute))

	result, err := dryRunForTest(ctx, opts, req)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if result.Run.RunID == withDiscussion.RunID || result.Run.RunID != "run-offline-relocation-repair" {
		t.Fatalf("run = %q, want fresh discussion-free run", result.Run.RunID)
	}
	withoutDiscussion := allocateDryRunForSHAs(t, store, opts.Layout, req, "run-pinned-without-discussion", req.ReviewHeadSHA, req.ReviewBaseSHA, fixedNow().Add(-time.Second))
	if err := runartifact.WriteMarkerWithOptions(withoutDiscussion.ArtifactPath, runartifact.KindReview, withoutDiscussion.RunID, runartifact.MarkerOptions{WithoutDiscussion: true}); err != nil {
		t.Fatalf("write discussion-free incomplete marker: %v", err)
	}
	for _, discussionFree := range []bool{false, true} {
		resumeReq := req
		resumeReq.WithoutDiscussion = discussionFree
		wantRun := withDiscussion.RunID
		if discussionFree {
			wantRun = withoutDiscussion.RunID
		}
		resumed, found, err := findIncompleteDryRun(ctx, observedStore, resumeReq, provider.pr)
		if err != nil || !found || resumed.RunID != wantRun {
			t.Fatalf("discussion-free %t resume = %q found %t error %v, want same-mode %q", discussionFree, resumed.RunID, found, err, wantRun)
		}
	}
	if !result.WithoutDiscussion || result.NamedSessionCandidate != nil || result.Plan.Outcome != reviewplan.OutcomeApproved {
		t.Fatalf("replay result = flag %t named session %#v outcome %q", result.WithoutDiscussion, result.NamedSessionCandidate, result.Plan.Outcome)
	}
	if provider.threadCalls != 0 || provider.reviewCalls != 0 || provider.issueCommentCalls != 0 {
		t.Fatalf("discussion reads = %d/%d/%d, want none", provider.threadCalls, provider.reviewCalls, provider.issueCommentCalls)
	}
	if len(result.ReviewerFailures) != 0 || len(result.ReviewerCoverage) != 1 {
		t.Fatalf("reviewer failures/coverage = %#v/%#v", result.ReviewerFailures, result.ReviewerCoverage)
	}
	coverage := result.ReviewerCoverage[0]
	if coverage.Status != reviewerCoverageCompleteConstrained || len(coverage.RelocationReviewedFiles) != 2 || len(coverage.MissingFiles) != 0 || len(coverage.SkippedFiles) != 0 {
		t.Fatalf("combined relocation/repair coverage = %#v, want both moves and complete residual coverage", coverage)
	}
	if !slices.Contains(coverage.InspectedFiles, "src/router.go") || !slices.Contains(coverage.ContextFiles, contextPath) {
		t.Fatalf("coverage omitted repaired residual or read context: %#v", coverage)
	}
	requests := adapter.Requests()
	counts := map[string]int{}
	var reviewerRequests []llm.Request
	for _, request := range requests {
		var prompt struct {
			Schema string `json:"schema"`
			Task   string `json:"task"`
		}
		if err := json.Unmarshal([]byte(request.Prompt), &prompt); err != nil {
			t.Fatalf("decode captured prompt: %v", err)
		}
		counts[prompt.Schema]++
		if prompt.Schema == "findings" {
			reviewerRequests = append(reviewerRequests, request)
			if strings.Contains(prompt.Task, "coverage repair") {
				counts["repair"]++
			}
		}
		for _, marker := range append(append([]string(nil), discussionMarkers...), "discussion_outcomes") {
			if strings.Contains(request.Prompt, marker) {
				t.Fatalf("%s prompt leaked discussion %q", prompt.Schema, marker)
			}
		}
	}
	if len(requests) != 4 || counts["selection"] != 1 || counts["findings"] != 2 || counts["repair"] != 1 || counts["rollup"] != 1 {
		t.Fatalf("task calls = %#v (%d total), want selection, primary, one repair, rollup", counts, len(requests))
	}
	if !strings.Contains(requests[0].Prompt, provider.pr.Title) || !strings.Contains(requests[0].Prompt, provider.pr.Body) {
		t.Fatal("selection lost the pinned PR title or body")
	}
	adapter.resumeMu.Lock()
	resumes := append([]llm.ResumeRequest(nil), adapter.resumes...)
	adapter.resumeMu.Unlock()
	if len(resumes) != 1 || resumes[0].SessionID != "relocation-findings" {
		t.Fatalf("resumes = %#v, want only repair resuming this invocation's primary session", resumes)
	}
	for _, resume := range resumes {
		if resume.SessionID == priorOrchestratorSession || resume.SessionID == priorReviewerSession {
			t.Fatalf("resumed seeded PR session %q", resume.SessionID)
		}
	}
	root, err := os.OpenRoot(result.Artifacts.Dir)
	if err != nil {
		t.Fatalf("open artifact root: %v", err)
	}
	defer root.Close()
	var manifest relocationManifest
	manifestBytes, err := root.ReadFile("relocations.json")
	if err != nil {
		t.Fatalf("read relocation artifact: %v", err)
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode relocation artifact: %v", err)
	}
	var artifact coverageArtifact
	coverageBytes, err := root.ReadFile("coverage.json")
	if err != nil {
		t.Fatalf("read coverage artifact: %v", err)
	}
	if err := json.Unmarshal(coverageBytes, &artifact); err != nil {
		t.Fatalf("decode coverage artifact: %v", err)
	}
	if len(manifest.Moves) != 2 || manifest.Digest == "" || manifest.BaseSHA != req.ReviewBaseSHA || manifest.HeadSHA != req.ReviewHeadSHA {
		t.Fatalf("pinned relocation manifest = %#v", manifest)
	}
	if artifact.ManifestDigest != manifest.Digest || !slices.Equal(artifact.Moves, manifest.Moves) || len(artifact.Reviewers) != 1 || len(artifact.Assessments) != 1 || len(artifact.Failures) != 0 {
		t.Fatalf("complete coverage artifact = %#v", artifact)
	}
	if !slices.Equal(artifact.Reviewers[0].RelocationReviewedFiles, coverage.RelocationReviewedFiles) || !slices.Equal(artifact.Reviewers[0].InspectedFiles, coverage.InspectedFiles) || !slices.Equal(artifact.Reviewers[0].ContextFiles, coverage.ContextFiles) || len(artifact.Reviewers[0].MissingFiles) != 0 {
		t.Fatalf("artifact lost combined coverage collections: %#v", artifact.Reviewers[0])
	}
	if len(artifact.Assessments[0].Assessments) != 1 || !artifact.Assessments[0].Assessments[0].Valid || len(artifact.Assessments[0].Assessments[0].ReviewedFiles) != 2 {
		t.Fatalf("artifact lost valid complete move assessment: %#v", artifact.Assessments)
	}
	marker, err := runartifact.ReadMarker(result.Artifacts.Dir, runartifact.KindReview)
	if err != nil || !marker.WithoutDiscussion {
		t.Fatalf("review marker = %#v, %v", marker, err)
	}
	summary, err := dossier.ReadDiscussionSummary(result.Artifacts)
	if err != nil || !summary.WithoutDiscussion || len(summary.TopLevelComments) != 0 || len(summary.InlineThreads) != 0 {
		t.Fatalf("discussion summary = %#v, %v", summary, err)
	}
	for _, marker := range discussionMarkers {
		if strings.Contains(dossierText(t, result.Artifacts.DossierDir), marker) {
			t.Fatalf("dossier leaked discussion %q", marker)
		}
	}
	var symlinks symlinkmetadata.Artifact
	symlinkBytes, err := root.ReadFile("symlink-metadata.json")
	if err != nil {
		t.Fatalf("read symlink artifact: %v", err)
	}
	if err := json.Unmarshal(symlinkBytes, &symlinks); err != nil || symlinks.Digest == "" || symlinks.BaseSHA != req.ReviewBaseSHA || symlinks.HeadSHA != req.ReviewHeadSHA {
		t.Fatalf("pinned symlink metadata = %#v, %v", symlinks, err)
	}
	for _, request := range reviewerRequests {
		workspace := request.ReviewerWorkspace
		if workspace == nil || workspace.SymlinkMetadataPath != result.Artifacts.SymlinkMetadataJSON || workspace.SymlinkMetadataDigest != symlinks.Digest || workspace.BaseSHA != symlinks.BaseSHA || workspace.HeadSHA != symlinks.HeadSHA {
			t.Fatalf("offline primary/repair lost pinned symlink identity: %#v", workspace)
		}
	}
	assertOfflineRelocationFingerprints(ctx, t, opts, result, reviewerRequests, manifest.Digest, symlinks.Digest)
	observedStore.mu.Lock()
	for method, calls := range observedStore.calls {
		if calls != 0 {
			t.Errorf("PR-scoped %s calls = %d, want zero", method, calls)
		}
	}
	observedStore.mu.Unlock()
	storedSession, err := store.GetNamedSession(ctx, sessionName)
	if err != nil || storedSession.ProviderSessionID != priorOrchestratorSession {
		t.Fatalf("seeded orchestrator session changed: %#v, %v", storedSession, err)
	}
	storedCohort, err := store.GetReviewerCohort(ctx, cohortScope)
	if err != nil || len(storedCohort.Members) != 1 || storedCohort.Members[0].ProviderSessionID != priorReviewerSession || !slices.Equal(storedCohort.Members[0].Files, cohort.Members[0].Files) {
		t.Fatalf("seeded reviewer cohort changed: %#v, %v", storedCohort, err)
	}
}

func assertOfflineRelocationFingerprints(ctx context.Context, t *testing.T, opts Options, result Result, requests []llm.Request, manifestDigest, symlinkDigest string) {
	t.Helper()
	core, err := loadDossierPromptCore(result.Artifacts)
	if err != nil {
		t.Fatalf("load fingerprint inputs: %v", err)
	}
	for i, request := range requests {
		var prompt struct {
			Assignment   reviewerPromptAssignment `json:"assignment"`
			FileManifest promptFileManifest       `json:"file_manifest"`
		}
		if err := json.Unmarshal([]byte(request.Prompt), &prompt); err != nil {
			t.Fatalf("decode reviewer assignment: %v", err)
		}
		taskID := reviewerTaskID(prompt.Assignment.AgentID)
		wantDeps := []string{orchestratorSelectionStage}
		wantMoves := 2
		if i == 1 {
			taskID = reviewerCoverageRepairTaskID(prompt.Assignment.AgentID)
			wantDeps = []string{reviewerTaskID(prompt.Assignment.AgentID)}
			wantMoves = 0
			if !slices.Equal(promptManifestPathsAtIndices(t, prompt.FileManifest, prompt.Assignment.ScopeIndices), []string{"src/router.go"}) {
				t.Fatalf("repair scope = %#v, want only omitted residual", prompt.Assignment)
			}
		}
		if prompt.Assignment.ManifestDigest != manifestDigest || prompt.Assignment.AssignmentDigest == "" || prompt.Assignment.RelocationCount != wantMoves {
			t.Fatalf("task %s relocation assignment = %#v", taskID, prompt.Assignment)
		}
		meta, ok, err := llmlifecycle.ReadMetadata(lifecyclePaths(result.Artifacts), taskID)
		if err != nil || !ok || meta.SchemaVersion != llmlifecycle.SchemaVersion || meta.Status != llmlifecycle.StatusSucceeded || !slices.Equal(meta.DependencyTaskIDs, wantDeps) {
			t.Fatalf("task metadata = %#v, found %t, error %v", meta, ok, err)
		}
		deps := append(append([]string(nil), wantDeps...), core.Dependencies...)
		deps = append(deps, "relocation-manifest="+prompt.Assignment.ManifestDigest, "relocation-assignment="+prompt.Assignment.AssignmentDigest, "symlink-metadata="+symlinkDigest, "symlink-contract="+symlinkInspectionContractVersion, "context-contract="+reviewerContextContractVersion, "without_discussion=true")
		fingerprint := func(values []string) string {
			return llmlifecycle.Fingerprint(opts.Adapter.Name(), taskID, meta.Phase, request.Model, request.Effort, request.Prompt, values)
		}
		if meta.InputFingerprint != fingerprint(deps) {
			t.Fatalf("task %s fingerprint = %q, want all relocation/symlink/context/discussion dependencies %q", taskID, meta.InputFingerprint, fingerprint(deps))
		}
		cacheReq := llmlifecycle.Request{Store: opts.Store, Adapter: opts.Adapter, RunID: result.Run.RunID, TaskID: taskID, Phase: meta.Phase, DependencyTaskIDs: wantDeps, InputFingerprint: meta.InputFingerprint, Paths: lifecyclePaths(result.Artifacts), Model: request.Model, Effort: request.Effort, Prompt: request.Prompt}
		decode := func(data []byte) (json.RawMessage, error) { return append(json.RawMessage(nil), data...), nil }
		if _, loaded, err := llmlifecycle.LoadStructured(ctx, cacheReq, decode); err != nil || !loaded {
			t.Fatalf("unchanged %s cache = loaded %t, error %v", taskID, loaded, err)
		}
		for _, prefix := range []string{"relocation-manifest=", "relocation-assignment=", "symlink-metadata=", "symlink-contract=", "context-contract=", "without_discussion="} {
			changed := append([]string(nil), deps...)
			for index, dep := range changed {
				if strings.HasPrefix(dep, prefix) {
					changed[index] = prefix + "changed"
				}
			}
			cacheReq.InputFingerprint = fingerprint(changed)
			if _, _, err := llmlifecycle.LoadStructured(ctx, cacheReq, decode); err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
				t.Fatalf("%s cache accepted changed %s dependency: %v", taskID, prefix, err)
			}
		}
		cacheReq.InputFingerprint = meta.InputFingerprint
		for _, version := range []int{1, 2, 3} {
			legacy := meta
			legacy.SchemaVersion = version
			if err := llmlifecycle.WriteMetadata(cacheReq.Paths, legacy); err != nil {
				t.Fatalf("write schema-v%d cache: %v", version, err)
			}
			if _, _, err := llmlifecycle.LoadStructured(ctx, cacheReq, decode); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("schema version = %d", version)) {
				t.Fatalf("%s cache accepted schema-v%d metadata: %v", taskID, version, err)
			}
		}
		if err := llmlifecycle.WriteMetadata(cacheReq.Paths, meta); err != nil {
			t.Fatalf("restore current metadata: %v", err)
		}
	}
}

// Resume runs the same dynamic relocation response as Start. Only the focused
// repair may resume the fresh primary session created by this invocation.
type offlineRelocationAdapter struct {
	*relocationWorkspaceAdapter
	gitCommand func(context.Context, string, ...string) ([]byte, error)
	resumeMu   sync.Mutex
	resumes    []llm.ResumeRequest
}

func (a *offlineRelocationAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if workspace := req.ReviewerWorkspace; workspace != nil {
		if !workspace.NoNetwork {
			return nil, fmt.Errorf("offline relocation reviewer has network tools enabled")
		}
		remotes, err := a.gitCommand(ctx, workspace.RepoDir, "remote")
		if err != nil {
			return nil, fmt.Errorf("list offline relocation reviewer remotes: %w", err)
		}
		if strings.TrimSpace(string(remotes)) != "" {
			return nil, fmt.Errorf("offline relocation reviewer remotes = %q, want none", remotes)
		}
	}
	return a.relocationWorkspaceAdapter.Start(ctx, req)
}

func (a *offlineRelocationAdapter) Resume(ctx context.Context, sessionID string, req llm.Request) (llm.Stream, error) {
	a.resumeMu.Lock()
	a.resumes = append(a.resumes, llm.ResumeRequest{SessionID: sessionID, Request: req})
	a.resumeMu.Unlock()
	return a.Start(ctx, req)
}

// Record every PR-scoped operation separately from allowed run-owned ledger
// sessions, so an empty or incompatible saved cohort cannot hide a lookup.
type offlinePRSessionStore struct {
	*ledger.Store
	mu    sync.Mutex
	calls map[string]int
}

func (s *offlinePRSessionStore) record(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[method]++
}

func (s *offlinePRSessionStore) GetNamedSession(ctx context.Context, name string) (ledger.NamedSession, error) {
	s.record("GetNamedSession")
	return s.Store.GetNamedSession(ctx, name)
}

func (s *offlinePRSessionStore) UpsertNamedSession(ctx context.Context, session ledger.NamedSession) error {
	s.record("UpsertNamedSession")
	return s.Store.UpsertNamedSession(ctx, session)
}

func (s *offlinePRSessionStore) DeleteNamedSession(ctx context.Context, name string) error {
	s.record("DeleteNamedSession")
	return s.Store.DeleteNamedSession(ctx, name)
}

func (s *offlinePRSessionStore) GetReviewerCohort(ctx context.Context, scope ledger.ReviewerCohortScope) (ledger.ReviewerCohort, error) {
	s.record("GetReviewerCohort")
	return s.Store.GetReviewerCohort(ctx, scope)
}

func (s *offlinePRSessionStore) ReplaceReviewerCohort(ctx context.Context, cohort ledger.ReviewerCohort) error {
	s.record("ReplaceReviewerCohort")
	return s.Store.ReplaceReviewerCohort(ctx, cohort)
}

func (s *offlinePRSessionStore) UpdateReviewerCohortSession(ctx context.Context, scope ledger.ReviewerCohortScope, agentID, sessionID string, at time.Time) error {
	s.record("UpdateReviewerCohortSession")
	return s.Store.UpdateReviewerCohortSession(ctx, scope, agentID, sessionID, at)
}

func (s *offlinePRSessionStore) DeleteReviewerCohort(ctx context.Context, scope ledger.ReviewerCohortScope) error {
	s.record("DeleteReviewerCohort")
	return s.Store.DeleteReviewerCohort(ctx, scope)
}
