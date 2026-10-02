//go:build unix

package llmadapters

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Process-group assertions are POSIX-specific; the CI runtime gate runs on
// Linux.
func TestPiRPCRuntimeCancellationStopsPi(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		cancel  bool
		want    error
	}{
		{name: "caller cancellation", timeout: piRuntimeTaskTimeout, cancel: true, want: context.Canceled},
		{name: "task deadline", timeout: 3 * time.Second, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			disconnected := make(chan struct{})
			fixture := newPiRuntimeFixture(t, piRuntimeStall(started, disconnected))
			// exec keeps the wrapper's PID, which is also the process group the
			// adapter starts Pi in.
			pidPath := filepath.Join(fixture.root, "pi.pid")
			wrapperPath := filepath.Join(fixture.root, "pi-wrapper.sh")
			fixture.writeFile(wrapperPath, "#!/bin/sh\necho $$ > "+strconv.Quote(pidPath)+"\nexec "+strconv.Quote(fixture.piPath)+" \"$@\"\n")
			if err := os.Chmod(wrapperPath, 0o700); err != nil { // #nosec G302 -- test wrapper must be executable and is rooted in t.TempDir.
				t.Fatalf("Chmod(wrapper): %v", err)
			}
			fixture.command = wrapperPath

			ctx, cancel := context.WithTimeout(context.Background(), piRuntimeTestTimeout)
			defer cancel()
			stream, err := fixture.adapter(tc.timeout).Start(ctx, Request{Model: piRuntimeModel, Prompt: `Return {"ok":true}.`})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			select {
			case <-started:
			case <-time.After(30 * time.Second):
				cancel()
				_, _ = stream.Wait(context.Background())
				t.Fatal("Pi never reached the mock provider")
			}
			pid := readPiRuntimePID(t, pidPath)

			began := time.Now()
			if tc.cancel {
				cancel()
			}
			response, err := stream.Wait(ctx)
			elapsed := time.Since(began)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Wait error = %v, want %v", err, tc.want)
			}
			if len(response.StructuredOutput) != 0 {
				t.Fatalf("StructuredOutput = %q, want none after %s", response.StructuredOutput, tc.name)
			}
			limit := 10 * time.Second
			if !tc.cancel {
				limit += tc.timeout
			}
			if elapsed > limit {
				t.Fatalf("Wait returned after %s, want termination within %s", elapsed, limit)
			}
			select {
			case <-disconnected:
			case <-time.After(5 * time.Second):
				t.Fatal("Pi kept the provider request open after the run ended")
			}
			eventually(t, 5*time.Second, func() bool {
				return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
			})
			if requests := fixture.provider.requests(); len(requests) != 1 {
				t.Fatalf("provider requests = %d, want the stalled request only", len(requests))
			}
		})
	}
}

func readPiRuntimePID(t *testing.T, path string) int {
	t.Helper()
	var data []byte
	eventually(t, 5*time.Second, func() bool {
		var err error
		data, err = os.ReadFile(path) // #nosec G304 -- path is rooted in the fixture's t.TempDir.
		return err == nil && strings.TrimSpace(string(data)) != ""
	})
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatalf("Pi PID file = %q: %v", data, err)
	}
	return pid
}
