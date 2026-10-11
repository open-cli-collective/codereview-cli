package llmlifecycle

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

func TestToolTraceSurvivesDurableReload(t *testing.T) {
	for _, callerOwned := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("caller_owned=%t/failed=%t", callerOwned, failed), func(t *testing.T) {
				want := lifecycleToolEvidence()
				adapter := &llm.FakeAdapter{NameValue: "fake-llm"}
				result := llm.FakeResult{SessionID: "session", Response: llm.Response{
					StructuredOutput: []byte(`{"ok":true}`), ReviewerToolEvidence: want,
				}}
				adapter.Queue(result)
				req := lifecycleRequest(t, newLifecycleStore(), adapter)
				if failed {
					// FakeAdapter's WaitErr discards Response. A real Pi failure can
					// return collected telemetry alongside the terminal error.
					req.Adapter = toolEvidenceWaitErrorAdapter{Adapter: adapter}
				}
				req.FailureStatus = StatusFailedIsolated
				if callerOwned {
					req.RunID = ""
					req.Store = nil
					req.AllowNoRunCache = true
				}
				_, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
				if (err != nil) != failed {
					t.Fatalf("fresh error = %v, failure expected %t", err, failed)
				}
				meta, exists, err := ReadMetadata(req.Paths, req.TaskID)
				if err != nil || !exists || !reflect.DeepEqual(meta.ReviewerToolEvidence, want) {
					t.Fatalf("persisted evidence = %#v, exists %t err %v", meta.ReviewerToolEvidence, exists, err)
				}
				cachedAdapter := &llm.FakeAdapter{NameValue: "fake-llm"}
				req.Adapter = cachedAdapter
				got, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
				if (err != nil) != failed || !got.Cached || !reflect.DeepEqual(got.Draft.Response.ReviewerToolEvidence, want) {
					t.Fatalf("cached = %t, evidence %#v, err %v", got.Cached, got.Draft.Response.ReviewerToolEvidence, err)
				}
				if len(cachedAdapter.Requests()) != 0 || len(cachedAdapter.Resumes()) != 0 {
					t.Fatal("durable reuse called the provider")
				}
			})
		}
	}
}

type toolEvidenceWaitErrorAdapter struct{ llm.Adapter }

func (adapter toolEvidenceWaitErrorAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	stream, err := adapter.Adapter.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	return toolEvidenceWaitErrorStream{Stream: stream}, nil
}

type toolEvidenceWaitErrorStream struct{ llm.Stream }

func (stream toolEvidenceWaitErrorStream) Wait(ctx context.Context) (llm.Response, error) {
	response, err := stream.Stream.Wait(ctx)
	if err != nil {
		return response, err
	}
	return response, errors.New("provider stopped after tool execution")
}

func TestToolTraceSchemaRejectsPreEvidenceCacheBeforeProviderCall(t *testing.T) {
	for _, oldSchema := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(oldSchema), func(t *testing.T) {
			adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
			req := lifecycleRequest(t, newLifecycleStore(), adapter)
			meta := BaseMetadata(req, SessionDraft{ProviderSessionID: "old-session"})
			meta.SchemaVersion = oldSchema
			meta.Status = StatusFailedBlocking
			if err := WriteMetadata(req.Paths, meta); err != nil {
				t.Fatal(err)
			}
			_, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
			if err == nil || !strings.Contains(err.Error(), "schema version") || !strings.Contains(err.Error(), "want 4") {
				t.Fatalf("error = %v, want schema rejection", err)
			}
			if len(adapter.Requests()) != 0 || len(adapter.Resumes()) != 0 {
				t.Fatal("old schema started or resumed a provider session")
			}
		})
	}
}

