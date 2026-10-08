package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/pireviewtool"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const largeRelocationMoveCount = 2500

func TestLargeRelocationDryRunAndLiveUseWorkspaceContextAndVerifiedImpact(t *testing.T) {
	for _, mode := range []string{"dry-run", "live"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req, contextPath := newLargeRelocationFixture(t)
			adapter := &relocationWorkspaceAdapter{FakeAdapter: &llm.FakeAdapter{NameValue: "relocation-test"}, contextPath: contextPath}
			layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
			newRunID := "run-large-relocations-" + mode
			var result Result
			var err error
			if mode == "dry-run" {
				result, err = dryRunForTest(ctx, Options{
					Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
					NewRunID: func() string { return newRunID }, NewSessionRowID: sequence("relocation-session"),
					NewFindingID: findingSequence("relocation-finding"), NewActionID: actionSequence(), MaxConcurrency: 1,
				}, req)
			} else {
				prKey, keyErr := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
				if keyErr != nil {
					t.Fatal(keyErr)
				}
				run, allocateErr := store.AllocateRun(ctx, ledger.AllocateRunParams{
					PRKey: prKey, PRURL: req.PRURL, RunID: newRunID, SHA: provider.pr.Head.SHA,
					BaseSHA: provider.pr.Base.SHA, Profile: req.ProfileName,
					PostingIdentity: req.PostingIdentity.Login, PostMode: ledger.PostModeLive,
					StartedAt: fixedNow(), ArtifactPath: filepath.Join(t.TempDir(), "live-run"),
				})
				if allocateErr != nil {
					t.Fatalf("AllocateRun: %v", allocateErr)
				}
				result, err = liveForTest(ctx, Options{
					Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
					NewSessionRowID: sequence("relocation-session"), NewFindingID: findingSequence("relocation-finding"),
					NewActionID: actionSequence(), MaxConcurrency: 1,
				}, req, run)
			}
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			if result.Plan.Outcome != reviewplan.OutcomeApproved {
				t.Fatalf("%s outcome = %q, want approve after complete residual and relocation coverage", mode, result.Plan.Outcome)
			}
			if len(result.ReviewerCoverage) != 1 {
				t.Fatalf("%s coverage = %#v, want one reviewer", mode, result.ReviewerCoverage)
			}
			coverage := result.ReviewerCoverage[0]
			if coverage.Status != reviewerCoverageCompleteConstrained || len(coverage.RelocationReviewedFiles) != largeRelocationMoveCount {
				t.Fatalf("%s relocation coverage = status %q, reviewed %d; want complete and %d moves", mode, coverage.Status, len(coverage.RelocationReviewedFiles), largeRelocationMoveCount)
			}
			if len(coverage.InspectedFiles) != 3 || !relocationTestContainsString(coverage.ContextFiles, contextPath) || len(coverage.MissingFiles) != 0 {
				t.Fatalf("%s residual/context/missing coverage = inspected %d context %#v missing %#v", mode, len(coverage.InspectedFiles), coverage.ContextFiles, coverage.MissingFiles)
			}
			adapter.mu.Lock()
			workspaceReads := append([]string(nil), adapter.workspaceReads...)
			adapter.mu.Unlock()
			if !relocationTestContainsString(workspaceReads, contextPath) {
				t.Fatalf("%s reviewer did not read cited context %q from ReviewerWorkspace.RepoDir: %#v", mode, contextPath, workspaceReads)
			}
			requests := adapter.Requests()
			if len(requests) < 3 || len(requests) > 4 {
				t.Fatalf("%s adapter requests = %d, want selection/reviewer/rollup and optional dossier without repair", mode, len(requests))
			}
			var manifest relocationManifest
			data, readErr := os.ReadFile(result.Artifacts.RelocationsJSON)
			if readErr != nil {
				t.Fatalf("read relocations.json: %v", readErr)
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatalf("decode relocations.json: %v", err)
			}
			if len(manifest.Moves) != largeRelocationMoveCount || manifest.Digest == "" {
				t.Fatalf("manifest has %d moves and digest %q, want %d certified moves", len(manifest.Moves), manifest.Digest, largeRelocationMoveCount)
			}
			coverageData, readErr := os.ReadFile(result.Artifacts.CoverageJSON)
			if readErr != nil {
				t.Fatalf("read coverage.json: %v", readErr)
			}
			if !strings.Contains(string(coverageData), manifest.Digest) || !strings.Contains(string(coverageData), contextPath) {
				t.Fatalf("coverage.json omitted manifest digest or context read: %s", string(coverageData[:min(len(coverageData), 1000)]))
			}
			var reviewerPrompt string
			requestSchemas := map[string]int{}
			for _, request := range requests {
				if strings.Contains(request.Prompt, "blob_oid") || strings.Contains(request.Prompt, `"relocations":[`) {
					t.Fatalf("%s prompt serialized full relocation proof instead of compact metadata", mode)
				}
				var envelope struct {
					Schema string `json:"schema"`
				}
				if err := json.Unmarshal([]byte(request.Prompt), &envelope); err != nil {
					t.Fatalf("decode prompt schema: %v", err)
				}
				requestSchemas[envelope.Schema]++
				if envelope.Schema == "findings" {
					reviewerPrompt = request.Prompt
				}
			}
			for _, schema := range []string{"selection", "findings", "rollup"} {
				if requestSchemas[schema] != 1 {
					t.Fatalf("%s prompt schemas = %#v, want one %s task and no coverage repair", mode, requestSchemas, schema)
				}
			}
			if requestSchemas["coverage_repair"] != 0 || reviewerPrompt == "" {
				t.Fatalf("%s prompt schemas = %#v, want no repair and one findings task", mode, requestSchemas)
			}
			var reviewerPromptEnvelope struct {
				Assignment   reviewerPromptAssignment `json:"assignment"`
				FileManifest promptFileManifest       `json:"file_manifest"`
			}
			if err := json.Unmarshal([]byte(reviewerPrompt), &reviewerPromptEnvelope); err != nil {
				t.Fatalf("decode reviewer prompt: %v", err)
			}
			if reviewerPromptEnvelope.Assignment.RelocationCount != largeRelocationMoveCount || reviewerPromptEnvelope.Assignment.ManifestDigest != manifest.Digest || reviewerPromptEnvelope.Assignment.AssignmentDigest == "" {
				t.Fatalf("compact assignment metadata = %#v", reviewerPromptEnvelope.Assignment)
			}
			if len(reviewerPromptEnvelope.FileManifest.Rows) < largeRelocationMoveCount {
				t.Fatalf("reviewer manifest rows = %d, want at least %d", len(reviewerPromptEnvelope.FileManifest.Rows), largeRelocationMoveCount)
			}
			foundVerifiedRelocation := false
			for _, row := range reviewerPromptEnvelope.FileManifest.Rows {
				if len(row) > 8 && row[8] == true {
					foundVerifiedRelocation = true
					break
				}
			}
			if !foundVerifiedRelocation {
				t.Fatalf("verified relocation marker absent from manifest rows")
			}
		})
	}
}

