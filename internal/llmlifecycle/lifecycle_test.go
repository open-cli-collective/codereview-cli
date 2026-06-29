package llmlifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

var lifecycleTestNow = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

type lifecyclePayload struct {
	OK bool `json:"ok"`
}

func TestRunStructuredPersistsAndLoadsSucceededTask(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	progress := &lifecycleProgressRecorder{}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response: llm.Response{
			StructuredOutput: []byte(`Here is JSON: {"ok":true}`),
			DurationMS:       123,
			Usage:            llm.Usage{TokensIn: intPtr(10), TokensOut: intPtr(5)},
		},
	})
	req := lifecycleRequest(t, store, adapter)
	req.Progress = progress

	first, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err != nil {
		t.Fatalf("RunStructured first: %v", err)
	}
	if !first.Value.OK {
		t.Fatalf("first value = %#v, want ok", first.Value)
	}
	if first.Cached {
		t.Fatal("first result cached = true, want false")
	}
	if len(adapter.Requests()) != 1 {
		t.Fatalf("adapter requests = %d, want 1", len(adapter.Requests()))
	}
	if len(progress.ends) != 1 || progress.ends[0].result.Usage.TokensIn == nil || *progress.ends[0].result.Usage.TokensIn != 10 ||
		progress.ends[0].result.Usage.TokensOut == nil || *progress.ends[0].result.Usage.TokensOut != 5 {
		t.Fatalf("fresh progress ends = %#v, want usage from provider response", progress.ends)
	}
	storedSession := store.session(t, "session-row-1")
	if storedSession.RunID != "run-1" || storedSession.ProviderSessionID != "provider-session-1" || storedSession.Role != ledger.SessionRoleOrchestrator {
		t.Fatalf("stored session = %#v, want run/provider/default orchestrator role", storedSession)
	}

	meta, ok, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil || !ok {
		t.Fatalf("ReadMetadata = %#v ok=%t err=%v, want metadata", meta, ok, err)
	}
	if meta.Status != StatusSucceeded || meta.SessionRowID != "session-row-1" || meta.ProviderSessionID != "provider-session-1" {
		t.Fatalf("metadata = %#v, want succeeded session metadata", meta)
	}
	assertFileContains(t, meta.ValidatedOutputPath, `"ok":true`)
	assertFileOmits(t, meta.ValidatedOutputPath, "Here is JSON")

	cachedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	progress.loads = nil
	req.Adapter = cachedAdapter
	cached, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err != nil {
		t.Fatalf("RunStructured cached: %v", err)
	}
	if !cached.Cached || !cached.Value.OK {
		t.Fatalf("cached result = %#v, want cached ok", cached)
	}
	if len(cachedAdapter.Requests()) != 0 {
		t.Fatalf("cached adapter requests = %d, want 0", len(cachedAdapter.Requests()))
	}
	if len(progress.loads) != 1 || progress.loads[0].result.Usage.TokensIn == nil || *progress.loads[0].result.Usage.TokensIn != 10 ||
		progress.loads[0].result.Usage.TokensOut == nil || *progress.loads[0].result.Usage.TokensOut != 5 {
		t.Fatalf("cached progress loads = %#v, want usage loaded from ledger session", progress.loads)
	}
}

