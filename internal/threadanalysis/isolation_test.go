package threadanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/llmlifecycle"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

func TestAnalyzeThreadsIsolatesInvalidOutputAndCachesFailure(t *testing.T) {
	threads := []threadcontext.Thread{promptThreadWithID("thread-1", "first"), promptThreadWithID("thread-2", "second"), promptThreadWithID("thread-3", "third")}
	adapter := &llm.FakeAdapter{NameValue: "fake", SupportsResumeValue: true}
	adapter.Queue(llm.FakeResult{SessionID: "session-1", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-1"))}})
	invalid := []byte(`{"thread_id":"thread-2","decision":"invalid","resolve":false}`)
	adapter.Queue(llm.FakeResult{SessionID: "session-2-initial", Response: llm.Response{StructuredOutput: invalid}})
	adapter.Queue(llm.FakeResult{SessionID: "session-2-retry", Response: llm.Response{StructuredOutput: invalid}})
	adapter.Queue(llm.FakeResult{SessionID: "session-3", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-3"))}})
	opts := testOptions(t, newFakeStore(), adapter)
	opts.IsolateFailures = true
	var checkpoints []string
	opts.OnSessionID = func(id string) error { checkpoints = append(checkpoints, id); return nil }
	logPath := func(thread threadcontext.Thread) (string, error) { return string(thread.ID) + ".log", nil }
	results, failures, err := AnalyzeThreads(context.Background(), opts, threads, logPath)
	if err != nil {
		t.Fatalf("AnalyzeThreads: %v", err)
	}
	if len(results) != 2 || results[0].ThreadID != "thread-1" || results[1].ThreadID != "thread-3" || len(failures) != 1 || failures[0].ThreadID != "thread-2" || !strings.Contains(failures[0].Error, "invalid") {
		t.Fatalf("results/failures = %#v/%#v, want successful siblings and failed thread diagnostic", results, failures)
	}
	if !reflect.DeepEqual(checkpoints, []string{"session-1", "session-2-retry", "session-3"}) {
		t.Fatalf("checkpoints = %#v, want failed session checkpoint before next thread", checkpoints)
	}
	meta := readThreadMetadata(t, opts, "thread-2")
	if meta.Status != llmlifecycle.StatusFailedIsolated || len(meta.Attempts) != 2 || meta.ValidatedOutputPath != "" {
		t.Fatalf("failed task = %#v, want isolated failure with attempts and no usable output", meta)
	}
	resumes := adapter.Resumes()
	if len(resumes) != 3 || resumes[2].SessionID != "session-2-retry" {
		t.Fatalf("resumes = %#v, want third thread to preserve existing session chain", resumes)
	}

	cached := &llm.FakeAdapter{NameValue: "fake", SupportsResumeValue: true}
	opts.Adapter = cached
	cachedResults, cachedFailures, err := AnalyzeThreads(context.Background(), opts, threads, logPath)
	if err != nil || !reflect.DeepEqual(cachedResults, results) || !reflect.DeepEqual(cachedFailures, failures) {
		t.Fatalf("cached batch = %#v/%#v err=%v, want identical partial outcome", cachedResults, cachedFailures, err)
	}
	if len(cached.Requests())+len(cached.Resumes()) != 0 {
		t.Fatal("cached isolated failure triggered provider work")
	}
	changed := append([]threadcontext.Thread(nil), threads...)
	changed[1] = promptThreadWithID("thread-2", "changed")
	if _, _, err := AnalyzeThreads(context.Background(), opts, changed, logPath); err == nil || !strings.Contains(err.Error(), "input fingerprint changed") {
		t.Fatalf("changed failed-thread input error = %v, want fail-closed stale cache", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := AnalyzeThreads(ctx, opts, threads, logPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cached batch error = %v, want cancellation", err)
	}

	fresh := &llm.FakeAdapter{NameValue: "fake"}
	for _, thread := range threads {
		fresh.Queue(llm.FakeResult{Response: llm.Response{StructuredOutput: []byte(validSkipOutput(string(thread.ID)))}})
	}
	opts.Adapter = fresh
	opts.RunID = "fresh-run"
	opts.LifecyclePaths = llmlifecycle.Paths{LLMTasksDir: filepath.Join(t.TempDir(), "llm-tasks")}
	freshResults, freshFailures, err := AnalyzeThreads(context.Background(), opts, threads, logPath)
	if err != nil || len(freshResults) != 3 || len(freshFailures) != 0 || len(fresh.Requests()) != 3 {
		t.Fatalf("fresh run = %#v/%#v err=%v, want all analyses retried", freshResults, freshFailures, err)
	}
}

func TestAnalyzeThreadsIsolationPreservesBlockingFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    llm.FakeResult
		configure func(*Options)
		want      error
	}{
		{name: "start without execution evidence", result: llm.FakeResult{StartErr: errors.New("auth unavailable")}},
		{name: "cancellation after start", result: llm.FakeResult{SessionID: "started", WaitErr: context.Canceled}, want: context.Canceled},
		{name: "deadline after start", result: llm.FakeResult{SessionID: "started", WaitErr: context.DeadlineExceeded}, want: context.DeadlineExceeded},
		{name: "ledger failure", result: llm.FakeResult{SessionID: "started", WaitErr: errors.New("provider failed")}, configure: func(opts *Options) { opts.Store = &fakeStore{insertErr: errors.New("ledger unavailable")} }},
		{name: "checkpoint failure", result: llm.FakeResult{SessionID: "started", WaitErr: errors.New("provider failed")}, configure: func(opts *Options) {
			opts.OnSessionID = func(string) error { return errors.New("checkpoint unavailable") }
		}},
		{name: "artifact failure", configure: func(opts *Options) {
			path := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			opts.LifecyclePaths.LLMTasksDir = path
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &llm.FakeAdapter{NameValue: "fake"}
			adapter.Queue(tc.result)
			opts := testOptions(t, newFakeStore(), adapter)
			opts.IsolateFailures = true
			if tc.configure != nil {
				tc.configure(&opts)
			}
			results, failures, err := AnalyzeThreads(context.Background(), opts, []threadcontext.Thread{promptThread("one"), promptThreadWithID("thread-2", "two")}, func(threadcontext.Thread) (string, error) { return "thread.log", nil })
			if err == nil || len(results) != 0 || len(failures) != 0 {
				t.Fatalf("batch = %#v/%#v err=%v, want blocking error", results, failures, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if len(adapter.Requests()) > 1 {
				t.Fatal("blocking error allowed next thread to run")
			}
		})
	}
}

func TestAnalyzeThreadsRetriesLegacyBlockingFailureAndKeepsSuccess(t *testing.T) {
	threads := []threadcontext.Thread{promptThreadWithID("thread-1", "one"), promptThreadWithID("thread-2", "two")}
	seed := &llm.FakeAdapter{NameValue: "fake"}
	seed.Queue(llm.FakeResult{SessionID: "success", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-1"))}})
	seed.Queue(llm.FakeResult{SessionID: "failure", WaitErr: errors.New("legacy provider failure")})
	opts := testOptions(t, newFakeStore(), seed)
	logPath := func(thread threadcontext.Thread) (string, error) { return string(thread.ID) + ".log", nil }
	if _, _, err := AnalyzeThreads(context.Background(), opts, threads, logPath); err == nil {
		t.Fatal("default analysis batch must retain blocking behavior")
	}
	if meta := readThreadMetadata(t, opts, "thread-2"); meta.Status != llmlifecycle.StatusFailedBlocking {
		t.Fatalf("legacy metadata = %#v", meta)
	}
	retry := &llm.FakeAdapter{NameValue: "fake", SupportsResumeValue: true}
	retry.Queue(llm.FakeResult{SessionID: "recovered", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-2"))}})
	opts.Adapter, opts.IsolateFailures = retry, true
	results, failures, err := AnalyzeThreads(context.Background(), opts, threads, logPath)
	if err != nil || len(results) != 2 || len(failures) != 0 {
		t.Fatalf("legacy resume = %#v/%#v err=%v", results, failures, err)
	}
	if len(retry.Requests()) != 0 || len(retry.Resumes()) != 1 || retry.Resumes()[0].SessionID != "failure" {
		t.Fatalf("legacy retry starts/resumes = %#v/%#v, want failed task only", retry.Requests(), retry.Resumes())
	}
}

func TestAnalyzeThreadsPublicDiagnosticDoesNotExposeRejectedOutput(t *testing.T) {
	const sentinel = "@reviewers RAW_OUTPUT_SENTINEL"
	for _, output := range []string{
		`{"thread_id":"` + sentinel + `","decision":"skip","resolve":false}`,
		`{"thread_id":"thread-1","decision":"` + sentinel + `","resolve":false}`,
		`{"thread_id":"thread-1","decision":"skip","resolve":false,"` + sentinel + `":true}`,
		"",
	} {
		t.Run(output, func(t *testing.T) {
			adapter := &llm.FakeAdapter{NameValue: "fake"}
			if output == "" {
				adapter.Queue(llm.FakeResult{SessionID: "failed", WaitErr: errors.New(sentinel)})
			} else {
				for i := 0; i < 2; i++ {
					adapter.Queue(llm.FakeResult{SessionID: "failed", Response: llm.Response{StructuredOutput: []byte(output)}})
				}
			}
			opts := testOptions(t, newFakeStore(), adapter)
			opts.IsolateFailures = true
			logPath := func(threadcontext.Thread) (string, error) { return "thread.log", nil }
			_, failures, err := AnalyzeThreads(context.Background(), opts, []threadcontext.Thread{promptThread("one")}, logPath)
			if err != nil || len(failures) != 1 {
				t.Fatalf("failure outcome = %#v err=%v", failures, err)
			}
			if strings.Contains(failures[0].Error, sentinel) || strings.Contains(failures[0].Error, "@reviewers") {
				t.Fatalf("public diagnostic exposed rejected output: %#v", failures)
			}
			if meta := readThreadMetadata(t, opts, "thread-1"); !strings.Contains(strings.ToLower(meta.Error), strings.ToLower(sentinel)) {
				t.Fatalf("local diagnostic lost raw failure detail: %#v", meta)
			}
			cached := &llm.FakeAdapter{NameValue: "fake"}
			opts.Adapter = cached
			_, reused, err := AnalyzeThreads(context.Background(), opts, []threadcontext.Thread{promptThread("one")}, logPath)
			if err != nil || !reflect.DeepEqual(reused, failures) || len(cached.Requests()) != 0 {
				t.Fatalf("cached public diagnostic = %#v err=%v, want safe identical failure", reused, err)
			}
		})
	}
}

func TestAnalyzeThreadsIsolationRejectsCorruptCacheWithoutProviderWork(t *testing.T) {
	for _, corruption := range []string{"malformed metadata", "wrong schema", "invalid accepted output", "missing session"} {
		t.Run(corruption, func(t *testing.T) {
			store := newFakeStore()
			seed := &llm.FakeAdapter{NameValue: "fake"}
			seed.Queue(llm.FakeResult{SessionID: "success", Response: llm.Response{StructuredOutput: []byte(validSkipOutput("thread-1"))}})
			opts := testOptions(t, store, seed)
			opts.IsolateFailures = true
			logPath := func(threadcontext.Thread) (string, error) { return "thread.log", nil }
			threads := []threadcontext.Thread{promptThread("one")}
			if _, _, err := AnalyzeThreads(context.Background(), opts, threads, logPath); err != nil {
				t.Fatal(err)
			}
			meta := readThreadMetadata(t, opts, "thread-1")
			metadataPath, err := opts.LifecyclePaths.Metadata(meta.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "malformed metadata":
				if err := os.WriteFile(metadataPath, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong schema":
				meta.SchemaVersion++
				data, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(metadataPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "invalid accepted output":
				if err := os.WriteFile(meta.ValidatedOutputPath, []byte(`{"decision":"invalid"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing session":
				delete(store.sessions, meta.SessionRowID)
			}
			adapter := &llm.FakeAdapter{NameValue: "fake"}
			opts.Adapter = adapter
			results, failures, err := AnalyzeThreads(context.Background(), opts, threads, logPath)
			if err == nil || len(results) != 0 || len(failures) != 0 || len(adapter.Requests()) != 0 {
				t.Fatalf("corrupt-cache result = %#v/%#v err=%v, want fatal without provider call", results, failures, err)
			}
		})
	}
}
