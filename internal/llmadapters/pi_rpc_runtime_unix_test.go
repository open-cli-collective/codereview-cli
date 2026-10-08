//go:build unix

package llmadapters

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

// A blank Pi pin must stop both Make targets: it would make the runtime tests
// skip and report ok, and it would let npm install the latest Pi. A fake go on
// PATH records any dispatch instead of running the suite.
func TestPiRuntimeMakeTargetsRejectBlankPin(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not installed")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("Abs(repo root): %v", err)
	}
	binDir := t.TempDir()
	recordPath := filepath.Join(t.TempDir(), "go-dispatch")
	fakeGo := "#!/bin/sh\nprintf '%s' \"$CR_PI_RUNTIME_VERSION\" > " + strconv.Quote(recordPath) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(fakeGo), 0o700); err != nil { // #nosec G306 -- fake go must be executable and is rooted in t.TempDir.
		t.Fatalf("WriteFile(fake go): %v", err)
	}
	env := []string{"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, entry := range os.Environ() {
		switch key, _, _ := strings.Cut(entry, "="); key {
		case "PATH", "MAKEFLAGS", "MFLAGS", "MAKELEVEL", "PI_RUNTIME_VERSION", piRuntimeVersionEnv:
		default:
			env = append(env, entry)
		}
	}
	runMake := func(args ...string) (string, bool, error) {
		t.Helper()
		_ = os.Remove(recordPath)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, makePath, append([]string{"-s", "--no-print-directory", "-C", repoRoot}, args...)...) // #nosec G204 -- test runs make with fixed targets.
		cmd.Env = env
		output, err := cmd.Output()
		recorded, readErr := os.ReadFile(recordPath) // #nosec G304 -- recordPath is rooted in t.TempDir.
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("ReadFile(go dispatch record): %v", readErr)
		}
		if readErr == nil {
			return string(recorded), true, err
		}
		return string(output), false, err
	}

	pin, dispatched, err := runMake("pi-runtime-version")
	pin = strings.TrimSpace(pin)
	if err != nil || dispatched || pin == "" || strings.ContainsAny(pin, " \t") {
		t.Fatalf("make pi-runtime-version = %q (err %v), want one default version", pin, err)
	}
	if got, dispatched, err := runMake("test-pi-runtime"); err != nil || !dispatched || got != pin {
		t.Fatalf("make test-pi-runtime dispatched=%v with %s=%q (err %v), want go test with %q", dispatched, piRuntimeVersionEnv, got, err, pin)
	}
	for _, bad := range []string{"", " ", "\t", pin + " " + pin} {
		if got, dispatched, err := runMake("pi-runtime-version", "PI_RUNTIME_VERSION="+bad); err == nil || dispatched || strings.TrimSpace(got) != "" {
			t.Errorf("make pi-runtime-version PI_RUNTIME_VERSION=%q printed %q (err %v), want failure without output", bad, got, err)
		}
		if got, dispatched, err := runMake("test-pi-runtime", "PI_RUNTIME_VERSION="+bad); err == nil || dispatched {
			t.Errorf("make test-pi-runtime PI_RUNTIME_VERSION=%q dispatched=%v with %q (err %v), want failure before go test", bad, dispatched, got, err)
		}
	}
}