func TestPatchlessSameBlobRenamesRemainOrdinaryReviewObligations(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		wantOutcome reviewplan.Outcome
	}{
		{name: "mode-changing regular rename is body-read", kind: "mode", wantOutcome: reviewplan.OutcomeApproved},
		{name: "unchanged symlink rename is skipped and withholds", kind: "symlink", wantOutcome: reviewplan.OutcomeComment},
	}
	for _, tc := range cases {
		for _, mode := range []string{"dry-run", "live"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				store := openPipelineStore(t)
				defer closeStore(t, store)
				provider, req, contextPath, renamePath, residualPath := newPatchlessRenameFixture(t, tc.kind)
				adapter := &relocationWorkspaceAdapter{
					FakeAdapter: &llm.FakeAdapter{NameValue: "relocation-test"}, contextPath: contextPath,
				}
				if tc.kind == "symlink" {
					adapter.skipPrimary = renamePath
				}
				layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
				runID := "run-patchless-" + tc.kind + "-" + mode
				var result Result
				var err error
				if mode == "dry-run" {
					result, err = dryRunForTest(ctx, Options{
						Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
						NewRunID: func() string { return runID }, NewSessionRowID: sequence("patchless-session"),
						NewFindingID: findingSequence("patchless-finding"), NewActionID: actionSequence(), MaxConcurrency: 1,
					}, req)
				} else {
					prKey, keyErr := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
					if keyErr != nil {
						t.Fatal(keyErr)
					}
					run, allocateErr := store.AllocateRun(ctx, ledger.AllocateRunParams{
						PRKey: prKey, PRURL: req.PRURL, RunID: runID, SHA: provider.pr.Head.SHA,
						BaseSHA: provider.pr.Base.SHA, Profile: req.ProfileName,
						PostingIdentity: req.PostingIdentity.Login, PostMode: ledger.PostModeLive,
						StartedAt: fixedNow(), ArtifactPath: filepath.Join(t.TempDir(), "live-run"),
					})
					if allocateErr != nil {
						t.Fatalf("AllocateRun: %v", allocateErr)
					}
					result, err = liveForTest(ctx, Options{
						Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
						NewSessionRowID: sequence("patchless-session"), NewFindingID: findingSequence("patchless-finding"),
						NewActionID: actionSequence(), MaxConcurrency: 1,
					}, req, run)
				}
				if err != nil {
					t.Fatalf("%s preparation/review: %v", mode, err)
				}
				if result.Plan.Outcome != tc.wantOutcome {
					t.Fatalf("%s outcome = %q, want %q", mode, result.Plan.Outcome, tc.wantOutcome)
				}
				if len(result.ReviewerCoverage) != 1 {
					t.Fatalf("coverage = %#v, want one reviewer", result.ReviewerCoverage)
				}
				coverage := result.ReviewerCoverage[0]
				if len(coverage.RelocationReviewedFiles) != 0 {
					t.Fatalf("ordinary rename received relocation coverage: %#v", coverage.RelocationReviewedFiles)
				}
				if !relocationTestContainsString(coverage.InspectedFiles, residualPath) {
					t.Fatalf("unrelated changed file %q was not body-inspected: %#v", residualPath, coverage.InspectedFiles)
				}
				adapter.mu.Lock()
				bodyReads := append([]string(nil), adapter.bodyReads...)
				adapter.mu.Unlock()
				if tc.kind == "mode" {
					if !relocationTestContainsString(coverage.InspectedFiles, renamePath) || !relocationTestContainsString(bodyReads, renamePath) {
						t.Fatalf("regular mode-changing rename was not assigned and read: coverage=%#v body reads=%#v", coverage, bodyReads)
					}
					if coverage.Status != reviewerCoverageCompleteBroad && coverage.Status != reviewerCoverageCompleteConstrained {
						t.Fatalf("regular mode-changing rename coverage status = %q, want complete", coverage.Status)
					}
				} else {
					if !relocationTestContainsString(coverage.SkippedFiles, renamePath) || !relocationTestContainsString(coverage.MissingFiles, renamePath) {
						t.Fatalf("symlink rename skip was not preserved as an unresolved obligation: %#v", coverage)
					}
					if relocationTestContainsString(bodyReads, renamePath) {
						t.Fatalf("test reviewer attempted to read symlink path %q: %#v", renamePath, bodyReads)
					}
					if coverage.Status != reviewerCoverageIncompleteSkipped {
						t.Fatalf("symlink rename coverage status = %q, want incomplete_skipped", coverage.Status)
					}
				}
				var manifest relocationManifest
				data, readErr := os.ReadFile(result.Artifacts.RelocationsJSON)
				if readErr != nil {
					t.Fatalf("read relocations.json: %v", readErr)
				}
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Fatalf("decode relocations.json: %v", err)
				}
				if len(manifest.Moves) != 0 {
					t.Fatalf("manifest certified ordinary rename: %#v", manifest.Moves)
				}
			})
		}
	}
}