func TestToolTraceOnResumeBelongsOnlyToCurrentInvocation(t *testing.T) {
	adapter := &llm.FakeAdapter{NameValue: "fake-llm", SupportsResumeValue: true}
	req := lifecycleRequest(t, newLifecycleStore(), adapter)
	prior := lifecycleToolEvidence()
	prior.Trace.Calls[0].Path = "prior.go"
	meta := BaseMetadata(req, SessionDraft{ProviderSessionID: "prior-session", Response: llm.Response{ReviewerToolEvidence: prior}})
	meta.Status = StatusFailedBlocking
	if err := WriteMetadata(req.Paths, meta); err != nil {
		t.Fatal(err)
	}
	want := lifecycleToolEvidence()
	adapter.Queue(llm.FakeResult{SessionID: "resumed-session", Response: llm.Response{StructuredOutput: []byte(`{"ok":true}`), ReviewerToolEvidence: want}})
	got, err := RunStructured(context.Background(), req, decodeLifecyclePayload)
	if err != nil || !reflect.DeepEqual(got.Draft.Response.ReviewerToolEvidence, want) {
		t.Fatalf("resumed evidence = %#v, err %v", got.Draft.Response.ReviewerToolEvidence, err)
	}
	if resumes := adapter.Resumes(); len(resumes) != 1 || resumes[0].SessionID != "prior-session" {
		t.Fatalf("resumes = %#v, want prior session", resumes)
	}
	restored, exists, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil || !exists || !reflect.DeepEqual(restored.ReviewerToolEvidence, want) {
		t.Fatalf("resumed metadata = %#v, exists %t err %v", restored.ReviewerToolEvidence, exists, err)
	}
}

func TestToolTraceMetadataSnapshotsDoNotShareMutableState(t *testing.T) {
	evidence := lifecycleToolEvidence()
	meta := BaseMetadata(lifecycleRequest(t, newLifecycleStore(), &llm.FakeAdapter{}), SessionDraft{Response: llm.Response{ReviewerToolEvidence: evidence}})
	draft := SessionDraftFromMetadata(meta)
	draft.Response.ReviewerToolEvidence.Trace.Calls[0].Range.Offset = 99
	evidence.Trace.Calls[0].Output.Bytes = 99
	if !reflect.DeepEqual(meta.ReviewerToolEvidence, lifecycleToolEvidence()) {
		t.Fatal("metadata shares mutable trace state with a response or restored draft")
	}
}

func TestUnsupportedToolTraceRemainsAbsentInMetadata(t *testing.T) {
	req := lifecycleRequest(t, newLifecycleStore(), &llm.FakeAdapter{})
	meta := BaseMetadata(req, SessionDraft{})
	if err := WriteMetadata(req.Paths, meta); err != nil {
		t.Fatal(err)
	}
	got, exists, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil || !exists || got.ReviewerToolEvidence != nil || SessionDraftFromMetadata(got).Response.ReviewerToolEvidence != nil {
		t.Fatalf("unsupported evidence = %#v, exists %t err %v", got.ReviewerToolEvidence, exists, err)
	}
}

func lifecycleToolEvidence() *llm.ReviewerToolEvidence {
	return &llm.ReviewerToolEvidence{DiffStatus: llm.DiffToolStatusSucceeded, Trace: &llm.ReviewerToolTrace{
		Version: 1, Source: "pi_rpc", StreamComplete: false, Truncated: true, DroppedEvents: 7,
		Calls: []llm.ReviewerToolCallEvidence{{
			CallID: "read-1", Tool: "cr_read", Path: "src/file.go", ReadView: llm.ReviewerToolReadFile, Status: llm.ReviewerToolCallSucceeded,
			StartObserved: true, EndObserved: true,
			Range: &llm.ReviewerToolRange{Offset: 12, Limit: 30}, Output: &llm.ReviewerToolOutput{Bytes: 30, Truncated: true},
		}, {
			CallID: "read-2", Tool: "cr_read", Path: "context.go", ReadView: llm.ReviewerToolReadSymlink, Status: llm.ReviewerToolCallIncomplete,
			StartObserved: true, ProvenanceIssue: "missing_end", Range: &llm.ReviewerToolRange{},
		}},
	}}
}
