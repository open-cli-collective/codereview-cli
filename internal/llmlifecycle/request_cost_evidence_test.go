package llmlifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
)

type lifecycleCostAdapter struct{ *llm.FakeAdapter }

func (*lifecycleCostAdapter) RequestCostEvidenceSource() string { return "openai_responses" }
func lifecycleCostPtr[T any](v T) *T                            { return &v }
func lifecycleCostResponse(id, text string) llm.Response {
	return llm.Response{StructuredOutput: []byte(text), ReviewerToolEvidence: &llm.ReviewerToolEvidence{DiffStatus: llm.DiffToolStatusSucceeded}, RequestCostEvidence: &llm.RequestCostEvidence{
		Version: 1, Source: "openai_responses", Scope: "invocation", AttemptsObserved: 1, AttemptCountExact: true,
		Attempts: []llm.RequestCostAttempt{{Ordinal: 1, AdapterAttempt: 1, ValidationPhase: "initial", RuntimeID: "openai-api-key", EndpointKind: "official_global", TransportMode: "single_send_v1", Dispatch: "possibly_dispatched", HTTPStatus: lifecycleCostPtr(200), BodyState: "complete", ProviderResponseID: lifecycleCostPtr(id), ObservedModel: lifecycleCostPtr("observed"), ObservedServiceTier: lifecycleCostPtr("default"), ResponseStatus: lifecycleCostPtr("completed"), RequestedModel: "model-1", Usage: &llm.OpenAIRequestUsage{InputTokens: lifecycleCostPtr(int64(10)), OutputTokens: lifecycleCostPtr(int64(3)), CacheReadTokens: lifecycleCostPtr(int64(2)), CacheWriteTokens: lifecycleCostPtr(int64(0))}}},
	}}
}
func newLifecycleCostAdapter() *lifecycleCostAdapter {
	return &lifecycleCostAdapter{&llm.FakeAdapter{NameValue: "openai_api"}}
}
func requireCostAttempts(t *testing.T, e *llm.RequestCostEvidence, ids ...string) {
	t.Helper()
	if e == nil || len(e.Attempts) != len(ids) {
		t.Fatalf("cost history=%#v, want %v", e, ids)
	}
	for i, id := range ids {
		if e.Attempts[i].ProviderResponseID == nil || *e.Attempts[i].ProviderResponseID != id {
			t.Fatalf("receipt %d=%#v, want %s", i, e.Attempts[i], id)
		}
	}
	if e.AttemptCountExact || !slices.Contains(e.Gaps, llm.CostGapLegacy) {
		t.Fatal("task history acquired an unestablished fresh origin")
	}
}

func TestRequestCostLifecycleSuccessCacheAndValidation(t *testing.T) {
	for _, noRun := range []bool{false, true} {
		t.Run(fmt.Sprint(noRun), func(t *testing.T) {
			a := newLifecycleCostAdapter()
			a.Queue(llm.FakeResult{SessionID: "provider-A", Response: lifecycleCostResponse("A", `invalid`)})
			a.Queue(llm.FakeResult{SessionID: "provider-B", Response: lifecycleCostResponse("B", `{"ok":true}`)})
			store := newLifecycleStore()
			req := lifecycleRequest(t, store, a)
			if noRun {
				req.RunID = ""
				req.AllowNoRunCache = true
				req.Store = nil
			}
			first, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
			if err != nil {
				t.Fatal(err)
			}
			requireCostAttempts(t, first.Draft.Response.RequestCostEvidence, "A", "B")
			cached, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
			if err != nil || !cached.Cached {
				t.Fatalf("cache err=%v", err)
			}
			requireCostAttempts(t, cached.Draft.Response.RequestCostEvidence, "A", "B")
			if !reflect.DeepEqual(first.Draft.Response.RequestCostEvidence, cached.Draft.Response.RequestCostEvidence) || len(a.Requests()) != 2 {
				t.Fatal("reload changed/counted history or called provider")
			}
			if !noRun && len(store.inserted) != 1 {
				t.Fatal("cache inserted another session")
			}
			*cached.Draft.Response.RequestCostEvidence.Attempts[0].Usage.InputTokens = 900
			if *first.Draft.Response.RequestCostEvidence.Attempts[0].Usage.InputTokens != 10 {
				t.Fatal("cache aliases response")
			}
		})
	}
}