func TestChangedSymlinkMetadataInspectionPreservesOrdinaryCoverageInDryRunAndLive(t *testing.T) {
	for _, mode := range []string{"dry-run", "live"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req, contextPath, symlinkPath, residualPath := newPatchlessRenameFixture(t, "symlink")
			adapter := &relocationWorkspaceAdapter{FakeAdapter: &llm.FakeAdapter{NameValue: "symlink-test"}, contextPath: contextPath, inspectSymlink: true}
			layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
			runID := "run-symlink-inspection-" + mode
			var result Result
			var err error
			if mode == "dry-run" {
				result, err = dryRunForTest(ctx, Options{
					Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
					NewRunID: func() string { return runID }, NewSessionRowID: sequence("symlink-session"),
					NewFindingID: findingSequence("symlink-finding"), NewActionID: actionSequence(), MaxConcurrency: 1,
				}, req)
			} else {
				prKey, keyErr := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
				if keyErr != nil {
					t.Fatal(keyErr)
				}
				run, allocateErr := store.AllocateRun(ctx, ledger.AllocateRunParams{
					PRKey: prKey, PRURL: req.PRURL, RunID: runID, SHA: provider.pr.Head.SHA,
					BaseSHA: provider.pr.Base.SHA, Profile: req.ProfileName,
					PostingIdentity: req.PostingIdentity.Login, PostMode: ledger.PostModeLive,
					StartedAt: fixedNow(), ArtifactPath: filepath.Join(t.TempDir(), "live-run"),
				})
				if allocateErr != nil {
					t.Fatalf("AllocateRun: %v", allocateErr)
				}
				result, err = liveForTest(ctx, Options{
					Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
					NewSessionRowID: sequence("symlink-session"), NewFindingID: findingSequence("symlink-finding"),
					NewActionID: actionSequence(), MaxConcurrency: 1,
				}, req, run)
			}
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			if result.Plan.Outcome != reviewplan.OutcomeApproved || len(result.ReviewerCoverage) != 1 {
				t.Fatalf("%s outcome/coverage = %q/%#v, want approved with one complete reviewer", mode, result.Plan.Outcome, result.ReviewerCoverage)
			}
			coverage := result.ReviewerCoverage[0]
			if coverage.Status != reviewerCoverageCompleteBroad && coverage.Status != reviewerCoverageCompleteConstrained {
				t.Fatalf("%s coverage status = %q, want complete", mode, coverage.Status)
			}
			if !relocationTestContainsString(coverage.InspectedFiles, symlinkPath) || !relocationTestContainsString(coverage.InspectedFiles, residualPath) || len(coverage.MissingFiles) != 0 || len(coverage.RelocationReviewedFiles) != 0 {
				t.Fatalf("%s coverage = %#v, want payload and residual reported inspected without relocation credit", mode, coverage)
			}
			adapter.mu.Lock()
			metadataReads := append([]string(nil), adapter.metadataReads...)
			bodyReads := append([]string(nil), adapter.bodyReads...)
			adapter.mu.Unlock()
			if !relocationTestContainsString(metadataReads, symlinkPath) || relocationTestContainsString(bodyReads, symlinkPath) {
				t.Fatalf("%s symlink read modes = metadata %#v/body %#v; want metadata-only inspection", mode, metadataReads, bodyReads)
			}
			var metadata struct {
				Digest string `json:"digest"`
				Links  []struct {
					Path string `json:"path"`
				} `json:"links"`
			}
			data, readErr := os.ReadFile(result.Artifacts.SymlinkMetadataJSON)
			if readErr != nil {
				t.Fatalf("read run symlink metadata artifact: %v", readErr)
			}
			if err := json.Unmarshal(data, &metadata); err != nil || metadata.Digest == "" || len(metadata.Links) != 1 || metadata.Links[0].Path != symlinkPath {
				t.Fatalf("run symlink metadata = %#v, decode error %v", metadata, err)
			}
			requests := adapter.Requests()
			foundReviewer := false
			for _, request := range requests {
				if request.ReviewerWorkspace == nil {
					continue
				}
				foundReviewer = true
				workspace := request.ReviewerWorkspace
				if workspace.SymlinkMetadataPath != result.Artifacts.SymlinkMetadataJSON || workspace.SymlinkMetadataDigest != metadata.Digest || workspace.BaseSHA != provider.pr.Base.SHA || workspace.HeadSHA != provider.pr.Head.SHA {
					t.Fatalf("%s pinned metadata workspace = %#v", mode, workspace)
				}
				if !strings.Contains(request.Prompt, "view=symlink") || !strings.Contains(request.Prompt, metadata.Digest) {
					t.Fatalf("%s reviewer prompt omitted symlink inspection contract/digest", mode)
				}
			}
			if !foundReviewer {
				t.Fatalf("%s adapter did not receive reviewer workspace request", mode)
			}
		})
	}
}

