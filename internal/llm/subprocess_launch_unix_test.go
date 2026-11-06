//go:build !windows

package llm

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

	// Keep the Unix socket path short, including on macOS. This private control
	// channel survives cancellation and never shares the pipe under test.
	controlDir, err := os.MkdirTemp("", "llm-control-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(controlDir) }()
	controlPath := filepath.Join(controlDir, "worker.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: controlPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The worker detaches before signaling readiness and holds both inherited
	// pipes until explicitly released. EOF on the test-owned control socket also
	// releases it if the test exits early; the timeout is a final safety bound.
	// The shell waits instead of using a timing-only sleep.
	script := `python3 -c '
import os, socket, sys, time
os.setsid()
control = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
control.settimeout(30)
control.connect(sys.argv[1])
deadline = time.monotonic() + 30
try:
    print("READY", flush=True)
    command = b""
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        control.settimeout(remaining)
        command = control.recv(1)
        if command != b"P":
            break
        control.sendall(b"P")
finally:
    os.close(1)
    os.close(2)
if command == b"Q":
    control.sendall(b"Q")
control.close()
' "$1" & wait`
	logPath := filepath.Join(t.TempDir(), "subprocess.log")
	p, err := LaunchProcess(ctx, "/bin/sh", []string{"-c", script, "worker", controlPath}, "", nil, 0, logPath, func() error { return nil }, false)
	if err != nil {
		t.Fatalf("LaunchProcess: %v", err)
	}
	var control *net.UnixConn
	defer func() {
		// Release only after the pipe assertion (or an earlier fatal failure).
		// Q acknowledges that both inherited pipe descriptors have been closed.
		if control != nil {
			if err := control.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Error(err)
			} else if _, err := control.Write([]byte("Q")); err != nil {
				t.Errorf("release worker: %v", err)
			} else {
				var reply [1]byte
				if _, err := io.ReadFull(control, reply[:]); err != nil || reply[0] != 'Q' {
					t.Errorf("worker release acknowledgment = %q, err=%v", reply, err)
				}
				if n, err := control.Read(reply[:]); n != 0 || !errors.Is(err, io.EOF) {
					t.Errorf("worker control did not close: n=%d err=%v", n, err)
				}
			}
			_ = control.Close()
		}
		cancel()
		_ = p.Stdout().Close()
		_ = p.Stderr().Close()
		waited := make(chan struct{})
		go func() {
			_ = p.Command().Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Error("launcher did not exit during cleanup")
		}
		closeSubprocessLog(p.logFile)
		_ = p.processGroup.close()
	}()
	control, err = listener.AcceptUnix()
	if err != nil {
		t.Fatalf("accept worker control: %v", err)
	}

	rd := bufio.NewReader(p.Stdout())
	ready := make(chan bool, 1)
	go func() {
		line, err := rd.ReadString('\n')
		ready <- err == nil && line == "READY\n"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("worker did not signal READY")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker readiness timed out")
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
	// A live response rules out helper exit as the reason the read unblocked.
	// Do not release the worker or call Wait before this assertion.
	if err := control.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Write([]byte("P")); err != nil {
		t.Fatalf("probe escaped worker: %v", err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(control, reply[:]); err != nil || reply[0] != 'P' {
		t.Fatalf("escaped worker is not holding the pipes: reply=%q err=%v", reply, err)
	}
}
