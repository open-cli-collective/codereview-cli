package pipeline

import (
	"encoding/json"
	"fmt"
	"reflect"
	"unicode/utf8"

	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
)

func TestThreadAnalysisFailuresChangeSynthesisFingerprint(t *testing.T) {
	pr := gitprovider.PR{Title: "fixture"}
	base, err := buildRollupPrompt(pr, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := buildRollupPrompt(pr, nil, nil, nil, nil)
	if err != nil || empty != base || strings.Contains(base, "thread_analysis_failures") {
		t.Fatalf("no-failure prompt changed: base=%s empty=%s err=%v", base, empty, err)
	}
	fingerprint := func(prompt string) string {
		return llmlifecycle.Fingerprint("fake", orchestratorRollupStage, "rollup", "model", "effort", prompt, nil)
	}
	seen := map[string]bool{fingerprint(base): true}
	for _, failure := range []reviewplan.ThreadAnalysisFailureSummary{
		{ThreadID: "thread-1", Error: "structured output remained invalid after retry"},
		{ThreadID: "thread-2", Error: "structured output remained invalid after retry"},
		{ThreadID: "thread-1", Error: "provider execution failed; see local task artifacts for details"},
	} {
		prompt, err := buildRollupPrompt(pr, nil, nil, nil, []reviewplan.ThreadAnalysisFailureSummary{failure})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(prompt, failure.ThreadID) || !strings.Contains(prompt, failure.Error) || !strings.Contains(prompt, "thread_analysis_failures") {
			t.Fatalf("prompt missing failure evidence: %s", prompt)
		}
		fp := fingerprint(prompt)
		if seen[fp] {
			t.Fatalf("failure evidence did not change fingerprint: %#v", failure)
		}
		seen[fp] = true
	}
}

func TestThreadFailureSynthesisPromptBoundsExamplesAndBindsOmittedEvidence(t *testing.T) {
	failures := make([]reviewplan.ThreadAnalysisFailureSummary, 2500)
	for i := range failures {
		failures[i] = reviewplan.ThreadAnalysisFailureSummary{ThreadID: fmt.Sprintf("thread-%04d", i), Error: strings.Repeat("界", 800)}
	}
	original := append([]reviewplan.ThreadAnalysisFailureSummary(nil), failures...)
	prompt, err := buildRollupPrompt(gitprovider.PR{}, nil, nil, nil, failures)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Failures rollupThreadAnalysisFailureSummary `json:"thread_analysis_failures"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Failures.Count != 2500 || payload.Failures.Omitted != 2495 || len(payload.Failures.Examples) != 5 || len(payload.Failures.EvidenceDigest) != 64 {
		t.Fatalf("failure prompt summary = %#v", payload.Failures)
	}
	if len(prompt) > 12000 || strings.Contains(prompt, "thread-0005") || strings.Contains(prompt, "thread-2499") || !utf8.ValidString(prompt) {
		t.Fatalf("failure prompt bytes=%d, want bounded examples", len(prompt))
	}
	if !reflect.DeepEqual(failures, original) {
		t.Fatal("prompt sampling mutated complete failure collection")
	}
	changed := append([]reviewplan.ThreadAnalysisFailureSummary(nil), failures...)
	changed[5].ThreadID = "zz-omitted-thread-changed"
	changedPrompt, err := buildRollupPrompt(gitprovider.PR{}, nil, nil, nil, changed)
	if err != nil {
		t.Fatal(err)
	}
	var changedPayload struct {
		Failures rollupThreadAnalysisFailureSummary `json:"thread_analysis_failures"`
	}
	if err := json.Unmarshal([]byte(changedPrompt), &changedPayload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.Failures.Examples, changedPayload.Failures.Examples) {
		t.Fatal("omitted evidence mutation unexpectedly changed visible examples")
	}
	fingerprint := func(value string) string {
		return llmlifecycle.Fingerprint("fake", orchestratorRollupStage, "rollup", "model", "effort", value, []string{"coverage-artifact=current", "relocation-manifest=current", "context-contract=current"})
	}
	if payload.Failures.EvidenceDigest == changedPayload.Failures.EvidenceDigest || fingerprint(prompt) == fingerprint(changedPrompt) {
		t.Fatal("changed omitted failure did not invalidate complete-evidence fingerprint")
	}
	changed = append([]reviewplan.ThreadAnalysisFailureSummary(nil), failures...)
	changed[5].Error = "provider execution failed; see local task artifacts for details"
	changedPrompt, err = buildRollupPrompt(gitprovider.PR{}, nil, nil, nil, changed)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint(prompt) == fingerprint(changedPrompt) {
		t.Fatal("changed omitted diagnostic did not invalidate fingerprint")
	}
	reordered := append([]reviewplan.ThreadAnalysisFailureSummary(nil), failures...)
	for i, j := 0, len(reordered)-1; i < j; i, j = i+1, j-1 {
		reordered[i], reordered[j] = reordered[j], reordered[i]
	}
	reorderedPrompt, err := buildRollupPrompt(gitprovider.PR{}, nil, nil, nil, reordered)
	if err != nil || reorderedPrompt != prompt {
		t.Fatalf("failure evidence digest is not deterministic across order, err=%v", err)
	}
}

func TestThreadFailureDigestBindsRawEvidenceBeforeDisplayRepair(t *testing.T) {
	first := []reviewplan.ThreadAnalysisFailureSummary{{ThreadID: "thread-" + string([]byte{0xff}), Error: "provider execution failed"}}
	second := []reviewplan.ThreadAnalysisFailureSummary{{ThreadID: "thread-�", Error: "provider execution failed"}}
	firstSummary, secondSummary := rollupThreadAnalysisFailurePrompt(first), rollupThreadAnalysisFailurePrompt(second)
	if !reflect.DeepEqual(firstSummary.Examples, secondSummary.Examples) {
		t.Fatal("display repair should produce identical safe examples")
	}
	if firstSummary.EvidenceDigest == secondSummary.EvidenceDigest {
		t.Fatal("distinct raw evidence shared a complete-evidence digest after display repair")
	}
}