func TestRunStructuredReloadsSucceededTaskWithRealLedgerAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	store, err := ledger.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	runID := "run-real-ledger-cache"
	if _, err := store.AllocateRun(ctx, ledger.AllocateRunParams{
		PRKey:           "github_open-cli-collective_codereview-cli_29",
		PRURL:           "https://github.com/open-cli-collective/codereview-cli/pull/29",
		RunID:           runID,
		SHA:             strings.Repeat("a", 40),
		BaseSHA:         strings.Repeat("b", 40),
		Profile:         "home",
		PostingIdentity: "review-bot",
		PostMode:        ledger.PostModeDryRun,
		StartedAt:       lifecycleTestNow,
		ArtifactPath:    filepath.Join(t.TempDir(), "run"),
	}); err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})
	req := lifecycleRequest(t, store, adapter)
	req.RunID = runID
	req.Paths = Paths{LLMTasksDir: filepath.Join(t.TempDir(), "llm-tasks")}
	if _, err := RunStructured(ctx, req, decodeLifecyclePayload); err != nil {
		t.Fatalf("RunStructured first: %v", err)
	}
	meta, ok, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil || !ok {
		t.Fatalf("ReadMetadata first = %#v ok=%t err=%v, want metadata", meta, ok, err)
	}
	assertFileContains(t, meta.ValidatedOutputPath, `"ok":true`)
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close first: %v", err)
	}

	reopened, err := ledger.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("ledger.Open reopened: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Fatalf("store.Close reopened: %v", err)
		}
	}()
	cachedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	req.Store = reopened
	req.Adapter = cachedAdapter
	cached, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err != nil {
		t.Fatalf("RunStructured cached after reopen: %v", err)
	}
	if !cached.Cached || !cached.Value.OK {
		t.Fatalf("cached result = %#v, want cached ok", cached)
	}
	if len(cachedAdapter.Requests()) != 0 || len(cachedAdapter.Resumes()) != 0 {
		t.Fatalf("cached adapter starts=%#v resumes=%#v, want no provider call", cachedAdapter.Requests(), cachedAdapter.Resumes())
	}
	if cached.Session.RunID != runID || cached.Session.ProviderSessionID != "provider-session-1" {
		t.Fatalf("cached session = %#v, want reloaded ledger session", cached.Session)
	}
}

func TestRunStructuredFailsClosedWhenCachedSessionIsMissing(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})
	req := lifecycleRequest(t, store, adapter)
	if _, err := RunStructured(ctx, req, decodeLifecyclePayload); err != nil {
		t.Fatalf("RunStructured seed: %v", err)
	}

	missingSessionStore := newLifecycleStore()
	cachedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	req.Store = missingSessionStore
	req.Adapter = cachedAdapter
	_, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err == nil || !strings.Contains(err.Error(), "load task") {
		t.Fatalf("RunStructured missing session error = %v, want fail-closed load error", err)
	}
	if len(cachedAdapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %d, want no provider call", len(cachedAdapter.Requests()))
	}
}

func TestRunStructuredRejectsCachedSessionForDifferentRun(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})
	req := lifecycleRequest(t, store, adapter)
	if _, err := RunStructured(ctx, req, decodeLifecyclePayload); err != nil {
		t.Fatalf("RunStructured seed: %v", err)
	}

	cachedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	req.Adapter = cachedAdapter
	req.RunID = "other-run"
	_, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err == nil || !strings.Contains(err.Error(), "belongs to run") {
		t.Fatalf("RunStructured mismatched run error = %v, want session run mismatch", err)
	}
	if len(cachedAdapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %d, want no provider call for mismatched cache", len(cachedAdapter.Requests()))
	}
}

func TestRunStructuredIgnoresPayloadWithoutMetadata(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	req := lifecycleRequest(t, store, adapter)
	outputPath, err := req.Paths.ValidatedOutput(req.TaskID)
	if err != nil {
		t.Fatalf("ValidatedOutput: %v", err)
	}
	if err := WriteFileAtomic(outputPath, []byte(`{"ok":false}`)); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})

	got, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err != nil {
		t.Fatalf("RunStructured: %v", err)
	}
	if !got.Value.OK {
		t.Fatalf("value = %#v, want provider result instead of uncommitted payload", got.Value)
	}
	if len(adapter.Requests()) != 1 {
		t.Fatalf("adapter requests = %d, want provider call when metadata is missing", len(adapter.Requests()))
	}
}