func TestRequestCostIsolatedFailureWithAndWithoutLedgerSession(t *testing.T) {
	for _, sessionID := range []string{"", "provider-A"} {
		a := newLifecycleCostAdapter()
		a.Queue(llm.FakeResult{SessionID: sessionID, Response: lifecycleCostResponse("A", `invalid`)})
		a.Queue(llm.FakeResult{SessionID: sessionID, Response: lifecycleCostResponse("B", `invalid`)})
		req := lifecycleRequest(t, newLifecycleStore(), a)
		req.FailureStatus = StatusFailedIsolated
		first, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
		if err == nil {
			t.Fatal("invalid output succeeded")
		}
		requireCostAttempts(t, first.Draft.Response.RequestCostEvidence, "A", "B")
		cached, ok, err := LoadStructured(context.Background(), req, decodeLifecyclePayload)
		if !ok || err == nil || !cached.Cached {
			t.Fatalf("isolated cache ok=%t err=%v", ok, err)
		}
		requireCostAttempts(t, cached.Draft.Response.RequestCostEvidence, "A", "B")
		if len(a.Requests()) != 2 {
			t.Fatal("isolated cache dispatched again")
		}
	}
}

func TestRequestCostResetRetainsMetadataPrefixAfterMissingSidecar(t *testing.T) {
	a := newLifecycleCostAdapter()
	a.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `invalid`)})
	a.Queue(llm.FakeResult{Response: lifecycleCostResponse("B", `{"ok":true}`)})
	req := lifecycleRequest(t, newLifecycleStore(), a)
	first, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
	if err != nil {
		t.Fatal(err)
	}
	requireCostAttempts(t, first.Draft.Response.RequestCostEvidence, "A", "B")
	path, _ := req.Paths.RequestCostCheckpoint(req.TaskID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := ResetIfInputFingerprintChanged(req.Paths, req.TaskID, "changed"); err != nil {
		t.Fatal(err)
	}
	preserved, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
	if gap != "" {
		t.Fatalf("preserved checkpoint gap=%s", gap)
	}
	requireCostAttempts(t, preserved.Evidence, "A", "B")
	if preserved.GenerationsObserved < 1 || preserved.OmittedGenerations < 1 {
		t.Fatal("metadata-only known generation reported as zero")
	}
	req.InputFingerprint = "changed"
	req.Model = "changed-model"
	a.Queue(llm.FakeResult{Response: lifecycleCostResponse("C", `{"ok":true}`)})
	last, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
	if err != nil {
		t.Fatal(err)
	}
	requireCostAttempts(t, last.Draft.Response.RequestCostEvidence, "A", "B", "C")
	if !slices.Contains(last.Draft.Response.RequestCostEvidence.Gaps, llm.CostGapCheckpoint) {
		t.Fatal("missing sidecar gap healed")
	}
	final, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
	if gap != "" || final.GenerationsObserved < 2 || final.OmittedGenerations < 1 {
		t.Fatal("retained A/B/C generation lower bound lost")
	}
}

func TestRequestCostInterruptionAndFinalBeforeLedger(t *testing.T) {
	a := newLifecycleCostAdapter()
	req := lifecycleRequest(t, newLifecycleStore(), a)
	if _, err := beginCostGeneration(req, "interrupted"); err != nil {
		t.Fatal(err)
	}
	a.Queue(llm.FakeResult{Response: lifecycleCostResponse("C", `{"ok":true}`)})
	got, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Draft.Response.RequestCostEvidence.Gaps, llm.CostGapInterrupted) {
		t.Fatal("pending generation erased")
	}
	b := newLifecycleCostAdapter()
	b.Queue(llm.FakeResult{SessionID: "reported", Response: lifecycleCostResponse("D", `{"ok":true}`)})
	store := newLifecycleStore()
	store.insertErr = errors.New("ledger failure")
	req = lifecycleRequest(t, store, b)
	got, err = RunStructured(context.Background(), req, decodeLifecyclePayload)
	if err == nil {
		t.Fatal("ledger failure hidden")
	}
	requireCostAttempts(t, got.Draft.Response.RequestCostEvidence, "D")
	checkpoint, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
	if gap != "" || checkpoint.Pending != nil {
		t.Fatalf("final checkpoint missing before ledger: %s", gap)
	}
	requireCostAttempts(t, checkpoint.Evidence, "D")
}

