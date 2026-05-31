//go:build unix || windows

package runlock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

const helperEnv = "GO_WANT_RUNLOCK_HELPER_PROCESS"

func TestAcquireCreatesLockAndReleaseAllowsReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "run.lock")

	lock := acquireLock(t, path)
	if !fileExists(path) {
		t.Fatalf("lock file %s does not exist", path)
	}
	assertPerm(t, filepath.Dir(path), dirPerm)
	assertPerm(t, path, filePerm)
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	reacquired := acquireLock(t, path)
	if err := reacquired.Release(); err != nil {
		t.Fatalf("reacquired Release: %v", err)
	}
}

func TestSecondAcquireSamePathFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	lock := acquireLock(t, path)
	defer releaseLock(t, lock)

	start := time.Now()
	_, err := Acquire(path)
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire error = %v, want ErrHeld", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("second Acquire took %v, want fail-fast", elapsed)
	}
}

func TestDistinctLockPathsCanBeHeldTogether(t *testing.T) {
	dir := t.TempDir()
	first := acquireLock(t, filepath.Join(dir, "first.lock"))
	defer releaseLock(t, first)
	second := acquireLock(t, filepath.Join(dir, "second.lock"))
	defer releaseLock(t, second)
}

func TestAcquireRejectsEmptyPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if _, err := Acquire(" "); err == nil {
		t.Fatal("Acquire empty path error = nil, want error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Acquire empty path created %#v, want no filesystem side effects", entries)
	}
}

func TestAcquireStatepathsLockFile(t *testing.T) {
	layout := statepaths.NewLayout(filepath.Join(t.TempDir(), statepaths.AppDir), filepath.Join(t.TempDir(), statepaths.AppDir))
	paths, err := layout.Run(statepaths.RunSpec{
		Host:            "github",
		Owner:           "open-cli",
		Repo:            "codereview-cli",
		PRNumber:        20,
		HeadSHA:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Profile:         "default",
		PostingIdentity: "reviewer@example.com",
		Attempt:         1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	lock := acquireLock(t, paths.LockFile)
	defer releaseLock(t, lock)
	if !fileExists(paths.LockFile) {
		t.Fatalf("statepaths lock file %s does not exist", paths.LockFile)
	}
}

func TestSubprocessCrashReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	// #nosec G204 G702 -- subprocess is the current test binary with controlled test arguments.
	cmd := exec.Command(os.Args[0], "-test.run=TestRunlockHelperProcess", "--", path)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start helper: %v", err)
	}
	reader := bufio.NewReader(stdout)
	ready := make(chan error, 1)
	go func() {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			ready <- readErr
			return
		}
		if line != "ready\n" {
			ready <- fmt.Errorf("helper output = %q, want ready", line)
			return
		}
		ready <- nil
	}()

	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("helper readiness: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("helper did not acquire lock in time")
	}

	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("Acquire while helper holds lock error = %v, want ErrHeld", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill helper: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper Wait error = nil, want killed process error")
	}

	lock := acquireLock(t, path)
	releaseLock(t, lock)
}

func TestRunlockHelperProcess(_ *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	if len(os.Args) == 0 {
		os.Exit(2)
	}
	path := os.Args[len(os.Args)-1]
	lock, err := Acquire(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Acquire helper lock: %v\n", err)
		os.Exit(3)
	}
	defer func() {
		_ = lock.Release()
	}()
	fmt.Fprintln(os.Stdout, "ready")
	select {}
}

func acquireLock(t *testing.T, path string) *Lock {
	t.Helper()
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire(%s): %v", path, err)
	}
	return lock
}

func releaseLock(t *testing.T, lock *Lock) {
	t.Helper()
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
