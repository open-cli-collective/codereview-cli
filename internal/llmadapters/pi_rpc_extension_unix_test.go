//go:build unix

package llmadapters

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPiRPCReviewerHelperStaysInParentProcessGroup(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	tempDir := t.TempDir()
	pidPath := filepath.Join(tempDir, "helper.pid")
	helperPath := filepath.Join(tempDir, "helper.mjs")
	helper := "#!/usr/bin/env node\nimport fs from 'node:fs';\nfs.writeFileSync(process.env.PI_RPC_TEST_PID, String(process.pid));\nsetInterval(() => {}, 1000);\n"
	if err := os.WriteFile(helperPath, []byte(helper), 0o700); err != nil { // #nosec G306,G703 -- executable test helper is rooted in t.TempDir.
		t.Fatalf("WriteFile(helper): %v", err)
	}
	extensionPath := filepath.Join(tempDir, "extension.mjs")
	const helperTimeout = 5 * time.Second
	extension := piRPCReviewerExtension(helperPath, filepath.Join(tempDir, "config.json"), tempDir, 2048, helperTimeout, "")
	if err := os.WriteFile(extensionPath, []byte(extension), 0o600); err != nil { // #nosec G703 -- extensionPath is rooted in t.TempDir.
		t.Fatalf("WriteFile(extension): %v", err)
	}
	runnerPath := filepath.Join(tempDir, "runner.mjs")
	runner := "import extension from " + strconv.Quote(extensionPath) + ";\nconst tools = {};\nextension({ registerTool(tool) { tools[tool.name] = tool; } });\nvoid tools.cr_diff.execute('call-1', {}, new AbortController().signal).then(result => { if (result.isError) { console.error(JSON.stringify(result)); process.exitCode = 1; } });\n"
	if err := os.WriteFile(runnerPath, []byte(runner), 0o600); err != nil {
		t.Fatalf("WriteFile(runner): %v", err)
	}
	cmd := exec.Command(nodePath, runnerPath) // #nosec G204 -- test launches the discovered Node executable with a test-owned script.
	outputPath := filepath.Join(tempDir, "runner.log")
	output, err := os.Create(outputPath) // #nosec G304 -- outputPath is rooted in t.TempDir.
	if err != nil {
		t.Fatalf("Create(runner log): %v", err)
	}
	defer func() { _ = output.Close() }()
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.Dir = tempDir
	cmd.Env = append(os.Environ(), "PI_RPC_TEST_PID="+pidPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(runner): %v", err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		if t.Failed() {
			data, err := os.ReadFile(outputPath) // #nosec G304 -- outputPath is rooted in t.TempDir.
			if err != nil {
				t.Logf("ReadFile(runner log): %v", err)
			} else {
				t.Logf("runner output: %s", data)
			}
		}
	}()
	// Runner startup precedes the helper's own timeout; allow both stages rather
	// than treating a slow Node startup as a process-group cleanup failure.
	eventually(t, 2*helperTimeout, func() bool {
		data, err := os.ReadFile(pidPath) // #nosec G304 -- pidPath is rooted in t.TempDir.
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	})
	pidData, err := os.ReadFile(pidPath) // #nosec G304 -- pidPath is rooted in t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(pid): %v", err)
	}
	helperPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("Atoi(pid): %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(helperPID, syscall.SIGKILL) })
	helperGroup, err := syscall.Getpgid(helperPID)
	if err != nil {
		t.Fatalf("Getpgid(helper): %v", err)
	}
	if helperGroup != cmd.Process.Pid {
		t.Fatalf("helper process group = %d, want runner group %d", helperGroup, cmd.Process.Pid)
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill runner process group: %v", err)
	}
	_ = cmd.Wait()
	eventually(t, time.Second, func() bool { return !processExists(helperPID) })
}

func TestPiRPCReviewerExtensionRequiresDiffBeforeHeadTools(t *testing.T) {
	extension := piRPCReviewerExtension("review-tool", "config.json", "/repo", 2048, time.Second, "")
	for _, want := range []string{
		"let diffAttempted = false",
		"if (!diffAttempted)",
		"cr_diff must be invoked before inspecting repository files",
		"diffAttempted = true",
		"execute: headTool(\"cr_read\")",
	} {
		if !strings.Contains(extension, want) {
			t.Fatalf("extension missing diff-ordering enforcement %q:\n%s", want, extension)
		}
	}
}