func TestRequestCostCheckpointFailurePreventsDispatchAndReset(t *testing.T) {
	a := newLifecycleCostAdapter()
	a.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `{"ok":true}`)})
	req := lifecycleRequest(t, newLifecycleStore(), a)
	path, _ := req.Paths.RequestCostCheckpoint(req.TaskID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RunStructured(context.Background(), req, decodeLifecyclePayload); err == nil || len(a.Requests()) != 0 {
		t.Fatal("dispatched without pending checkpoint")
	}
	meta := Metadata{SchemaVersion: SchemaVersion, TaskID: req.TaskID, InputFingerprint: "old"}
	if err := WriteMetadata(req.Paths, meta); err != nil {
		t.Fatal(err)
	}
	if err := ResetIfInputFingerprintChanged(req.Paths, req.TaskID, "new"); err == nil {
		t.Fatal("reset without preservation")
	}
	if _, ok, _ := ReadMetadata(req.Paths, req.TaskID); !ok {
		t.Fatal("reset erased unpreserved metadata")
	}
}

func TestRequestCostBothSchema4ReadersIsolateOptionalCorruption(t *testing.T) {
	for _, raw := range []string{`"bad"`, `{"version":2}`, `{"version":1,"attempts":"bad"}`, `{"version":1,"version":1}`, `{"huge":"` + strings.Repeat("x", llm.MaxRequestCostEvidenceBytes) + `"}`} {
		paths := Paths{LLMTasksDir: t.TempDir()}
		path, _ := paths.Metadata("task")
		data := []byte(`{"schema_version":4,"task_id":"task","status":"succeeded","reviewer_tool_evidence":{"diff_status":"succeeded","trace":{"version":1,"source":"pi_rpc","calls":[]}},"request_cost_evidence":` + raw + `}`)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		meta, ok, err := ReadMetadata(paths, "task")
		if err != nil || !ok {
			t.Fatalf("ReadMetadata rejected optional cost: %v", err)
		}
		all, err := ListMetadata(paths)
		if err != nil || len(all) != 1 {
			t.Fatalf("ListMetadata rejected optional cost: %v", err)
		}
		for _, m := range []Metadata{meta, all[0]} {
			if m.ReviewerToolEvidence == nil || m.ReviewerToolEvidence.Trace == nil || m.ReviewerToolEvidence.DiffStatus != llm.DiffToolStatusSucceeded || m.RequestCostEvidence == nil || m.RequestCostEvidence.AttemptCountExact {
				t.Fatal("cost corruption damaged valid tool metadata or became complete")
			}
		}
	}
	var meta Metadata
	if decodeMetadata([]byte(`{"schema_version":4`), &meta) == nil {
		t.Fatal("malformed whole metadata accepted")
	}
}

type blockingCostAdapter struct {
	*lifecycleCostAdapter
	entered chan struct{}
	release chan struct{}
}

func (a *blockingCostAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	close(a.entered)
	<-a.release
	return a.lifecycleCostAdapter.Start(ctx, req)
}
func TestRequestCostConcurrentRunNoRunAndResetOwnership(t *testing.T) {
	for _, noRun := range []bool{false, true} {
		t.Run(fmt.Sprint(noRun), func(t *testing.T) {
			a := &blockingCostAdapter{newLifecycleCostAdapter(), make(chan struct{}), make(chan struct{})}
			a.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `{"ok":true}`)})
			req := lifecycleRequest(t, newLifecycleStore(), a)
			if noRun {
				req.RunID = ""
				req.AllowNoRunCache = true
				req.Store = nil
			}
			finished := make(chan error, 1)
			go func() { _, err := RunStructured(context.Background(), req, decodeLifecyclePayload); finished <- err }()
			<-a.entered
			if _, err := RunStructured(context.Background(), req, decodeLifecyclePayload); !errors.Is(err, runlock.ErrHeld) {
				t.Errorf("concurrent run=%v", err)
			}
			if _, _, err := LoadStructured(context.Background(), req, decodeLifecyclePayload); !errors.Is(err, runlock.ErrHeld) {
				t.Errorf("concurrent load=%v", err)
			}
			if err := ResetIfInputFingerprintChanged(req.Paths, req.TaskID, "changed"); !errors.Is(err, runlock.ErrHeld) {
				t.Errorf("active reset=%v", err)
			}
			close(a.release)
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			if len(a.Requests()) != 1 {
				t.Fatal("multiple dispatch owners")
			}
		})
	}
}

