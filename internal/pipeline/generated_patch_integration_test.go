package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/pireviewtool"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const generatedPatchPath = "patches/dependency.patch"

func TestGeneratedPatchInspectionReachesNormalApprovalGate(t *testing.T) {
	for _, tc := range []struct {
		name, view        string
		unaccountedChange bool
		existing          bool
		want              reviewplan.Outcome
	}{
		{"ordinary read is incomplete", "", false, false, reviewplan.OutcomeComment},
		{"complete generated comparison is inspected", "patch", false, false, reviewplan.OutcomeApproved},
		{"comparison cannot account for broad replacement", "patch", true, false, reviewplan.OutcomeComment},
		{"existing patch changes same late byte on both nested sides", "patch", false, true, reviewplan.OutcomeComment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openPipelineStore(t)
			defer closeStore(t, store)
			provider, req := newGeneratedPatchReviewFixture(t, tc.unaccountedChange, tc.existing)
			adapter := &generatedPatchReviewAdapter{
				FakeAdapter: &llm.FakeAdapter{NameValue: "generated-patch-fixture"},
				view:        tc.view,
			}
			result, err := dryRunForTest(t.Context(), Options{
				Provider: provider, Adapter: adapter, Store: store,
				Layout: statepaths.NewLayout(t.TempDir(), t.TempDir()), Now: fixedNow,
				NewRunID:        func() string { return "generated-patch-review" },
				NewSessionRowID: sequence("generated-patch-session"),
				NewFindingID:    findingSequence("generated-patch-finding"),
				NewActionID:     actionSequence(), MaxConcurrency: 1,
			}, req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Plan.Outcome != tc.want || adapter.reads == 0 {
				t.Fatalf("outcome %q with %d real tool reads; want %q", result.Plan.Outcome, adapter.reads, tc.want)
			}
		})
	}
}

func newGeneratedPatchReviewFixture(t *testing.T, unaccountedChange, existing bool) (*readOnlyProvider, Request) {
	t.Helper()
	provider, req := dryRunHarness(t)
	removeRepoAgentFixture(provider)
	repo := t.TempDir()
	gitCommandMustSucceed(t, repo, "init", "-b", "main")
	gitCommandMustSucceed(t, repo, "config", "user.name", "Patch Fixture")
	gitCommandMustSucceed(t, repo, "config", "user.email", "patch@example.invalid")
	gitCommandMustSucceed(t, repo, "remote", "add", "origin", "git@github.com:open-cli-collective/codereview-cli.git")
	writeFile(t, filepath.Join(repo, "README.md"), "Synthetic fixture\n")
	rng := rand.New(rand.NewPCG(5, 9)) // #nosec G404 -- reproducible synthetic bundle bytes.
	old := make([]byte, 250_000)
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	for i := range old {
		old[i] = alphabet[rng.IntN(len(alphabet))]
	}
	patch := func(n string) string {
		patched := n[:1000] + "FIRST" + n[1000:10_000] + "SECOND" + n[10_000:200_000] + "THIRD" + n[200_000:]
		if unaccountedChange {
			patched = strings.Repeat("?", len(n))
		}
		return "--- a/source.js\n+++ b/source.js\n@@ -1 +1 @@\n-return false;\n+return true;\n" +
			"--- a/bundle.js\n+++ b/bundle.js\n@@ -1 +1 @@\n-" + n + "\n+" + patched + "\n"
	}
	if existing {
		writeFile(t, filepath.Join(repo, generatedPatchPath), patch(string(old)))
		old[240_000] = '!'
	}
	gitCommandMustSucceed(t, repo, "add", ".")
	gitCommandMustSucceed(t, repo, "commit", "-m", "fixture base")
	base := gitCommandMustSucceed(t, repo, "rev-parse", "HEAD")
	gitCommandMustSucceed(t, repo, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(repo, generatedPatchPath), patch(string(old)))
	gitCommandMustSucceed(t, repo, "add", generatedPatchPath)
	gitCommandMustSucceed(t, repo, "commit", "-m", "add dependency patch")
	head := gitCommandMustSucceed(t, repo, "rev-parse", "HEAD")
	provider.fixtureRepoDir = repo
	provider.pr.Base.SHA, provider.pr.Head.SHA = base, head
	provider.pr.Base.Name, provider.pr.Base.Ref = "main", "refs/heads/main"
	provider.pr.Head.Name, provider.pr.Head.Ref = "feature", "refs/heads/feature"
	provider.diff = gitprovider.UnifiedDiff{Raw: gitCommandOutput(t, repo, "diff", "--no-ext-diff", base, head)}
	return provider, req
}

type generatedPatchReviewAdapter struct {
	*llm.FakeAdapter
	view  string
	reads int
}

func (a *generatedPatchReviewAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	var prompt struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal([]byte(req.Prompt), &prompt); err != nil {
		return nil, err
	}
	var result llm.FakeResult
	switch prompt.Schema {
	case "selection":
		result = fakeLLMResult("selection", selectionJSON("harness:reviewer", generatedPatchPath), 1, 1)
	case "findings":
		w := req.ReviewerWorkspace
		if w == nil {
			return nil, fmt.Errorf("generated patch review has no workspace")
		}
		config := pireviewtool.Config{
			RepoDir: w.RepoDir, DiffPath: w.DiffPath, BaseSHA: w.BaseSHA, HeadSHA: w.HeadSHA,
			MaxOutputBytes: w.MaxToolOutputBytes, AddedPatchBlobs: w.AddedPatchBlobs,
		}
		if _, err := pireviewtool.Execute(ctx, config, pireviewtool.Request{Tool: pireviewtool.ToolDiff}); err != nil {
			return nil, err
		}
		report, err := pireviewtool.Execute(ctx, config, pireviewtool.Request{
			Tool: pireviewtool.ToolRead, View: a.view, Path: generatedPatchPath,
		})
		a.reads++
		var inspected, skipped []string
		if err == nil && strings.Contains(report, "+return true;") &&
			strings.Contains(report, "All bytes outside the listed ranges matched exactly.") &&
			strings.Count(report, " -> new[") == 3 &&
			strings.Contains(report, "THIRD") {
			inspected = []string{generatedPatchPath}
		} else {
			skipped = []string{generatedPatchPath}
		}
		result = fakeLLMResult("reviewer", coverageOnlyJSON("harness:reviewer", inspected, skipped), 1, 1)
		result.Response.ReviewerToolEvidence = &llm.ReviewerToolEvidence{DiffStatus: llm.DiffToolStatusSucceeded}
	case "rollup":
		result = fakeLLMResult("rollup", rollupJSON("approve", nil), 1, 1)
	default:
		result = fakeLLMResult("dossier", discussionSummaryJSON(nil, nil), 1, 1)
	}
	a.Queue(result)
	return a.FakeAdapter.Start(ctx, req)
}
