package progress

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoggerStartAndEndFormatsLines(t *testing.T) {
	var out bytes.Buffer
	clock := &fixedClock{
		times: []time.Time{
			time.Unix(10, 0),
			time.Unix(10, 25*int64(time.Millisecond)),
		},
	}
	logger := New(&out, false, clock.Now)

	span := logger.Start("benchmark.run", "run_suite", "suite=one")
	span.End(nil)

	got := out.String()
	if !strings.Contains(got, `cr progress event=start command="benchmark.run" op="run_suite" target="suite\=one"`) {
		t.Fatalf("start line = %q", got)
	}
	if !strings.Contains(got, `cr progress event=finish command="benchmark.run" op="run_suite" target="suite\=one" duration_ms=25 status=ok`) {
		t.Fatalf("finish line = %q", got)
	}
}

func TestLoggerEndErrorSanitizesSummary(t *testing.T) {
	var out bytes.Buffer
	clock := &fixedClock{
		times: []time.Time{
			time.Unix(20, 0),
			time.Unix(20, 50*int64(time.Millisecond)),
		},
	}
	logger := New(&out, false, clock.Now)

	span := logger.Start("config.clear", "cache_cleanup", "session")
	span.End(errors.New("failed to remove /tmp/private/file\ntry again"))

	got := out.String()
	if !strings.Contains(got, `event=error`) || !strings.Contains(got, `status=error`) {
		t.Fatalf("error line = %q", got)
	}
	if strings.Contains(got, "/tmp/private/file") {
		t.Fatalf("error line leaked path: %q", got)
	}
	if !strings.Contains(got, `error="failed to remove [path] try again"`) {
		t.Fatalf("error summary = %q", got)
	}
}

func TestLoggerFieldsAreRenderedAndSorted(t *testing.T) {
	var out bytes.Buffer
	clock := &fixedClock{
		times: []time.Time{
			time.Unix(40, 0),
			time.Unix(40, 15*int64(time.Millisecond)),
		},
	}
	logger := New(&out, false, clock.Now)

	span := logger.StartFields("review", "run_llm", "llm",
		Field{Key: "session_id", Value: "pending"},
		Field{Key: "model", Value: "gpt-5.5"},
		Field{Key: "provider", Value: "openai"},
	)
	span.EndFields(nil, Field{Key: "session_id", Value: "sess-123"})

	got := out.String()
	if !strings.Contains(got, `command="review" op="run_llm" target="llm" model="gpt-5.5" provider="openai" session_id="pending"`) {
		t.Fatalf("start line = %q", got)
	}
	if !strings.Contains(got, `command="review" op="run_llm" target="llm" model="gpt-5.5" provider="openai" session_id="sess-123" duration_ms=15 status=ok`) {
		t.Fatalf("finish line = %q", got)
	}
}

func TestDisabledLoggerWritesNothing(t *testing.T) {
	var out bytes.Buffer
	logger := New(&out, true, time.Now)

	span := logger.Start("sessions.delete", "delete", "session")
	span.End(nil)

	if out.Len() != 0 {
		t.Fatalf("output = %q, want empty", out.String())
	}
}

func TestEndOnlyWritesOnce(t *testing.T) {
	var out bytes.Buffer
	clock := &fixedClock{
		times: []time.Time{
			time.Unix(30, 0),
			time.Unix(30, 10*int64(time.Millisecond)),
			time.Unix(30, 20*int64(time.Millisecond)),
		},
	}
	logger := New(&out, false, clock.Now)

	span := logger.Start("data.prune", "prune", "data-root")
	span.End(nil)
	span.End(nil)

	if got := strings.Count(out.String(), "event=finish"); got != 1 {
		t.Fatalf("finish count = %d, want 1; output = %q", got, out.String())
	}
}

type fixedClock struct {
	times []time.Time
}

func (c *fixedClock) Now() time.Time {
	if len(c.times) == 0 {
		return time.Unix(0, 0)
	}
	t := c.times[0]
	c.times = c.times[1:]
	return t
}