func TestRequestCostGenerationAndByteBoundsKeepPendingSpace(t *testing.T) {
	paths := Paths{LLMTasksDir: t.TempDir()}
	c, err := newCostCheckpoint(paths, "task", "run")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 65; i++ {
		c.Pending = &pendingCostGeneration{GenerationID: fmt.Sprint(i), InputFingerprint: "fingerprint"}
		if err := writeCostCheckpoint(paths, c); err != nil {
			t.Fatal(err)
		}
		if err := finishCostGeneration(paths, c, lifecycleCostResponse(fmt.Sprint(i), "").RequestCostEvidence); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.Generations) != 64 || c.OmittedGenerations != 1 || len(c.Evidence.Attempts) != 64 || !c.Evidence.Truncated {
		t.Fatalf("bounds=%#v", c)
	}
	c.Pending = &pendingCostGeneration{GenerationID: "still-pending", InputFingerprint: "fingerprint"}
	if err := writeCostCheckpoint(paths, c); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	if len(b) > llm.MaxRequestCostEvidenceBytes || c.Pending == nil {
		t.Fatal("full prefix prevented pending marker")
	}
	got, gap := readCostCheckpoint(paths, "task", "other-run", false)
	if got != nil || gap != llm.CostGapScope {
		t.Fatal("cross-run checkpoint imported")
	}
}

func TestRequestCostCheckpointDigestIncludesScopePendingAndFingerprint(t *testing.T) {
	c, _ := newCostCheckpoint(Paths{LLMTasksDir: t.TempDir()}, "task", "run")
	c.Pending = &pendingCostGeneration{GenerationID: "generation", InputFingerprint: "first"}
	a, _ := checkpointDigest(c)
	c.Pending.InputFingerprint = "second"
	b, _ := checkpointDigest(c)
	c.Pending = nil
	d, _ := checkpointDigest(c)
	c.RunID = "other"
	e, _ := checkpointDigest(c)
	if a == b || b == d || d == e {
		t.Fatal("digest omitted scoped pending or fingerprint state")
	}
}

func TestRequestCostFinalizationFailureKeepsPendingCheckpoint(t *testing.T) {
	req := lifecycleRequest(t, nil, newLifecycleCostAdapter())
	c, err := beginCostGeneration(req, "generation")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := req.Paths.RequestCostCheckpoint(req.TaskID)
	// Blocking the temporary destination models a failure before atomic rename.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	e := lifecycleCostResponse("A", "").RequestCostEvidence
	e.Attempts[0].Ordinal = 1
	if err := finishCostGeneration(req.Paths, c, e); err == nil {
		t.Fatal("finalization failure hidden")
	}
	persisted, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
	if gap != "" || persisted.Pending == nil || persisted.Pending.GenerationID != "generation" {
		t.Fatal("failed finalization overwrote pending marker")
	}
}

func TestRequestCostMetadataAndOutputFailuresKeepFinalCheckpoint(t *testing.T) {
	for _, target := range []string{"metadata.json", "validated-output.json"} {
		a := newLifecycleCostAdapter()
		a.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `{"ok":true}`)})
		req := lifecycleRequest(t, newLifecycleStore(), a)
		dir, _ := req.Paths.TaskDir(req.TaskID)
		// metadata itself must remain absent for pre-dispatch reads. A directory
		// at its .tmp blocks only the later atomic commit.
		if err := os.MkdirAll(filepath.Join(dir, target+".tmp"), 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
		if err == nil {
			t.Fatal("artifact failure hidden")
		}
		requireCostAttempts(t, got.Draft.Response.RequestCostEvidence, "A")
		c, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
		if gap != "" || c.Pending != nil {
			t.Fatal("artifact error erased final cost checkpoint")
		}
		requireCostAttempts(t, c.Evidence, "A")
	}
}

