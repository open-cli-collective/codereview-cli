//go:build !windows

package llm

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLaunchProcessUnblocksReadOnCanceledContext reproduces the subprocess
// wedge: a worker that escapes the process group (becomes its own group leader)
// holds stdout open, so cmd.Cancel's group kill misses it and a consumer's
// synchronous read of Stdout() never sees EOF. LaunchProcess must force the
// pipes closed a grace after the context ends so the read unblocks. Without the
// on-cancel pipe close this test times out.
func TestLaunchProcessUnblocksReadOnCanceledContext(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required to spawn a process-group-escaping worker")
	}
	prev := subprocessWaitDelay
	subprocessWaitDelay = 100 * time.Millisecond
	defer func() { subprocessWaitDelay = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	// The worker os.setsid()s out of the process group, prints READY on the
	// inherited stdout, then holds it open. Reading READY proves it has escaped
	// and owns the pipe. The parent sleeps so it (and the group kill) can't
	// close stdout on its own.
	script := `python3 -c 'import os,sys,time; os.setsid(); print("READY"); sys.stdout.flush(); time.sleep(30)' & sleep 20`
	logPath := filepath.Join(t.TempDir(), "subprocess.log")
	p, err := LaunchProcess(ctx, "/bin/sh", []string{"-c", script}, "", nil, 0, logPath, func() error { return nil }, false)
	if err != nil {
		t.Fatalf("LaunchProcess: %v", err)
	}

	rd := bufio.NewReader(p.Stdout())
	line, err := rd.ReadString('\n')
	if err != nil || !strings.Contains(line, "READY") {
		t.Fatalf("worker did not signal READY: %q err=%v", line, err)
	}

	cancel() // simulate task-timeout / abort; the escaped worker still holds stdout

	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(rd)
		close(done)
	}()
	select {
	case <-done:
		// unblocked as expected
	case <-time.After(5 * time.Second):
		t.Fatal("stdout read wedged after context cancel: pipes were not force-closed")
	}
	_ = p.Command().Wait()
}
