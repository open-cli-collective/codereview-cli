package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/symlinkmetadata"
)

func TestPinnedWithoutDiscussionPreservesSymlinkInspectionObligations(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        string
		readBody    bool
		wantOutcome reviewplan.Outcome
	}{
		{name: "head symlink payload", kind: "symlink", wantOutcome: reviewplan.OutcomeApproved},
		{name: "historical metadata is not head body", kind: "symlink-to-regular", wantOutcome: reviewplan.OutcomeComment},
		{name: "transition head body inspected", kind: "symlink-to-regular", readBody: true, wantOutcome: reviewplan.OutcomeApproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req, contextPath, linkPath, residualPath := newPatchlessRenameFixture(t, tc.kind)
			req.ReviewBaseSHA, req.ReviewHeadSHA = provider.pr.Base.SHA, provider.pr.Head.SHA
			req.WithoutDiscussion = true
			provider.diffBetween = provider.diff
			addExistingDiscussion(provider, req.PostingIdentity)
			sessionName, cohortScope := storePriorSessions(ctx, t, store, provider, req)
			observedStore := &offlinePRSessionStore{Store: store, calls: map[string]int{}}
			adapter := &offlineRelocationAdapter{
				relocationWorkspaceAdapter: &relocationWorkspaceAdapter{
					FakeAdapter: &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true},
					contextPath: contextPath, inspectSymlink: true, readBaseOnlyBody: tc.readBody,
				},
				gitCommand: workbenchGitCommandForTest(req.PRRef, provider.fixtureRepoDir),
			}
			opts := withoutDiscussionOptions(t, provider, adapter, store, "run-offline-symlink")
			opts.Store, opts.NamedSessions = observedStore, observedStore
			result, err := dryRunForTest(ctx, opts, req)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			if !result.WithoutDiscussion || result.NamedSessionCandidate != nil || result.Plan.Outcome != tc.wantOutcome || len(result.ReviewerCoverage) != 1 || len(result.ReviewerFailures) != 0 {
				t.Fatalf("offline symlink result = %#v, want outcome %q and one reviewer without failures", result, tc.wantOutcome)
			}
			coverage := result.ReviewerCoverage[0]
			wantInspected := tc.kind == "symlink" || tc.readBody
			if slices.Contains(coverage.InspectedFiles, linkPath) != wantInspected || !slices.Contains(coverage.InspectedFiles, residualPath) || len(coverage.RelocationReviewedFiles) != 0 {
				t.Fatalf("offline symlink inspection coverage = %#v", coverage)
			}
			if !wantInspected && (!slices.Contains(coverage.SkippedFiles, linkPath) || !slices.Contains(coverage.MissingFiles, linkPath)) {
				t.Fatalf("historical symlink metadata cleared the head-body obligation: %#v", coverage)
			}
			adapter.mu.Lock()
			metadataReads := append([]string(nil), adapter.metadataReads...)
			bodyReads := append([]string(nil), adapter.bodyReads...)
			adapter.mu.Unlock()
			if !slices.Contains(metadataReads, linkPath) || slices.Contains(bodyReads, linkPath) != tc.readBody {
				t.Fatalf("offline symlink read modes = metadata %#v/body %#v", metadataReads, bodyReads)
			}
			root, err := os.OpenRoot(result.Artifacts.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			data, err := root.ReadFile("symlink-metadata.json")
			if err != nil {
				t.Fatal(err)
			}
			var metadata symlinkmetadata.Artifact
			if err := json.Unmarshal(data, &metadata); err != nil || metadata.Digest == "" || len(metadata.Links) != 1 || metadata.Links[0].Path != linkPath {
				t.Fatalf("offline pinned metadata = %#v, error %v", metadata, err)
			}
			var reviewers int
			for _, request := range adapter.Requests() {
				workspace := request.ReviewerWorkspace
				if workspace == nil {
					continue
				}
				reviewers++
				if !workspace.NoNetwork || workspace.SymlinkMetadataPath != result.Artifacts.SymlinkMetadataJSON || workspace.SymlinkMetadataDigest != metadata.Digest || workspace.BaseSHA != req.ReviewBaseSHA || workspace.HeadSHA != req.ReviewHeadSHA {
					t.Fatalf("offline reviewer lost metadata identity: %#v", workspace)
				}
			}
			if reviewers == 0 || provider.threadCalls != 0 || provider.reviewCalls != 0 || provider.issueCommentCalls != 0 {
				t.Fatalf("reviewers %d, discussion reads %d/%d/%d", reviewers, provider.threadCalls, provider.reviewCalls, provider.issueCommentCalls)
			}
			observedStore.mu.Lock()
			for method, count := range observedStore.calls {
				if count != 0 {
					t.Errorf("PR-scoped %s calls = %d, want zero", method, count)
				}
			}
			observedStore.mu.Unlock()
			session, err := store.GetNamedSession(ctx, sessionName)
			if err != nil || session.ProviderSessionID != priorOrchestratorSession {
				t.Fatalf("saved orchestrator session changed: %#v, %v", session, err)
			}
			cohort, err := store.GetReviewerCohort(ctx, cohortScope)
			if err != nil || len(cohort.Members) != 1 || cohort.Members[0].ProviderSessionID != priorReviewerSession {
				t.Fatalf("saved reviewer cohort changed: %#v, %v", cohort, err)
			}
		})
	}
}