func TestRequestCostReconciliationCorruptMissingConflictingAndLegacy(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "oversized", "wrong-scope"} {
		a := newLifecycleCostAdapter()
		a.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `{"ok":true}`)})
		req := lifecycleRequest(t, newLifecycleStore(), a)
		if _, err := RunStructured(context.Background(), req, decodeLifecyclePayload); err != nil {
			t.Fatal(err)
		}
		path, _ := req.Paths.RequestCostCheckpoint(req.TaskID)
		switch damage {
		case "missing":
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		case "corrupt":
			if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
				t.Fatal(err)
			}
		case "oversized":
			if err := os.WriteFile(path, []byte(strings.Repeat("x", llm.MaxRequestCostEvidenceBytes+1)), 0o600); err != nil {
				t.Fatal(err)
			}
		case "wrong-scope":
			c, _ := newCostCheckpoint(req.Paths, "other", req.RunID)
			c.Digest, _ = checkpointDigest(c)
			b, _ := json.Marshal(c)
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, ok, err := LoadStructured(context.Background(), req, decodeLifecyclePayload)
		if err != nil || !ok || !got.Value.OK {
			t.Fatalf("cost damage broke output reuse: %s %v", damage, err)
		}
		requireCostAttempts(t, got.Draft.Response.RequestCostEvidence, "A")
		if len(got.Draft.Response.RequestCostEvidence.Gaps) < 2 || len(a.Requests()) != 1 {
			t.Fatal("gap repaired by assumption or provider call")
		}
	}
	paths := Paths{LLMTasksDir: t.TempDir()}
	c, _ := newCostCheckpoint(paths, "task", "run")
	e := lifecycleCostResponse("A", "").RequestCostEvidence
	e.Scope = "task_artifact_history"
	e.ArtifactScope = c.ArtifactScope
	e.TaskID = c.TaskID
	e.RunID = c.RunID
	e.Attempts[0].GenerationID = "g"
	e.Attempts[0].Ordinal = 1
	mergeCostObservations(c, e)
	mergeCostObservations(c, e)
	if len(c.Evidence.Attempts) != 1 {
		t.Fatal("duplicate cache reference counted twice")
	}
	conflict := llm.CloneRequestCostEvidence(e)
	*conflict.Attempts[0].Usage.InputTokens = 99
	mergeCostObservations(c, conflict)
	if len(c.Evidence.Attempts) != 1 || !slices.Contains(c.Evidence.Gaps, llm.CostGapConflict) {
		t.Fatal("same identity conflict silently selected")
	}
	e.Attempts[0].GenerationID = "other-generation"
	mergeCostObservations(c, e)
	if !slices.Contains(c.Evidence.Gaps, llm.CostGapDuplicate) {
		t.Fatal("repeated provider response ID silently deduplicated")
	}
	freshLooking, _ := reconcileCostCheckpoint(Paths{LLMTasksDir: filepath.Join(t.TempDir(), "absent")}, "task", "run", nil, false)
	if freshLooking.Evidence.AttemptCountExact || !slices.Contains(freshLooking.Evidence.Gaps, llm.CostGapLegacy) {
		t.Fatal("directory absence became origin proof")
	}
}

func TestRequestCostCheckpointCanonicalGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/request-cost-checkpoint-v1.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var c costCheckpointProjection
	if json.Unmarshal(want, &c) != nil {
		t.Fatal("golden fixture")
	}
	checkpoint := &costCheckpoint{costCheckpointProjection: c}
	got, err := costCheckpointBytes(checkpoint)
	if err != nil || string(got) != string(want) {
		t.Fatalf("canonical bytes changed: %s %v", got, err)
	}
	digest, err := checkpointDigest(checkpoint)
	if err != nil || digest != "3b909dbf6714f12b12b8a5a508f7fd63d4af3d640907d729c37a57a5abb74cc8" {
		t.Fatalf("digest=%s err=%v", digest, err)
	}
}

func TestRequestCostResetForeignRunSidecarPreservesMetadataPrefix(t *testing.T) {
	a := newLifecycleCostAdapter()
	a.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `invalid`)})
	a.Queue(llm.FakeResult{Response: lifecycleCostResponse("B", `{"ok":true}`)})
	req := lifecycleRequest(t, newLifecycleStore(), a)
	if _, err := RunStructured(context.Background(), req, decodeLifecyclePayload); err != nil {
		t.Fatal(err)
	}
	foreign, _ := newCostCheckpoint(req.Paths, req.TaskID, "foreign-run")
	if err := writeCostCheckpoint(req.Paths, foreign); err != nil {
		t.Fatal(err)
	}
	if err := ResetIfInputFingerprintChanged(req.Paths, req.TaskID, "changed"); err != nil {
		t.Fatal(err)
	}
	preserved, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
	if gap != "" {
		t.Fatal(gap)
	}
	requireCostAttempts(t, preserved.Evidence, "A", "B")
	if !slices.Contains(preserved.Evidence.Gaps, llm.CostGapScope) {
		t.Fatal("foreign-run mismatch gap missing")
	}
	req.InputFingerprint = "changed"
	a.Queue(llm.FakeResult{Response: lifecycleCostResponse("C", `{"ok":true}`)})
	got, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
	if err != nil {
		t.Fatal(err)
	}
	requireCostAttempts(t, got.Draft.Response.RequestCostEvidence, "A", "B", "C")
}

