package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
)

type promptBudgetSection struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

type promptBudgetError struct {
	phase         string
	totalBytes    int
	limitBytes    int
	sections      []promptBudgetSection
	envelopeBytes int
	unparsedBytes int
}

func (e *promptBudgetError) Error() string {
	parts := make([]string, 0, len(e.sections)+2)
	for _, section := range e.sections {
		parts = append(parts, fmt.Sprintf("%s=%d", section.Name, section.Bytes))
	}
	if e.unparsedBytes > 0 {
		parts = append(parts, fmt.Sprintf("unparsed_bytes=%d", e.unparsedBytes))
	}
	parts = append(parts, fmt.Sprintf("envelope=%d", e.envelopeBytes))
	return fmt.Sprintf("pipeline: context budget exceeded for %s: %d bytes > %d (sections: %s)", e.phase, e.totalBytes, e.limitBytes, strings.Join(parts, ", "))
}

func (opts Options) checkPromptBudget(phase, prompt string) error {
	limit := opts.Budget.MaxPromptBytes
	if limit == 0 {
		limit = defaultMaxPromptBytes
	}
	if limit < 0 || len(prompt) <= limit {
		return nil
	}
	return promptBudgetDetails(phase, prompt, limit)
}

func promptBudgetDetails(phase, prompt string, limit int) *promptBudgetError {
	total := len(prompt)
	out := &promptBudgetError{phase: phase, totalBytes: total, limitBytes: limit}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(prompt), &raw); err != nil || raw == nil {
		out.unparsedBytes = total
		return out
	}
	allowed := map[string]bool{
		"task": true, "output_contract": true, "schema": true, "max_selected_agents": true,
		"agents": true, "file_manifest": true, "dossier": true, "workbench": true,
		"threads": true, "selection_instructions": true, "pr": true, "findings": true,
		"reviewer_failures": true, "reviewer_coverage": true, "agent": true,
		"assignment": true, "discussion_outcomes": true, "coverage_repair": true,
	}
	sizes := map[string]int{}
	for key, value := range raw {
		name := key
		if !allowed[key] {
			name = "other"
		}
		sizes[name] += len(value)
	}
	names := make([]string, 0, len(sizes))
	for name := range sizes {
		names = append(names, name)
	}
	sort.Strings(names)
	sectionBytes := 0
	for _, name := range names {
		bytes := sizes[name]
		out.sections = append(out.sections, promptBudgetSection{Name: name, Bytes: bytes})
		sectionBytes += bytes
	}
	out.envelopeBytes = total - sectionBytes
	return out
}

type promptBudgetReceipt struct {
	SchemaVersion int                   `json:"schema_version"`
	Kind          string                `json:"kind"`
	Phase         string                `json:"phase"`
	TotalBytes    int                   `json:"total_bytes"`
	LimitBytes    int                   `json:"limit_bytes"`
	Sections      []promptBudgetSection `json:"sections"`
	EnvelopeBytes int                   `json:"envelope_bytes"`
	UnparsedBytes int                   `json:"unparsed_bytes,omitempty"`
}

func (opts Options) checkAndPersistPromptBudget(paths ArtifactPaths, taskID, phase, prompt string) error {
	err := opts.checkPromptBudget(phase, prompt)
	if err == nil {
		return nil
	}
	var budgetErr *promptBudgetError
	if !errors.As(err, &budgetErr) {
		return err
	}
	root := filepath.Join(paths.Dir, "prompt-budget")
	if mkdirErr := os.MkdirAll(root, 0o700); mkdirErr != nil {
		return fmt.Errorf("%w (diagnostic persistence failed)", budgetErr)
	}
	// #nosec G302 -- directory requires owner-only traversal.
	if chmodErr := os.Chmod(root, 0o700); chmodErr != nil {
		return fmt.Errorf("%w (diagnostic persistence failed)", budgetErr)
	}
	hash := sha256.Sum256([]byte(taskID))
	receipt := promptBudgetReceipt{
		SchemaVersion: 1,
		Kind:          "prompt_budget_rejection",
		Phase:         budgetErr.phase,
		TotalBytes:    budgetErr.totalBytes,
		LimitBytes:    budgetErr.limitBytes,
		Sections:      append([]promptBudgetSection(nil), budgetErr.sections...),
		EnvelopeBytes: budgetErr.envelopeBytes,
		UnparsedBytes: budgetErr.unparsedBytes,
	}
	data, marshalErr := json.Marshal(receipt)
	if marshalErr != nil {
		return fmt.Errorf("%w (diagnostic persistence failed)", budgetErr)
	}
	path := filepath.Join(root, hex.EncodeToString(hash[:])+".json")
	if writeErr := fsatomic.WriteFileAtomic(path, append(data, '\n'), 0o600); writeErr != nil {
		return fmt.Errorf("%w (diagnostic persistence failed)", budgetErr)
	}
	return budgetErr
}