func TestSymlinkToRegularTransitionRequiresPinnedHeadBodyInDryRunAndLive(t *testing.T) {
	for _, mode := range []string{"dry-run", "live"} {
		for _, tc := range []struct {
			name        string
			readBody    bool
			wantOutcome reviewplan.Outcome
		}{
			{name: "metadata only remains unresolved", wantOutcome: reviewplan.OutcomeComment},
			{name: "ordinary head body completes coverage", readBody: true, wantOutcome: reviewplan.OutcomeApproved},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				store := openPipelineStore(t)
				defer closeStore(t, store)
				provider, req, contextPath, transitionPath, residualPath := newPatchlessRenameFixture(t, "symlink-to-regular")
				adapter := &relocationWorkspaceAdapter{
					FakeAdapter: &llm.FakeAdapter{NameValue: "symlink-transition-test"},
					contextPath: contextPath, inspectSymlink: true, readBaseOnlyBody: tc.readBody,
				}
				layout := statepaths.NewLayout(t.TempDir(), t.TempDir())
				runID := "run-symlink-transition-" + mode + "-" + strings.ReplaceAll(tc.name, " ", "-")
				var result Result
				var err error
				if mode == "dry-run" {
					result, err = dryRunForTest(ctx, Options{
						Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
						NewRunID: func() string { return runID }, NewSessionRowID: sequence("symlink-transition-session"),
						NewFindingID: findingSequence("symlink-transition-finding"), NewActionID: actionSequence(), MaxConcurrency: 1,
					}, req)
				} else {
					prKey, keyErr := statepaths.PRKey(req.PRRef.Host, req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number)
					if keyErr != nil {
						t.Fatal(keyErr)
					}
					run, allocateErr := store.AllocateRun(ctx, ledger.AllocateRunParams{
						PRKey: prKey, PRURL: req.PRURL, RunID: runID, SHA: provider.pr.Head.SHA,
						BaseSHA: provider.pr.Base.SHA, Profile: req.ProfileName,
						PostingIdentity: req.PostingIdentity.Login, PostMode: ledger.PostModeLive,
						StartedAt: fixedNow(), ArtifactPath: filepath.Join(t.TempDir(), "live-run"),
					})
					if allocateErr != nil {
						t.Fatalf("AllocateRun: %v", allocateErr)
					}
					result, err = liveForTest(ctx, Options{
						Provider: provider, Adapter: adapter, Store: store, Layout: layout, Now: fixedNow,
						NewSessionRowID: sequence("symlink-transition-session"), NewFindingID: findingSequence("symlink-transition-finding"),
						NewActionID: actionSequence(), MaxConcurrency: 1,
					}, req, run)
				}
				if err != nil {
					t.Fatalf("%s transition review: %v", mode, err)
				}
				if result.Plan.Outcome != tc.wantOutcome || len(result.ReviewerCoverage) != 1 {
					t.Fatalf("%s outcome/coverage = %q/%#v, want %q and one reviewer", mode, result.Plan.Outcome, result.ReviewerCoverage, tc.wantOutcome)
				}
				coverage := result.ReviewerCoverage[0]
				adapter.mu.Lock()
				metadataReads := append([]string(nil), adapter.metadataReads...)
				bodyReads := append([]string(nil), adapter.bodyReads...)
				adapter.mu.Unlock()
				if !relocationTestContainsString(metadataReads, transitionPath) {
					t.Fatalf("%s did not inspect historical base-link metadata for %q: %#v", mode, transitionPath, metadataReads)
				}
				if tc.readBody {
					if coverage.Status != reviewerCoverageCompleteBroad && coverage.Status != reviewerCoverageCompleteConstrained {
						t.Fatalf("%s body-read coverage = %#v, want complete", mode, coverage)
					}
					if !relocationTestContainsString(coverage.InspectedFiles, transitionPath) || !relocationTestContainsString(bodyReads, transitionPath) || len(coverage.MissingFiles) != 0 {
						t.Fatalf("%s ordinary head body was not actually read for complete coverage: coverage=%#v body reads=%#v", mode, coverage, bodyReads)
					}
				} else {
					if coverage.Status != reviewerCoverageIncompleteSkipped || !relocationTestContainsString(coverage.SkippedFiles, transitionPath) || !relocationTestContainsString(coverage.MissingFiles, transitionPath) {
						t.Fatalf("%s metadata-only transition coverage = %#v, want unresolved skipped body", mode, coverage)
					}
					if relocationTestContainsString(coverage.InspectedFiles, transitionPath) || relocationTestContainsString(bodyReads, transitionPath) {
						t.Fatalf("%s historical link metadata was incorrectly treated as inspected head body: coverage=%#v body reads=%#v", mode, coverage, bodyReads)
					}
				}

				foundPrimaryPrompt := false
				for _, request := range adapter.Requests() {
					var prompt struct {
						Schema     string `json:"schema"`
						Task       string `json:"task"`
						Assignment struct {
							SymlinkPaths         []string `json:"symlink_paths"`
							BaseOnlySymlinkPaths []string `json:"base_only_symlink_paths"`
						} `json:"assignment"`
						OutputContract struct {
							Instructions []string `json:"instructions"`
						} `json:"output_contract"`
					}
					if err := json.Unmarshal([]byte(request.Prompt), &prompt); err != nil {
						t.Fatalf("decode transition reviewer prompt: %v", err)
					}
					if prompt.Schema != "findings" || strings.Contains(prompt.Task, "coverage repair") {
						continue
					}
					foundPrimaryPrompt = true
					if !relocationTestContainsString(prompt.Assignment.BaseOnlySymlinkPaths, transitionPath) || relocationTestContainsString(prompt.Assignment.SymlinkPaths, transitionPath) {
						t.Fatalf("%s assignment did not distinguish the base-only symlink transition: %#v", mode, prompt.Assignment)
					}
					instructions := strings.Join(prompt.OutputContract.Instructions, "\n")
					for _, want := range []string{"base_only_symlink_paths", "ordinary cr_read", "head-tree regular body"} {
						if !strings.Contains(instructions, want) {
							t.Fatalf("%s transition prompt instructions omitted %q: %s", mode, want, instructions)
						}
					}
				}
				if !foundPrimaryPrompt {
					t.Fatalf("%s did not send a primary findings prompt", mode)
				}
				if !relocationTestContainsString(bodyReads, residualPath) {
					t.Fatalf("%s residual changed file %q was not actually read: %#v", mode, residualPath, bodyReads)
				}
			})
		}
	}
}