func TestRunStructuredFailsClosedWhenSuccessMetadataPayloadIsMissing(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	req := lifecycleRequest(t, store, adapter)
	meta := BaseMetadata(req, SessionDraft{
		RowID:             "session-row-1",
		ProviderSessionID: "provider-session-1",
		Adapter:           "fake-llm",
		Model:             req.Model,
		Effort:            req.Effort,
	})
	meta.Status = StatusSucceeded
	meta.ValidatedOutputPath, _ = req.Paths.ValidatedOutput(req.TaskID)
	if err := WriteMetadata(req.Paths, meta); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	_, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err == nil || !strings.Contains(err.Error(), "read task") {
		t.Fatalf("RunStructured error = %v, want missing payload error", err)
	}
	if len(adapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %d, want fail-closed before provider call", len(adapter.Requests()))
	}
}

func TestRunStructuredSessionPersistenceFailureLeavesNoMetadata(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	store.insertErr = errors.New("database down")
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})
	req := lifecycleRequest(t, store, adapter)

	_, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("RunStructured error = %v, want persistence failure", err)
	}
	if _, ok, readErr := ReadMetadata(req.Paths, req.TaskID); readErr != nil || ok {
		t.Fatalf("ReadMetadata ok=%t err=%v, want no commit marker after persistence failure", ok, readErr)
	}
}

func TestRunStructuredRejectsStaleMetadataBeforeProviderCall(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})
	req := lifecycleRequest(t, store, adapter)
	if _, err := RunStructured(ctx, req, decodeLifecyclePayload); err != nil {
		t.Fatalf("RunStructured seed: %v", err)
	}

	staleAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	req.Adapter = staleAdapter
	req.InputFingerprint = ""
	req.Prompt = "changed prompt"
	_, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
		t.Fatalf("RunStructured stale metadata error = %v, want fingerprint error", err)
	}
	if len(staleAdapter.Requests()) != 0 {
		t.Fatalf("adapter requests = %d, want no provider call", len(staleAdapter.Requests()))
	}
}

