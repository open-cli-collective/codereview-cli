package llm

import (
	"os"
	"path/filepath"
	"testing"
)

const transcriptJobCreatedAt = "2026-06-09T20:00:00.000Z"

func writeTranscript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestClaudeTranscriptUsage(t *testing.T) {
	t.Run("sums job-scoped assistant turns and dedupes streamed repeats", func(t *testing.T) {
		path := writeTranscript(t, `{"type":"user","timestamp":"2026-06-09T20:00:01Z","message":{"id":"u1"}}
{"type":"assistant","timestamp":"2026-06-09T20:00:02Z","message":{"id":"m1","usage":{"input_tokens":3,"cache_creation_input_tokens":7225,"cache_read_input_tokens":3266,"output_tokens":50}}}
{"type":"assistant","timestamp":"2026-06-09T20:00:03Z","message":{"id":"m2","usage":{"input_tokens":1,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"output_tokens":300}}}
{"type":"assistant","timestamp":"2026-06-09T20:00:04Z","message":{"id":"m2","usage":{"input_tokens":1,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"output_tokens":400}}}
not json at all
{"type":"assistant","timestamp":"2026-06-09T20:00:05Z","message":{"id":"","usage":{"input_tokens":999}}}
{"type":"assistant","timestamp":"2026-06-09T20:00:06Z","message":{"id":"m3"}}
`)

		usage := claudeBGTranscriptUsage(map[string]any{"linkScanPath": path, "createdAt": transcriptJobCreatedAt})
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

	t.Run("prior-session turns before job creation are excluded", func(t *testing.T) {
		path := writeTranscript(t, `{"type":"assistant","timestamp":"2026-06-09T19:30:00Z","message":{"id":"old","usage":{"input_tokens":5000,"output_tokens":5000}}}
{"type":"assistant","timestamp":"2026-06-09T20:00:02Z","message":{"id":"current","usage":{"input_tokens":10,"output_tokens":20}}}
`)

		usage := claudeBGTranscriptUsage(map[string]any{"linkScanPath": path, "createdAt": transcriptJobCreatedAt})
		if usage.TokensIn == nil || *usage.TokensIn != 10 || usage.TokensOut == nil || *usage.TokensOut != 20 {
			t.Fatalf("usage = %#v, want only the current job's turn (in=10 out=20)", usage)
		}
	})

	t.Run("unscopeable usage event makes the whole transcript unavailable", func(t *testing.T) {
		path := writeTranscript(t, `{"type":"assistant","message":{"id":"untimed","usage":{"input_tokens":7000,"output_tokens":7000}}}
{"type":"assistant","timestamp":"2026-06-09T20:00:02Z","message":{"id":"current","usage":{"input_tokens":10,"output_tokens":20}}}
`)

		if usage := claudeBGTranscriptUsage(map[string]any{"linkScanPath": path, "createdAt": transcriptJobCreatedAt}); usage != (Usage{}) {
			t.Fatalf("usage = %#v, want empty (no silently partial sums)", usage)
		}
	})

	t.Run("unprovable job scope yields nullable usage", func(t *testing.T) {
		path := writeTranscript(t, `{"type":"assistant","timestamp":"2026-06-09T20:00:02Z","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":20}}}`+"\n")
		for name, state := range map[string]map[string]any{
			"no path":           {"createdAt": transcriptJobCreatedAt},
			"blank path":        {"linkScanPath": "  ", "createdAt": transcriptJobCreatedAt},
			"missing file":      {"linkScanPath": filepath.Join(t.TempDir(), "absent.jsonl"), "createdAt": transcriptJobCreatedAt},
			"missing createdAt": {"linkScanPath": path},
			"bad createdAt":     {"linkScanPath": path, "createdAt": "yesterday-ish"},
		} {
			if usage := claudeBGTranscriptUsage(state); usage != (Usage{}) {
				t.Fatalf("%s: usage = %#v, want empty", name, usage)
			}
		}
	})

	t.Run("transcript without assistant usage yields nullable usage", func(t *testing.T) {
		path := writeTranscript(t, `{"type":"user","timestamp":"2026-06-09T20:00:01Z","message":{"id":"u1"}}`+"\n")
		usage := claudeBGTranscriptUsage(map[string]any{"linkScanPath": path, "createdAt": transcriptJobCreatedAt})
		if usage != (Usage{}) {
			t.Fatalf("usage = %#v, want empty", usage)
		}
	})
}