func TestRelocationAssessmentAndCoverageRepairFailClosedEndToEnd(t *testing.T) {
	movePath := "apps/components/shared/file-0000.go"
	residualPath := "src/router.go"
	cases := []struct {
		name              string
		moveCount         int
		assessmentMode    string
		skipPrimary       string
		omitPrimary       string
		repairResolve     bool
		primaryFinding    bool
		primaryToolStatus llm.DiffToolStatus
		wantRepair        bool
		wantStatus        string
		wantReviewedMoves int
		wantMissingPath   string
		wantSkippedPath   string
		wantFinding       bool
		wantOutcome       reviewplan.Outcome
	}{
		{name: "missing assessment repairs once but remains unresolved", moveCount: 1, assessmentMode: "missing", wantRepair: true, wantStatus: reviewerCoverageIncompleteSkipped, wantMissingPath: movePath, wantSkippedPath: movePath, wantOutcome: reviewplan.OutcomeComment},
		{name: "wrong manifest digest repairs once but remains unresolved", moveCount: 1, assessmentMode: "wrong_manifest", wantRepair: true, wantStatus: reviewerCoverageIncompleteSkipped, wantMissingPath: movePath, wantSkippedPath: movePath, wantOutcome: reviewplan.OutcomeComment},
		{name: "wrong assignment digest repairs once but remains unresolved", moveCount: 1, assessmentMode: "wrong_assignment", wantRepair: true, wantStatus: reviewerCoverageIncompleteSkipped, wantMissingPath: movePath, wantSkippedPath: movePath, wantOutcome: reviewplan.OutcomeComment},
		{name: "explicit skip overrides primary impact assessment", moveCount: 2, skipPrimary: movePath, wantRepair: true, wantStatus: reviewerCoverageIncompleteSkipped, wantReviewedMoves: 1, wantMissingPath: movePath, wantSkippedPath: movePath, wantOutcome: reviewplan.OutcomeComment},
		{name: "omitted residual gets one repair and stays withheld", moveCount: 1, omitPrimary: residualPath, wantRepair: true, wantStatus: reviewerCoverageIncompleteSkipped, wantReviewedMoves: 1, wantMissingPath: residualPath, wantSkippedPath: residualPath, wantOutcome: reviewplan.OutcomeComment},
		{name: "valid focused repair closes residual and preserves finding", moveCount: 1, omitPrimary: residualPath, repairResolve: true, primaryFinding: true, wantRepair: true, wantStatus: "complete", wantReviewedMoves: 1, wantFinding: true, wantOutcome: reviewplan.OutcomeComment},
		{name: "primary tool failure blocks even valid impact assessment", moveCount: 1, primaryToolStatus: llm.DiffToolStatusFailed, wantRepair: false, wantStatus: reviewerCoverageIncompleteTool, wantMissingPath: movePath, wantOutcome: reviewplan.OutcomeComment},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req, contextPath := newRelocationFixture(t, tc.moveCount)
			adapter := &relocationWorkspaceAdapter{
				FakeAdapter:       &llm.FakeAdapter{NameValue: "relocation-test"},
				contextPath:       contextPath,
				assessmentMode:    tc.assessmentMode,
				skipPrimary:       tc.skipPrimary,
				omitPrimary:       tc.omitPrimary,
				repairResolve:     tc.repairResolve,
				primaryFinding:    tc.primaryFinding,
				primaryToolStatus: tc.primaryToolStatus,
			}
			result, err := dryRunForTest(ctx, Options{
				Provider: provider, Adapter: adapter, Store: store, Layout: statepaths.NewLayout(t.TempDir(), t.TempDir()), Now: fixedNow,
				NewRunID: func() string { return "run-relocation-gate" }, NewSessionRowID: sequence("relocation-session"),
				NewFindingID: findingSequence("relocation-finding"), NewActionID: actionSequence(), MaxConcurrency: 1,
			}, req)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			if result.Plan.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", result.Plan.Outcome, tc.wantOutcome)
			}
			if len(result.ReviewerCoverage) != 1 {
				t.Fatalf("reviewer coverage = %#v, want one assigned reviewer", result.ReviewerCoverage)
			}
			coverage := result.ReviewerCoverage[0]
			statusMatches := coverage.Status == tc.wantStatus
			if tc.wantStatus == "complete" {
				statusMatches = coverage.Status == reviewerCoverageCompleteBroad || coverage.Status == reviewerCoverageCompleteConstrained
			}
			if !statusMatches || len(coverage.RelocationReviewedFiles) != tc.wantReviewedMoves {
				t.Fatalf("coverage = %#v, want status %q and %d relocation-reviewed moves", coverage, tc.wantStatus, tc.wantReviewedMoves)
			}
			if tc.wantMissingPath != "" && !relocationTestContainsString(coverage.MissingFiles, tc.wantMissingPath) {
				t.Fatalf("missing files = %#v, want %q", coverage.MissingFiles, tc.wantMissingPath)
			}
			if tc.wantSkippedPath != "" && !relocationTestContainsString(coverage.SkippedFiles, tc.wantSkippedPath) {
				t.Fatalf("skipped files = %#v, want %q", coverage.SkippedFiles, tc.wantSkippedPath)
			}
			if tc.name == "explicit skip overrides primary impact assessment" && relocationTestContainsString(coverage.RelocationReviewedFiles, tc.skipPrimary) {
				t.Fatalf("explicitly skipped relocation %q received impact-review credit: %#v", tc.skipPrimary, coverage.RelocationReviewedFiles)
			}
			if tc.wantFinding {
				if len(result.Findings) != 1 || !strings.Contains(result.Findings[0].Body, "Preserve this primary finding") {
					t.Fatalf("findings = %#v, want primary finding preserved after repair", result.Findings)
				}
			}
			requests := adapter.Requests()
			repairRequests := 0
			for _, request := range requests {
				var prompt struct {
					Schema string `json:"schema"`
					Task   string `json:"task"`
				}
				if err := json.Unmarshal([]byte(request.Prompt), &prompt); err != nil {
					t.Fatalf("decode task prompt: %v", err)
				}
				if prompt.Schema == "findings" && strings.Contains(prompt.Task, "coverage repair") {
					repairRequests++
				}
			}
			wantRepairs := 0
			if tc.wantRepair {
				wantRepairs = 1
			}
			if repairRequests != wantRepairs {
				t.Fatalf("focused repair calls = %d, want %d", repairRequests, wantRepairs)
			}
			adapter.mu.Lock()
			contextReads := append([]string(nil), adapter.workspaceReads...)
			bodyReads := append([]string(nil), adapter.bodyReads...)
			adapter.mu.Unlock()
			if !relocationTestContainsString(contextReads, contextPath) {
				t.Fatalf("reviewer never read assessment evidence %q from its workspace: %#v", contextPath, contextReads)
			}
			if tc.repairResolve && !relocationTestContainsString(bodyReads, residualPath) {
				t.Fatalf("repair did not read residual %q from its workspace: %#v", residualPath, bodyReads)
			}
		})
	}
}

func relocationTestContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func newLargeRelocationFixture(t *testing.T) (*readOnlyProvider, Request, string) {
	return newRelocationFixture(t, largeRelocationMoveCount)
}