func TestRunStructuredRerunsFailedBlockingTaskWithStoredProviderSession(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-2",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})
	req := lifecycleRequest(t, store, adapter)
	failedMeta := BaseMetadata(req, SessionDraft{
		RowID:             "failed-row",
		ProviderSessionID: "provider-session-1",
		Adapter:           "fake-llm",
		Model:             req.Model,
		Effort:            req.Effort,
	})
	failedMeta.Status = StatusFailedBlocking
	failedMeta.Error = "previous failure"
	if err := WriteMetadata(req.Paths, failedMeta); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	got, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err != nil {
		t.Fatalf("RunStructured retry: %v", err)
	}
	if !got.Value.OK {
		t.Fatalf("retry value = %#v, want ok", got.Value)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "provider-session-1" {
		t.Fatalf("resumes = %#v, want resume from failed metadata session", resumes)
	}
	session := store.session(t, "session-row-1")
	if session.RunID != req.RunID || session.ProviderSessionID != "provider-session-2" {
		t.Fatalf("persisted session = %#v, want resumed success session for run", session)
	}
	meta, ok, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil || !ok {
		t.Fatalf("ReadMetadata = %#v ok=%t err=%v, want metadata", meta, ok, err)
	}
	if meta.Status != StatusSucceeded || meta.SessionRowID != "session-row-1" || meta.ProviderSessionID != "provider-session-2" {
		t.Fatalf("metadata after resume = %#v, want succeeded resumed session", meta)
	}
	assertFileContains(t, meta.ValidatedOutputPath, `"ok":true`)
}

func TestRunStructuredRerunsFailedBlockingTaskWithLatestAttemptSession(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-3",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})
	req := lifecycleRequest(t, store, adapter)
	failedMeta := BaseMetadata(req, SessionDraft{
		RowID:   "failed-row",
		Adapter: "fake-llm",
		Model:   req.Model,
		Effort:  req.Effort,
	})
	failedMeta.Status = StatusFailedBlocking
	failedMeta.Error = "previous failure"
	failedMeta.ProviderSessionID = ""
	failedMeta.Attempts = []AttemptMetadata{
		{Attempt: "initial", ProviderSessionID: "provider-session-1", DecodeError: "invalid"},
		{Attempt: "retry", ProviderSessionID: "provider-session-2", DecodeError: "still invalid"},
	}
	if err := WriteMetadata(req.Paths, failedMeta); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	got, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err != nil {
		t.Fatalf("RunStructured retry: %v", err)
	}
	if !got.Value.OK {
		t.Fatalf("retry value = %#v, want ok", got.Value)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 1 || resumes[0].SessionID != "provider-session-2" {
		t.Fatalf("resumes = %#v, want resume from latest failed attempt session", resumes)
	}
	session := store.session(t, "session-row-1")
	if session.RunID != req.RunID || session.ProviderSessionID != "provider-session-3" {
		t.Fatalf("persisted session = %#v, want resumed success session for run", session)
	}
	meta, ok, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil || !ok {
		t.Fatalf("ReadMetadata = %#v ok=%t err=%v, want metadata", meta, ok, err)
	}
	if meta.Status != StatusSucceeded || meta.SessionRowID != "session-row-1" || meta.ProviderSessionID != "provider-session-3" {
		t.Fatalf("metadata after attempt resume = %#v, want succeeded resumed session", meta)
	}
	assertFileContains(t, meta.ValidatedOutputPath, `"ok":true`)
}

func TestRunStructuredLoadsIsolatedFailureWithoutRerun(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":"not-bool"}`)},
	})
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":"still-not-bool"}`)},
	})
	req := lifecycleRequest(t, store, adapter)
	req.FailureStatus = StatusFailedIsolated
	_, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if !IsTaskStatus(err, StatusFailedIsolated) {
		t.Fatalf("RunStructured invalid output error = %v, want isolated task error", err)
	}

	cachedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	req.Adapter = cachedAdapter
	_, err = RunStructured(ctx, req, decodeLifecyclePayload)
	if !IsTaskStatus(err, StatusFailedIsolated) {
		t.Fatalf("RunStructured cached isolated error = %v, want cached isolated task error", err)
	}
	if len(cachedAdapter.Requests()) != 0 {
		t.Fatalf("cached adapter requests = %d, want 0", len(cachedAdapter.Requests()))
	}
}

func TestRunStructuredCallerOwnedCacheDoesNotRequireRunOrSessionStore(t *testing.T) {
	ctx := context.Background()
	adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	adapter.Queue(llm.FakeResult{
		SessionID: "provider-session-1",
		Response:  llm.Response{StructuredOutput: []byte(`{"ok":true}`)},
	})
	req := lifecycleRequest(t, nil, adapter)
	req.RunID = ""
	req.Store = nil
	req.AllowNoRunCache = true

	first, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err != nil {
		t.Fatalf("RunStructured no-run first: %v", err)
	}
	if !first.Value.OK {
		t.Fatalf("first value = %#v, want ok", first.Value)
	}
	meta, ok, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil || !ok {
		t.Fatalf("ReadMetadata no-run = %#v ok=%t err=%v, want metadata", meta, ok, err)
	}
	if meta.SessionRowID != "" {
		t.Fatalf("metadata session_row_id = %q, want empty for caller-owned cache", meta.SessionRowID)
	}

	cachedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
	req.Adapter = cachedAdapter
	cached, err := RunStructured(ctx, req, decodeLifecyclePayload)
	if err != nil {
		t.Fatalf("RunStructured no-run cached: %v", err)
	}
	if !cached.Cached || !cached.Value.OK {
		t.Fatalf("cached no-run result = %#v, want cached ok", cached)
	}
	if len(cachedAdapter.Requests()) != 0 {
		t.Fatalf("cached adapter requests = %d, want 0", len(cachedAdapter.Requests()))
	}
}

