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

func TestPiRPCReviewerExtensionRejectsInvalidImageTransport(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	dir := t.TempDir()
	helperPath := filepath.Join(dir, "helper.mjs")
	helper := "#!/usr/bin/env node\nlet input=''; for await (const chunk of process.stdin) input+=chunk; const request=JSON.parse(input); process.stdout.write(request.tool==='cr_diff'?'diff':'{\"content\":[');"
	if err := os.WriteFile(helperPath, []byte(helper), 0o700); err != nil {
		t.Fatal(err)
	} // #nosec G306,G703 -- executable fixture under t.TempDir.
	extensionPath := filepath.Join(dir, "extension.mjs")
	if err := os.WriteFile(extensionPath, []byte(extensionWithoutPiSDK(piRPCReviewerExtension(helperPath, "config.json", dir, 4096, time.Second, ""))), 0o600); err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(dir, "runner.mjs")
	runner := `import extension from ` + strconv.Quote(extensionPath) + `;
const tools={}; extension({registerTool(tool){tools[tool.name]=tool;}});
const read=(params)=>tools.cr_read.execute("read",params,new AbortController().signal);
const denied=await read({path:"image.png",view:"image"});
await tools.cr_diff.execute("diff",{},new AbortController().signal);
const malformed=await read({path:"image.png",view:"image"});
const text=await read({path:"text.json"});
if(!denied.isError || !malformed.isError || text.isError || text.content.length!==1 || text.content[0].type!=="text") throw new Error(JSON.stringify({denied,malformed,text}));
`
	if err := os.WriteFile(runnerPath, []byte(runner), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nodePath, runnerPath) // #nosec G204 -- discovered Node and test-owned script.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
}

func TestPiRPCReviewerExtensionForwardsBinaryViewsAfterDiff(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	dir := t.TempDir()
	helperPath := filepath.Join(dir, "helper.mjs")
	helper := "#!/usr/bin/env node\nlet input=''; for await (const chunk of process.stdin) input+=chunk; process.stdout.write(input);"
	if err := os.WriteFile(helperPath, []byte(helper), 0o700); err != nil {
		t.Fatal(err)
	} // #nosec G306,G703 -- executable fixture under t.TempDir.
	extensionPath := filepath.Join(dir, "extension.mjs")
	if err := os.WriteFile(extensionPath, []byte(extensionWithoutPiSDK(piRPCReviewerExtension(helperPath, "config.json", dir, 2048, 5*time.Second, ""))), 0o600); err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(dir, "runner.mjs")
	runner := `import extension from ` + strconv.Quote(extensionPath) + `;
const tools={}; extension({registerTool(tool){tools[tool.name]=tool;}});
const read=(params)=>tools.cr_read.execute("read",params,new AbortController().signal);
for(const view of ["binary","binary-hex"]){
  if(!tools.cr_read.parameters.properties.view.enum.includes(view)) throw new Error("missing schema view");
  const denied=await read({path:"lib.gz",view});
  if(!denied.isError || !denied.content[0].text.includes("cr_diff")) throw new Error("diff-first bypass");
}
await tools.cr_diff.execute("diff",{},new AbortController().signal);
for(const view of ["binary","binary-hex"]){
  const response=await read({path:"lib.gz",view,offset:25,limit:100});
  if(response.isError || response.content.length!==1 || response.content[0].type!=="text") throw new Error(JSON.stringify(response));
  const request=JSON.parse(response.content[0].text);
  if(request.tool!=="cr_read" || request.view!==view || request.path!=="lib.gz" || request.offset!==25 || request.limit!==100) throw new Error(JSON.stringify(request));
}`
	if err := os.WriteFile(runnerPath, []byte(runner), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nodePath, runnerPath) // #nosec G204 -- discovered Node and test-owned script.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
}

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
	extension := extensionWithoutPiSDK(piRPCReviewerExtension(helperPath, filepath.Join(tempDir, "config.json"), tempDir, 2048, helperTimeout, ""))
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

// Plain Node has no Pi extension alias resolver. These transport-only tests
// must never reach the reader; installed-Pi tests exercise the real SDK.
func extensionWithoutPiSDK(extension string) string {
	return strings.Replace(extension, `import { createReadToolDefinition } from "@earendil-works/pi-coding-agent";`, `const createReadToolDefinition = () => { throw new Error("unexpected image decode in transport-only test"); };`, 1)
}

func TestPiRPCReviewerImageProcessingBounds(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	for _, test := range []struct{ name, reader, want string }{
		{"output", `const createReadToolDefinition = () => ({execute:async()=>({content:[{type:"image",mimeType:"image/png",data:"x".repeat(4096)}]})});`, "output budget"},
		{"deadline", `const createReadToolDefinition = () => ({execute:(_id,_params,signal)=>new Promise((_resolve,reject)=>signal.addEventListener("abort",()=>reject(new Error("aborted")),{once:true}))});`, "timed out"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			helperPath := filepath.Join(dir, "helper.mjs")
			helper := `#!/usr/bin/env node
let input=""; for await (const c of process.stdin) input+=c;
const request=JSON.parse(input);
process.stdout.write(request.tool==="cr_diff"?"diff":JSON.stringify({data:"AQ==",mimeType:"image/png",width:1,height:1,source_sha256:"a".repeat(64)}));
`
			if err := os.WriteFile(helperPath, []byte(helper), 0o700); err != nil {
				t.Fatal(err)
			} // #nosec G306,G703 -- executable fixture under t.TempDir.
			extension := piRPCReviewerExtension(helperPath, "config.json", dir, 1024, 500*time.Millisecond, "")
			extension = strings.Replace(extension, `import { createReadToolDefinition } from "@earendil-works/pi-coding-agent";`, test.reader, 1)
			extensionPath := filepath.Join(dir, "extension.mjs")
			if err := os.WriteFile(extensionPath, []byte(extension), 0o600); err != nil {
				t.Fatal(err)
			}
			runner := `import extension from ` + strconv.Quote(extensionPath) + `;
const tools={}; extension({registerTool(t){tools[t.name]=t;}});
await tools.cr_diff.execute("diff",{},new AbortController().signal);
const result=await tools.cr_read.execute("image",{path:"image",view:"image"},new AbortController().signal);
if(!result.isError || !result.content[0].text.includes(` + strconv.Quote(test.want) + `) || Buffer.byteLength(JSON.stringify(result))>1024) throw new Error(JSON.stringify(result));
`
			runnerPath := filepath.Join(dir, "runner.mjs")
			if err := os.WriteFile(runnerPath, []byte(runner), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(nodePath, runnerPath) // #nosec G204 -- discovered Node and test-owned script.
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("node: %v: %s", err, out)
			}
		})
	}
}
