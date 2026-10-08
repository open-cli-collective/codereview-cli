package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPromptBudgetRejectionReceiptIsSafePrivateAndReconciles(t *testing.T) {
	const taskID = "task-id-must-not-be-persisted"
	const sentinel = "PRIVATE_FILE_PATH_MUST_NOT_LEAK"
	prompt := `{"task":"select","file_manifest":["` + sentinel + `"],"private-top-level-key":"` + sentinel + `"}`
	root := t.TempDir()
	err := (Options{Budget: ContextBudget{MaxPromptBytes: 32}}).checkAndPersistPromptBudget(ArtifactPaths{Dir: root}, taskID, "selection", prompt)
	if err == nil {
		t.Fatal("checkAndPersistPromptBudget error = nil, want rejection")
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), taskID) || strings.Contains(err.Error(), "private-top-level-key") {
		t.Fatalf("budget error leaked prompt content or unknown key: %v", err)
	}
	var budgetErr *promptBudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("budget error type = %T, want private typed error", err)
	}
	if budgetErr.totalBytes != len(prompt) || budgetErr.limitBytes != 32 || budgetErr.phase != "selection" {
		t.Fatalf("budget error = %#v, want phase, byte total, and limit", budgetErr)
	}
	hash := sha256.Sum256([]byte(taskID))
	dir := filepath.Join(root, "prompt-budget")
	path := filepath.Join(dir, hex.EncodeToString(hash[:])+".json")
	if got := fileMode(t, dir); got != 0o700 {
		t.Fatalf("receipt directory mode = %#o, want 0700", got)
	}
	if got := fileMode(t, path); got != 0o600 {
		t.Fatalf("receipt file mode = %#o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if strings.Contains(string(data), sentinel) || strings.Contains(string(data), taskID) || strings.Contains(string(data), "private-top-level-key") {
		t.Fatalf("receipt leaked prompt content or task metadata: %s", data)
	}
	var receipt promptBudgetReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if receipt.SchemaVersion != 1 || receipt.Kind != "prompt_budget_rejection" || receipt.Phase != "selection" || receipt.TotalBytes != len(prompt) || receipt.LimitBytes != 32 {
		t.Fatalf("receipt = %#v, want safe rejection details", receipt)
	}
	var sectionBytes int
	var sectionNames []string
	for _, section := range receipt.Sections {
		sectionBytes += section.Bytes
		sectionNames = append(sectionNames, section.Name)
	}
	if !reflect.DeepEqual(sectionNames, []string{"file_manifest", "other", "task"}) {
		t.Fatalf("receipt section names = %#v, want sorted fixed names with unknown keys aggregated", sectionNames)
	}
	if sectionBytes+receipt.EnvelopeBytes != receipt.TotalBytes {
		t.Fatalf("receipt bytes do not reconcile: sections=%d envelope=%d total=%d", sectionBytes, receipt.EnvelopeBytes, receipt.TotalBytes)
	}
}

func TestPromptBudgetInvalidJSONUsesUnparsedBytesOnce(t *testing.T) {
	const prompt = `{"private_path":"DO_NOT_LEAK"`
	err := (Options{Budget: ContextBudget{MaxPromptBytes: 1}}).checkAndPersistPromptBudget(ArtifactPaths{Dir: t.TempDir()}, "task", "reviewer", prompt)
	if err == nil {
		t.Fatal("checkAndPersistPromptBudget error = nil, want rejection")
	}
	if strings.Contains(err.Error(), "DO_NOT_LEAK") || strings.Contains(err.Error(), "private_path") {
		t.Fatalf("invalid-JSON budget error leaked content: %v", err)
	}
	var budgetErr *promptBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.unparsedBytes != len(prompt) || budgetErr.envelopeBytes != 0 || len(budgetErr.sections) != 0 {
		t.Fatalf("invalid JSON accounting = %#v, want unparsed bytes counted once", budgetErr)
	}
	if !strings.Contains(err.Error(), "unparsed_bytes=") {
		t.Fatalf("invalid JSON error missing unparsed_bytes: %v", err)
	}
}

func TestPromptBudgetReceiptPersistenceFailureRemainsSafeRejection(t *testing.T) {
	obstacle := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(obstacle, []byte("x"), 0o600); err != nil {
		t.Fatalf("create receipt obstacle: %v", err)
	}
	const sentinel = "DO_NOT_LEAK"
	err := (Options{Budget: ContextBudget{MaxPromptBytes: 1}}).checkAndPersistPromptBudget(ArtifactPaths{Dir: obstacle}, "task", "reviewer", `{"body":"`+sentinel+`"}`)
	if err == nil || !strings.Contains(err.Error(), "diagnostic persistence failed") {
		t.Fatalf("persistence failure = %v, want generic persistence-failed budget rejection", err)
	}
	if strings.Contains(err.Error(), obstacle) || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("persistence failure leaked path or prompt content: %v", err)
	}
	var budgetErr *promptBudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("persistence failure type = %T, want wrapped budget rejection", err)
	}
}

func TestPromptBudgetZeroAndNegativeLimitSemantics(t *testing.T) {
	prompt := strings.Repeat("x", defaultMaxPromptBytes+1)
	if err := (Options{}).checkPromptBudget("selection", prompt); err == nil {
		t.Fatal("zero-limit check error = nil, want default 512 KiB rejection")
	}
	if err := (Options{Budget: ContextBudget{MaxPromptBytes: -1}}).checkPromptBudget("selection", prompt); err != nil {
		t.Fatalf("negative-limit check error = %v, want disabled check", err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func readPromptBudgetReceiptForTest(t *testing.T, root, taskID string) (promptBudgetReceipt, []byte) {
	t.Helper()
	hash := sha256.Sum256([]byte(taskID))
	dir := filepath.Join(root, "prompt-budget")
	path := filepath.Join(dir, hex.EncodeToString(hash[:])+".json")
	if got := fileMode(t, dir); got != 0o700 {
		t.Fatalf("receipt directory mode = %#o, want 0700", got)
	}
	if got := fileMode(t, path); got != 0o600 {
		t.Fatalf("receipt file mode = %#o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read budget receipt: %v", err)
	}
	var receipt promptBudgetReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatalf("decode budget receipt: %v", err)
	}
	var sectionBytes int
	for _, section := range receipt.Sections {
		sectionBytes += section.Bytes
	}
	if receipt.UnparsedBytes > 0 {
		sectionBytes += receipt.UnparsedBytes
	}
	if sectionBytes+receipt.EnvelopeBytes != receipt.TotalBytes {
		t.Fatalf("budget receipt bytes do not reconcile: sections=%d envelope=%d total=%d", sectionBytes, receipt.EnvelopeBytes, receipt.TotalBytes)
	}
	return receipt, data
}