func TestRequestCostNearByteLimitMetadataReadersAndReset(t *testing.T) {
	paths := Paths{LLMTasksDir: t.TempDir()}
	c, _ := newCostCheckpoint(paths, "task", "run")
	for i := 0; i < 64; i++ {
		a := lifecycleCostResponse(fmt.Sprint(i), "").RequestCostEvidence.Attempts[0]
		a.GenerationID = fmt.Sprintf("%02d", i) + strings.Repeat("<", 254)
		a.RequestedModel = strings.Repeat("<", 256)
		a.ProviderResponseID = lifecycleCostPtr(fmt.Sprintf("%02d", i) + strings.Repeat("<", 254))
		a.ObservedModel = lifecycleCostPtr(strings.Repeat("<", 256))
		a.RequestedServiceTier = lifecycleCostPtr(strings.Repeat("<", 64))
		a.ObservedServiceTier = lifecycleCostPtr(strings.Repeat("<", 64))
		if !appendCostAttempt(c, a) {
			break
		}
		c.Evidence.AttemptsObserved++
	}
	if len(c.Evidence.Attempts) < 2 {
		t.Fatal("near-cap fixture did not retain observations")
	}
	if err := writeCostCheckpoint(paths, c); err != nil {
		t.Fatal(err)
	}
	meta := Metadata{SchemaVersion: SchemaVersion, TaskID: "task", InputFingerprint: "before", ReviewerToolEvidence: &llm.ReviewerToolEvidence{DiffStatus: llm.DiffToolStatusSucceeded}, RequestCostEvidence: llm.CloneRequestCostEvidence(c.Evidence), RequestCostCheckpoint: checkpointBinding(c)}
	pretty, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var prettyFields map[string]json.RawMessage
	if err := json.Unmarshal(pretty, &prettyFields); err != nil {
		t.Fatal(err)
	}
	if len(prettyFields["request_cost_evidence"]) <= llm.MaxRequestCostEvidenceBytes {
		t.Fatal("near-cap fixture does not expose the historical indented-envelope overflow")
	}
	if err := WriteMetadata(paths, meta); err != nil {
		t.Fatal(err)
	}
	read, ok, err := ReadMetadata(paths, "task")
	if err != nil || !ok {
		t.Fatal(err)
	}
	listed, err := ListMetadata(paths)
	if err != nil || len(listed) != 1 {
		t.Fatal(err)
	}
	for _, m := range []Metadata{read, listed[0]} {
		if m.RequestCostEvidence == nil || len(m.RequestCostEvidence.Attempts) != len(c.Evidence.Attempts) || m.ReviewerToolEvidence == nil {
			t.Fatal("durable formatting inflated cost envelope beyond reader bound")
		}
	}
	path, _ := paths.RequestCostCheckpoint("task")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := ResetIfInputFingerprintChanged(paths, "task", "after"); err != nil {
		t.Fatal(err)
	}
	preserved, gap := readCostCheckpoint(paths, "task", "run", false)
	if gap != "" || len(preserved.Evidence.Attempts) != len(c.Evidence.Attempts) {
		t.Fatal("near-cap metadata prefix lost at reset")
	}
}

func TestRequestCostReadersRejectCaseFoldedDuplicateCostFields(t *testing.T) {
	for _, tc := range []struct {
		fields string
		gap    llm.CostEvidenceGap
	}{
		{`"request_cost_evidence":{"version":1},"REQUEST_COST_EVIDENCE":{"version":2}`, llm.CostGapDuplicate},
		{`"request_cost_evidence":{"version":1,"VERSION":1,"source":"openai_responses","scope":"invocation","attempts_observed":0,"attempt_count_exact":true,"gaps":[],"attempts":[]}`, llm.CostGapInvalid},
		{`"request_cost_checkpoint":null,"REQUEST_COST_CHECKPOINT":null`, llm.CostGapDuplicate},
	} {
		paths := Paths{LLMTasksDir: t.TempDir()}
		path, _ := paths.Metadata("task")
		body := []byte(`{"schema_version":4,"task_id":"task","reviewer_tool_evidence":{"diff_status":"succeeded"},` + tc.fields + `}`)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		meta, ok, err := ReadMetadata(paths, "task")
		if err != nil || !ok {
			t.Fatal(err)
		}
		listed, err := ListMetadata(paths)
		if err != nil || len(listed) != 1 {
			t.Fatal(err)
		}
		for _, m := range []Metadata{meta, listed[0]} {
			if m.ReviewerToolEvidence == nil || m.RequestCostEvidence == nil || !slices.Contains(m.RequestCostEvidence.Gaps, tc.gap) || m.RequestCostEvidence.AttemptCountExact {
				t.Fatalf("case-folded duplicate did not produce expected %s gap", tc.gap)
			}
		}
	}
}