func newRelocationFixture(t *testing.T, moveCount int) (*readOnlyProvider, Request, string) {
	t.Helper()
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	ref := req.PRRef
	repo := t.TempDir()
	gitCommandMustSucceed(t, repo, "init", "-b", "main")
	gitCommandMustSucceed(t, repo, "config", "user.name", "Relocation Test")
	gitCommandMustSucceed(t, repo, "config", "user.email", "relocation@example.invalid")
	gitCommandMustSucceed(t, repo, "remote", "add", "origin", fmt.Sprintf("git@%s:%s/%s.git", ref.Host, ref.Owner, ref.Repo))
	contextPath := "config/routes.yaml"
	writeFile(t, filepath.Join(repo, contextPath), "routes:\n  moved: legacy/components/shared\n")
	writeFile(t, filepath.Join(repo, "src/router.go"), "package src\n\nconst route = \"/old\"\n")
	for i := 0; i < moveCount; i++ {
		path := filepath.Join(repo, "legacy/components/shared", fmt.Sprintf("file-%04d.go", i))
		writeFile(t, path, "package shared\n")
	}
	gitCommandMustSucceed(t, repo, "add", ".")
	gitCommandMustSucceed(t, repo, "commit", "-m", "relocation base")
	base := gitCommandMustSucceed(t, repo, "rev-parse", "HEAD")
	gitCommandMustSucceed(t, repo, "checkout", "-b", "feature")
	for i := 0; i < moveCount; i++ {
		oldPath := filepath.Join(repo, "legacy/components/shared", fmt.Sprintf("file-%04d.go", i))
		newPath := filepath.Join(repo, "apps/components/shared", fmt.Sprintf("file-%04d.go", i))
		if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			t.Fatalf("move fixture file %d: %v", i, err)
		}
	}
	writeFile(t, filepath.Join(repo, "src/router.go"), "package src\n\nconst route = \"/new\"\n")
	writeFile(t, filepath.Join(repo, "scripts/build.sh"), "#!/bin/sh\nexec go build ./apps/components/shared\n")
	writeFile(t, filepath.Join(repo, "workspace.yaml"), "packages:\n  - apps/components/shared\n")
	gitCommandMustSucceed(t, repo, "add", "-A")
	gitCommandMustSucceed(t, repo, "commit", "-m", "relocate identical files and update residual paths")
	head := gitCommandMustSucceed(t, repo, "rev-parse", "HEAD")
	diff := gitCommandOutput(t, repo, "diff", "--find-renames=100%", "--no-ext-diff", base, head)
	if strings.Count(diff, "rename from ") != moveCount {
		t.Fatalf("git identified %d exact rename pairs, want %d", strings.Count(diff, "rename from "), moveCount)
	}
	provider.fixtureRepoDir = repo
	provider.pr.Base.SHA = base
	provider.pr.Head.SHA = head
	provider.pr.Base.Name = "main"
	provider.pr.Base.Ref = "refs/heads/main"
	provider.pr.Head.Name = "feature"
	provider.pr.Head.Ref = "refs/heads/feature"
	provider.pr.Ref = ref
	provider.diff = gitprovider.UnifiedDiff{Raw: diff}
	agentDir := t.TempDir()
	writeRequiredAgentForGlob(t, agentDir, "relocation", "**/*")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{agentDir}
	return provider, req, contextPath
}