func TestValidatePayloadPathRejectsEscape(t *testing.T) {
	paths := Paths{LLMTasksDir: t.TempDir()}
	outside := filepath.Join(t.TempDir(), "payload.json")
	if _, err := ValidatePayloadPath(paths, "task-1", "validated_output_path", outside); err == nil || !strings.Contains(err.Error(), "outside the task artifact directory") {
		t.Fatalf("ValidatePayloadPath error = %v, want escape rejection", err)
	}
}

func lifecycleRequest(t *testing.T, store Store, adapter llm.Adapter) Request {
	t.Helper()
	return Request{
		Store:            store,
		Adapter:          adapter,
		RunID:            "run-1",
		TaskID:           "task-1",
		Phase:            "unit",
		InputFingerprint: "input-fingerprint",
		Paths:            Paths{LLMTasksDir: filepath.Join(t.TempDir(), "llm-tasks")},
		Model:            "model-1",
		Effort:           "low",
		LogPath:          filepath.Join(t.TempDir(), "agent.log"),
		Prompt:           "return json",
		Now:              func() time.Time { return lifecycleTestNow },
		NewSessionRowID:  sequence("session-row"),
	}
}

func decodeLifecyclePayload(data []byte) (lifecyclePayload, error) {
	var out lifecyclePayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return lifecyclePayload{}, err
	}
	return out, nil
}

type lifecycleStore struct {
	insertErr error
	getErr    error
	sessions  map[string]ledger.Session
	inserted  []ledger.Session
}

type lifecycleProgressRecorder struct {
	ends  []lifecycleProgressEnd
	loads []lifecycleProgressLoad
}

type lifecycleProgressSpan struct {
	parent *lifecycleProgressRecorder
	event  ProgressEvent
}

type lifecycleProgressEnd struct {
	event  ProgressEvent
	err    error
	result ProgressResult
}

type lifecycleProgressLoad struct {
	event  ProgressEvent
	result ProgressResult
}

func (r *lifecycleProgressRecorder) StartLLMTask(event ProgressEvent) ProgressSpan {
	return lifecycleProgressSpan{parent: r, event: event}
}

func (r *lifecycleProgressRecorder) LoadLLMTask(event ProgressEvent, result ProgressResult) {
	r.loads = append(r.loads, lifecycleProgressLoad{event: event, result: result})
}

func (s lifecycleProgressSpan) End(err error, result ProgressResult) {
	s.parent.ends = append(s.parent.ends, lifecycleProgressEnd{event: s.event, err: err, result: result})
}

func newLifecycleStore() *lifecycleStore {
	return &lifecycleStore{sessions: map[string]ledger.Session{}}
}

func (s *lifecycleStore) InsertSession(_ context.Context, session ledger.Session) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.sessions[session.SessionRowID] = session
	s.inserted = append(s.inserted, session)
	return nil
}

func (s *lifecycleStore) GetSession(_ context.Context, rowID string) (ledger.Session, error) {
	if s.getErr != nil {
		return ledger.Session{}, s.getErr
	}
	session, ok := s.sessions[rowID]
	if !ok {
		return ledger.Session{}, ledger.ErrNotFound
	}
	return session, nil
}

func (s *lifecycleStore) session(t *testing.T, rowID string) ledger.Session {
	t.Helper()
	session, err := s.GetSession(context.Background(), rowID)
	if err != nil {
		t.Fatalf("GetSession(%s): %v", rowID, err)
	}
	return session
}

func sequence(prefix string) func() string {
	next := 0
	return func() string {
		next++
		return prefix + "-" + strconv.Itoa(next)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test helper reads path under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want contains %q", path, string(data), want)
	}
}

func assertFileOmits(t *testing.T, path, unwanted string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test helper reads path under t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if strings.Contains(string(data), unwanted) {
		t.Fatalf("%s = %q, want it to omit %q", path, string(data), unwanted)
	}
}

func intPtr(value int) *int {
	return &value
}

var _ Store = (*lifecycleStore)(nil)