func TestRequestCostSidecarNamesPreserveExistingLongTaskIDs(t *testing.T) {
	for _, length := range []int{234, 238, 242} {
		for _, direct := range []bool{false, true} {
			fake := &llm.FakeAdapter{}
			var adapter llm.Adapter = fake
			if direct {
				adapter = &lifecycleCostAdapter{fake}
			}
			fake.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `{"ok":true}`)})
			req := lifecycleRequest(t, newLifecycleStore(), adapter)
			req.TaskID = strings.Repeat("a", length)
			if _, err := RunStructured(context.Background(), req, decodeLifecyclePayload); err != nil {
				t.Fatalf("length=%d direct=%t error=%v", length, direct, err)
			}
			path, err := req.Paths.RequestCostCheckpoint(req.TaskID)
			if err != nil || len(filepath.Base(path)+".tmp") > 255 {
				t.Fatal("sidecar suffix exceeds filename budget")
			}
			if err := ResetIfInputFingerprintChanged(req.Paths, req.TaskID, "changed"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRequestCostResetInvalidMetadataRunIDKeepsNewerValidSidecar(t *testing.T) {
	for _, badRunID := range []string{"bad\nrun", strings.Repeat("x", 257)} {
		a := newLifecycleCostAdapter()
		a.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `{"ok":true}`)})
		req := lifecycleRequest(t, newLifecycleStore(), a)
		if _, err := RunStructured(context.Background(), req, decodeLifecyclePayload); err != nil {
			t.Fatal(err)
		}
		metadataPath, _ := req.Paths.Metadata(req.TaskID)
		data, err := os.ReadFile(metadataPath)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		var evidence map[string]json.RawMessage
		if err := json.Unmarshal(raw["request_cost_evidence"], &evidence); err != nil {
			t.Fatal(err)
		}
		evidence["run_id"], _ = json.Marshal(badRunID)
		raw["request_cost_evidence"], _ = json.Marshal(evidence)
		corrupt, _ := json.Marshal(raw)
		if err := os.WriteFile(metadataPath, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		checkpoint, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
		if gap != "" {
			t.Fatal(gap)
		}
		checkpoint.Pending = &pendingCostGeneration{GenerationID: "newer-generation", InputFingerprint: req.InputFingerprint}
		if err := finishCostGeneration(req.Paths, checkpoint, lifecycleCostResponse("B", "").RequestCostEvidence); err != nil {
			t.Fatal(err)
		}
		if err := ResetIfInputFingerprintChanged(req.Paths, req.TaskID, "changed"); err != nil {
			t.Fatal(err)
		}
		preserved, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
		if gap != "" {
			t.Fatal(gap)
		}
		requireCostAttempts(t, preserved.Evidence, "A", "B")
		if preserved.RunID != req.RunID || !slices.Contains(preserved.Evidence.Gaps, llm.CostGapScope) {
			t.Fatal("invalid metadata RunID became authoritative no-run scope")
		}
	}
}