func newPatchlessRenameFixture(t *testing.T, kind string) (*readOnlyProvider, Request, string, string, string) {
	t.Helper()
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	ref := req.PRRef
	repo := t.TempDir()
	gitCommandMustSucceed(t, repo, "init", "-b", "main")
	gitCommandMustSucceed(t, repo, "config", "user.name", "Relocation Test")
	gitCommandMustSucceed(t, repo, "config", "user.email", "relocation@example.invalid")
	gitCommandMustSucceed(t, repo, "remote", "add", "origin", fmt.Sprintf("git@%s:%s/%s.git", ref.Host, ref.Owner, ref.Repo))
	contextPath := "config/routes.yaml"
	residualPath := "src/router.go"
	writeFile(t, filepath.Join(repo, contextPath), "routes:\n  moved: scripts\n")
	writeFile(t, filepath.Join(repo, residualPath), "package src\n\nconst route = \"/old\"\n")
	oldPath, renamePath := "", ""
	switch kind {
	case "mode":
		oldPath, renamePath = "scripts/tool.sh", "scripts/renamed-tool.sh"
		writeFile(t, filepath.Join(repo, oldPath), "#!/bin/sh\necho reviewed\n")
	case "symlink":
		oldPath, renamePath = "links/old", "links/new"
		writeFile(t, filepath.Join(repo, "targets/target.txt"), "target\n")
		if err := os.MkdirAll(filepath.Join(repo, "links"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../targets/target.txt", filepath.Join(repo, oldPath)); err != nil {
			t.Fatal(err)
		}
	case "symlink-to-regular":
		oldPath, renamePath = "links/current", "links/current"
		writeFile(t, filepath.Join(repo, "targets/target.txt"), "target\n")
		if err := os.MkdirAll(filepath.Join(repo, "links"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../targets/target.txt", filepath.Join(repo, oldPath)); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown patchless rename fixture kind %q", kind)
	}
	gitCommandMustSucceed(t, repo, "add", "-A")
	gitCommandMustSucceed(t, repo, "commit", "-m", "patchless rename base")
	base := gitCommandMustSucceed(t, repo, "rev-parse", "HEAD")
	gitCommandMustSucceed(t, repo, "checkout", "-b", "feature")
	if kind == "symlink-to-regular" {
		if err := os.Remove(filepath.Join(repo, oldPath)); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(repo, renamePath), "replacement regular head body\n")
	} else {
		gitCommandMustSucceed(t, repo, "mv", oldPath, renamePath)
	}
	if kind == "mode" {
		if err := os.Chmod(filepath.Join(repo, renamePath), 0o755); err != nil { // #nosec G302 -- Git fixture requires a tracked executable mode.
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(repo, residualPath), "package src\n\nconst route = \"/new\"\n")
	gitCommandMustSucceed(t, repo, "add", "-A")
	gitCommandMustSucceed(t, repo, "commit", "-m", "rename without hunks and change unrelated file")
	head := gitCommandMustSucceed(t, repo, "rev-parse", "HEAD")
	diff := gitCommandOutput(t, repo, "diff", "--find-renames=100%", "--no-ext-diff", strings.TrimSpace(string(base)), strings.TrimSpace(string(head)))
	if kind == "symlink-to-regular" {
		// The provider seam supplies one normalized patch per changed path; local
		// Git encodes this file-type transition as delete/add sections for the same
		// path, which this focused coverage test is not intended to exercise.
		transitionDiff := fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-../targets/target.txt\n+replacement regular head body\n", renamePath, renamePath, renamePath, renamePath)
		residualDiff := gitCommandOutput(t, repo, "diff", "--no-ext-diff", strings.TrimSpace(string(base)), strings.TrimSpace(string(head)), "--", residualPath)
		diff = transitionDiff + residualDiff
	}
	parsed, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parse patchless fixture diff: %v", err)
	}
	var foundRename, foundResidual, foundTransition bool
	for _, patch := range parsed.Patches {
		if patch.OldPath == oldPath && patch.Path == renamePath && len(patch.Hunks) == 0 {
			foundRename = true
		}
		if kind == "symlink-to-regular" && patch.Path == renamePath && len(patch.Hunks) > 0 {
			foundTransition = true
		}
		if patch.Path == residualPath && len(patch.Hunks) > 0 {
			foundResidual = true
		}
	}
	if kind == "symlink-to-regular" && (!foundTransition || !foundResidual) {
		t.Fatalf("fixture diff lacks symlink-to-regular transition or unrelated content change: %s", diff)
	}
	if kind != "symlink-to-regular" && (!foundRename || !foundResidual) {
		t.Fatalf("fixture diff lacks zero-hunk rename or unrelated content change: %s", diff)
	}
	provider.fixtureRepoDir = repo
	provider.pr.Base.SHA = strings.TrimSpace(string(base))
	provider.pr.Head.SHA = strings.TrimSpace(string(head))
	provider.pr.Base.Name = "main"
	provider.pr.Base.Ref = "refs/heads/main"
	provider.pr.Head.Name = "feature"
	provider.pr.Head.Ref = "refs/heads/feature"
	provider.pr.Ref = ref
	provider.diff = gitprovider.UnifiedDiff{Raw: diff}
	agentDir := t.TempDir()
	writeRequiredAgentForGlob(t, agentDir, "relocation", "**/*")
	trustCurrentTempFixtures(t)
	req.Profile.AgentSources = []string{agentDir}
	return provider, req, contextPath, renamePath, residualPath
}

type relocationWorkspaceAdapter struct {
	*llm.FakeAdapter
	mu                sync.Mutex
	contextPath       string
	workspaceReads    []string
	bodyReads         []string
	assessmentMode    string
	skipPrimary       string
	omitPrimary       string
	repairResolve     bool
	primaryFinding    bool
	primaryToolStatus llm.DiffToolStatus
	inspectSymlink    bool
	readBaseOnlyBody  bool
	metadataReads     []string
}

func (a *relocationWorkspaceAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	var prompt struct {
		Schema       string                   `json:"schema"`
		Task         string                   `json:"task"`
		Agents       []selectionAgentPrompt   `json:"agents"`
		FileManifest promptFileManifest       `json:"file_manifest"`
		Assignment   reviewerPromptAssignment `json:"assignment"`
	}
	if err := json.NewDecoder(strings.NewReader(req.Prompt)).Decode(&prompt); err != nil {
		return nil, fmt.Errorf("decode relocation test prompt: %w", err)
	}
	var output map[string]any
	switch prompt.Schema {
	case "selection":
		var agents []map[string]any
		for _, agent := range prompt.Agents {
			var files []string
			for i, row := range prompt.FileManifest.Rows {
				if len(row) > 7 && row[7] == true {
					files = append(files, row[0].(string))
				}
				_ = i
			}
			agents = append(agents, map[string]any{"agent_id": agent.ID, "rationale": "one broad required reviewer", "files": files})
		}
		output = map[string]any{"schema_version": 1, "selected_agents": agents, "thread_actions": []any{}, "reasoning": "selected the required relocation reviewer"}
	case "findings":
		if req.ReviewerWorkspace == nil {
			return nil, fmt.Errorf("relocation reviewer request has no workspace")
		}
		isRepair := strings.Contains(prompt.Task, "coverage repair")
		var inspected []string
		var skipped []string
		for _, index := range prompt.Assignment.ScopeIndices {
			if index < 0 || index >= len(prompt.FileManifest.Rows) {
				return nil, fmt.Errorf("scope index %d out of range", index)
			}
			row := prompt.FileManifest.Rows[index]
			path, ok := row[0].(string)
			if !ok {
				return nil, fmt.Errorf("manifest path at %d is not a string", index)
			}
			isRelocation := len(row) > 8 && row[8] == true
			if relocationTestContainsString(prompt.Assignment.BaseOnlySymlinkPaths, path) {
				workspace := req.ReviewerWorkspace
				config := pireviewtool.Config{
					RepoDir: workspace.RepoDir, DiffPath: workspace.DiffPath, MaxOutputBytes: workspace.MaxToolOutputBytes,
					TimeoutMS: 1000, SymlinkMetadataPath: workspace.SymlinkMetadataPath,
					SymlinkMetadataDigest: workspace.SymlinkMetadataDigest, BaseSHA: workspace.BaseSHA, HeadSHA: workspace.HeadSHA,
				}
				metadataOutput, metadataErr := pireviewtool.Execute(ctx, config, pireviewtool.Request{Tool: pireviewtool.ToolRead, Path: path, View: "symlink"})
				if metadataErr != nil || !strings.Contains(metadataOutput, `"path": "`+path+`"`) {
					return nil, fmt.Errorf("inspect historical base symlink %q from pinned metadata: %s, %w", path, metadataOutput, metadataErr)
				}
				a.mu.Lock()
				a.metadataReads = append(a.metadataReads, path)
				a.mu.Unlock()
				if !a.readBaseOnlyBody {
					skipped = append(skipped, path)
					continue
				}
				bodyOutput, bodyErr := pireviewtool.Execute(ctx, config, pireviewtool.Request{Tool: pireviewtool.ToolRead, Path: path})
				if bodyErr != nil || !strings.Contains(bodyOutput, "replacement regular head body") {
					return nil, fmt.Errorf("inspect pinned head regular body %q with ordinary cr_read: %s, %w", path, bodyOutput, bodyErr)
				}
				inspected = append(inspected, path)
				a.mu.Lock()
				a.bodyReads = append(a.bodyReads, path)
				a.mu.Unlock()
				continue
			}
			if relocationTestContainsString(prompt.Assignment.SymlinkPaths, path) {
				if !a.inspectSymlink {
					skipped = append(skipped, path)
					continue
				}
				workspace := req.ReviewerWorkspace
				metadataOutput, metadataErr := pireviewtool.Execute(ctx, pireviewtool.Config{
					RepoDir: workspace.RepoDir, DiffPath: workspace.DiffPath, MaxOutputBytes: workspace.MaxToolOutputBytes,
					TimeoutMS: 1000, SymlinkMetadataPath: workspace.SymlinkMetadataPath,
					SymlinkMetadataDigest: workspace.SymlinkMetadataDigest, BaseSHA: workspace.BaseSHA, HeadSHA: workspace.HeadSHA,
				}, pireviewtool.Request{Tool: pireviewtool.ToolRead, Path: path, View: "symlink"})
				if metadataErr != nil || !strings.Contains(metadataOutput, `"path": "`+path+`"`) {
					return nil, fmt.Errorf("inspect changed symlink %q from pinned metadata: %s, %w", path, metadataOutput, metadataErr)
				}
				inspected = append(inspected, path)
				a.mu.Lock()
				a.metadataReads = append(a.metadataReads, path)
				a.mu.Unlock()
				continue
			}
			if a.skipPrimary == path {
				skipped = append(skipped, path)
				continue
			}
			if isRelocation {
				// A valid assignment assessment reviews the path impact, not the body.
				// A repair without impact evidence leaves its assigned move unresolved.
				if isRepair {
					skipped = append(skipped, path)
				}
				continue
			}
			if (!isRepair && a.omitPrimary == path) || (isRepair && !a.repairResolve) {
				if isRepair {
					skipped = append(skipped, path)
				}
				continue
			}
			if _, err := os.ReadFile(filepath.Join(req.ReviewerWorkspace.RepoDir, filepath.FromSlash(path))); err != nil {
				return nil, fmt.Errorf("read assigned residual %q from reviewer workspace: %w", path, err)
			}
			inspected = append(inspected, path)
			a.mu.Lock()
			a.bodyReads = append(a.bodyReads, path)
			a.mu.Unlock()
		}
		if _, err := os.ReadFile(filepath.Join(req.ReviewerWorkspace.RepoDir, filepath.FromSlash(a.contextPath))); err != nil {
			return nil, fmt.Errorf("read relocation context %q from reviewer workspace: %w", a.contextPath, err)
		}
		a.mu.Lock()
		a.workspaceReads = append(a.workspaceReads, a.contextPath)
		a.mu.Unlock()
		output = map[string]any{
			"schema_version": 1, "agent_id": prompt.Assignment.AgentID,
			"inspected_files": inspected, "context_files": []string{a.contextPath},
			"skipped_files": skipped, "constraints": []string{}, "findings": []any{},
		}
		if !isRepair && a.primaryFinding {
			output["findings"] = []any{map[string]any{
				"severity": "major", "file_path": "workspace.yaml", "anchor": map[string]any{"kind": "file"},
				"body": "Preserve this primary finding after focused coverage repair.",
			}}
		}
		if !isRepair && a.assessmentMode != "missing" {
			manifestDigest := prompt.Assignment.ManifestDigest
			assignmentDigest := prompt.Assignment.AssignmentDigest
			if a.assessmentMode == "wrong_manifest" {
				manifestDigest = "wrong-manifest-digest"
			}
			if a.assessmentMode == "wrong_assignment" {
				assignmentDigest = "wrong-assignment-digest"
			}
			output["relocation_assessment"] = map[string]any{
				"manifest_digest": manifestDigest, "assignment_digest": assignmentDigest,
				"path_impact_reviewed": true, "evidence_files": []string{a.contextPath},
				"basis": "Read route configuration from the pinned reviewer workspace and checked move-path impact.",
			}
		}
	case "rollup":
		var rollupPrompt struct {
			Findings []struct {
				ID string `json:"id"`
			} `json:"findings"`
		}
		if err := json.NewDecoder(strings.NewReader(req.Prompt)).Decode(&rollupPrompt); err != nil {
			return nil, fmt.Errorf("decode relocation test rollup prompt: %w", err)
		}
		findingIDs := make([]string, 0, len(rollupPrompt.Findings))
		for _, finding := range rollupPrompt.Findings {
			findingIDs = append(findingIDs, finding.ID)
		}
		event := "approve"
		if len(findingIDs) > 0 {
			event = "comment"
		}
		return a.queue(ctx, req, "relocation-rollup", []byte(rollupJSON(event, findingIDs)), nil)
	default:
		return a.queue(ctx, req, "relocation-dossier", []byte(discussionSummaryJSON(nil, nil)), nil)
	}
	data, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	var toolEvidence *llm.ReviewerToolEvidence
	if prompt.Schema == "findings" {
		status := llm.DiffToolStatusSucceeded
		if !strings.Contains(prompt.Task, "coverage repair") && a.primaryToolStatus != "" {
			status = a.primaryToolStatus
		}
		toolEvidence = &llm.ReviewerToolEvidence{DiffStatus: status}
	}
	return a.queue(ctx, req, "relocation-"+prompt.Schema, data, toolEvidence)
}

func (a *relocationWorkspaceAdapter) queue(ctx context.Context, req llm.Request, sessionID string, output []byte, evidence *llm.ReviewerToolEvidence) (llm.Stream, error) {
	a.Queue(llm.FakeResult{SessionID: sessionID, Response: llm.Response{StructuredOutput: output, ReviewerToolEvidence: evidence}})
	return a.FakeAdapter.Start(ctx, req)
}

var _ llm.Adapter = (*relocationWorkspaceAdapter)(nil)
