package llm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeTranscriptUsage(t *testing.T) {
	t.Run("sums assistant turns and dedupes streamed repeats", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		transcript := `{"type":"user","message":{"id":"u1"}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":3,"cache_creation_input_tokens":7225,"cache_read_input_tokens":3266,"output_tokens":50}}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":1,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"output_tokens":300}}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":1,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"output_tokens":400}}}
not json at all
{"type":"assistant","message":{"id":"","usage":{"input_tokens":999}}}
{"type":"assistant","message":{"id":"m3"}}
`
		if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		usage := claudeBGTranscriptUsage(map[string]any{"linkScanPath": path})
		if usage.TokensIn == nil || *usage.TokensIn != 4 ||
			usage.TokensOut == nil || *usage.TokensOut != 450 ||
			usage.CacheRead == nil || *usage.CacheRead != 3466 ||
			usage.CacheCreate == nil || *usage.CacheCreate != 7325 {
			t.Fatalf("usage = %#v, want deduped sums (in=4 out=450 read=3466 create=7325)", usage)
		}
		if usage.CostUSD != nil {
			t.Fatalf("cost = %v, want nil (transcripts carry no cost)", *usage.CostUSD)
		}
	})

	t.Run("missing transcript or path yields nullable usage", func(t *testing.T) {
		for name, state := range map[string]map[string]any{
			"no path":      {},
			"blank path":   {"linkScanPath": "  "},
			"missing file": {"linkScanPath": filepath.Join(t.TempDir(), "absent.jsonl")},
		} {
			if usage := claudeBGTranscriptUsage(state); usage != (Usage{}) {
				t.Fatalf("%s: usage = %#v, want empty", name, usage)
			}
		}
	})

	t.Run("transcript without assistant usage yields nullable usage", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(`{"type":"user","message":{"id":"u1"}}`+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if usage := claudeTranscriptUsage(path); usage != (Usage{}) {
			t.Fatalf("usage = %#v, want empty", usage)
		}
	})
}