func TestRequestCostResetUntrustedBindingCannotDisplaceNewerSidecar(t *testing.T) {
	for _, rawScope := range []string{"invalid", "overlong", "null", "missing"} {
		for _, actualRunID := range []string{"run-1", ""} {
			for _, bindingRun := range []string{"foreign-run", ""} {
				runLabel := actualRunID
				if runLabel == "" {
					runLabel = "no-run"
				}
				t.Run(rawScope+"/"+runLabel+"/"+bindingRun, func(t *testing.T) {
					adapter := newLifecycleCostAdapter()
					adapter.Queue(llm.FakeResult{Response: lifecycleCostResponse("A", `{"ok":true}`)})
					req := lifecycleRequest(t, newLifecycleStore(), adapter)
					req.RunID = actualRunID
					if actualRunID == "" {
						req.AllowNoRunCache = true
						req.Store = nil
					}
					if _, err := RunStructured(context.Background(), req, decodeLifecyclePayload); err != nil {
						t.Fatal(err)
					}
					metadataPath, _ := req.Paths.Metadata(req.TaskID)
					data, err := os.ReadFile(metadataPath)
					if err != nil {
						t.Fatal(err)
					}
					var raw map[string]json.RawMessage
					if err := json.Unmarshal(data, &raw); err != nil {
						t.Fatal(err)
					}
					var evidence map[string]json.RawMessage
					if err := json.Unmarshal(raw["request_cost_evidence"], &evidence); err != nil {
						t.Fatal(err)
					}
					switch rawScope {
					case "invalid":
						evidence["run_id"], _ = json.Marshal("bad\nrun")
					case "overlong":
						evidence["run_id"], _ = json.Marshal(strings.Repeat("x", 257))
					case "null":
						evidence["run_id"] = json.RawMessage(`null`)
					case "missing":
						delete(evidence, "run_id")
					}
					var binding map[string]json.RawMessage
					if err := json.Unmarshal(raw["request_cost_checkpoint"], &binding); err != nil {
						t.Fatal(err)
					}
					binding["run_id"], _ = json.Marshal(bindingRun)
					raw["request_cost_evidence"], _ = json.Marshal(evidence)
					raw["request_cost_checkpoint"], _ = json.Marshal(binding)
					corrupt, _ := json.Marshal(raw)
					if err := os.WriteFile(metadataPath, corrupt, 0o600); err != nil {
						t.Fatal(err)
					}
					checkpoint, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
					if gap != "" {
						t.Fatal(gap)
					}
					checkpoint.Pending = &pendingCostGeneration{GenerationID: "newer-generation", InputFingerprint: req.InputFingerprint}
					if err := finishCostGeneration(req.Paths, checkpoint, lifecycleCostResponse("B", "").RequestCostEvidence); err != nil {
						t.Fatal(err)
					}
					if err := ResetIfInputFingerprintChanged(req.Paths, req.TaskID, "changed"); err != nil {
						t.Fatal(err)
					}
					preserved, gap := readCostCheckpoint(req.Paths, req.TaskID, req.RunID, false)
					if gap != "" {
						t.Fatal(gap)
					}
					requireCostAttempts(t, preserved.Evidence, "A", "B")
					if preserved.RunID != req.RunID || !slices.Contains(preserved.Evidence.Gaps, llm.CostGapScope) || !slices.Contains(preserved.Evidence.Gaps, llm.CostGapConflict) {
						t.Fatal("untrusted binding displaced the newer sidecar or hid ambiguity")
					}
					req.InputFingerprint = "changed"
					adapter.Queue(llm.FakeResult{Response: lifecycleCostResponse("C", `{"ok":true}`)})
					result, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
					if err != nil {
						t.Fatal(err)
					}
					requireCostAttempts(t, result.Draft.Response.RequestCostEvidence, "A", "B", "C")
				})
			}
		}
	}
}

func TestRequestCostSidecarOwnRawRunScopeMustBeValid(t *testing.T) {
	for _, mutation := range []string{"invalid", "overlong", "missing", "null", "actual-empty"} {
		paths := Paths{LLMTasksDir: t.TempDir()}
		checkpoint, err := newCostCheckpoint(paths, "task", "")
		if err != nil {
			t.Fatal(err)
		}
		if mutation == "invalid" {
			checkpoint.RunID = "bad\nrun"
			checkpoint.Evidence.RunID = checkpoint.RunID
		}
		if mutation == "overlong" {
			checkpoint.RunID = strings.Repeat("x", 257)
			checkpoint.Evidence.RunID = checkpoint.RunID
		}
		checkpoint.Digest, err = checkpointDigest(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &raw); err != nil {
			t.Fatal(err)
		}
		if mutation == "missing" {
			delete(raw, "run_id")
		}
		if mutation == "null" {
			raw["run_id"] = json.RawMessage(`null`)
		}
		encoded, err = json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		path, _ := paths.RequestCostCheckpoint("task")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		got, gap := readCostCheckpoint(paths, "task", "", true)
		if mutation == "actual-empty" {
			if got == nil || gap != "" {
				t.Fatal("genuine empty no-run scope rejected")
			}
		} else if got != nil || gap != llm.CostGapScope {
			t.Fatal("invalid sidecar run scope became authoritative")
		}
	}
}
