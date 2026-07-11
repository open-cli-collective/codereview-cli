package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/agents"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/dossier"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

const dossierFinalExcerptRunes = 240

func buildReviewerPrompt(paths ArtifactPaths, pr gitprovider.PR, selected llm.SelectedAgent, agent agents.Agent, changedFiles []string) (string, []string, error) {
	input, deps, err := reviewerPromptInputFromArtifacts(paths, pr, selected, agent)
	if err != nil {
		return "", nil, err
	}
	assignmentScope := reviewerAssignmentScope(selected, changedFiles)
	payload := map[string]any{
		"task":            "review files and return findings JSON only",
		"output_contract": findingsOutputContract(agent.ID, assignmentScope),
		"agent":           reviewerAgentPromptFromAgent(agent),
		"assignment":      input.Assignment,
		"dossier":         input.Dossier,
		"workbench":       input.Workbench,
		"pr":              input.PR,
		"schema":          "findings",
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return string(body), deps, nil
}

type selectionAgentPrompt struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Category             string   `json:"category,omitempty"`
	FileGlobs            []string `json:"file_globs,omitempty"`
	AppliesWhen          []string `json:"applies_when,omitempty"`
	NeedsFullFileContent bool     `json:"needs_full_file_content"`
}

type selectionPromptDossier struct {
	PRIntent     string `json:"pr_intent"`
	ChangeMap    string `json:"change_map"`
	RepoGuidance string `json:"repo_guidance"`
	Discussion   string `json:"discussion"`
}

type selectionPromptWorkbench struct {
	CheckoutMode string                  `json:"checkout_mode"`
	PR           workbenchPRIdentity     `json:"pr"`
	Base         workbenchBranchArtifact `json:"base"`
	Head         workbenchBranchArtifact `json:"head"`
}

type selectionThreadPrompt struct {
	ThreadID   string `json:"thread_id"`
	Path       string `json:"path"`
	Line       int    `json:"line,omitempty"`
	Side       string `json:"side,omitempty"`
	AnchorKind string `json:"anchor_kind,omitempty"`
	Resolved   bool   `json:"resolved,omitempty"`
	Status     string `json:"status,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

type selectionPromptInput struct {
	ChangedFiles []string                 `json:"changed_files"`
	Dossier      selectionPromptDossier   `json:"dossier"`
	Workbench    selectionPromptWorkbench `json:"workbench"`
	Threads      []selectionThreadPrompt  `json:"threads,omitempty"`
}

type promptPR struct {
	Ref    gitprovider.PRRef       `json:"ref"`
	Title  string                  `json:"title"`
	URL    string                  `json:"url"`
	State  gitprovider.PRState     `json:"state"`
	Author gitprovider.Identity    `json:"author"`
	Head   gitprovider.PRBranchRef `json:"head"`
	Base   gitprovider.PRBranchRef `json:"base"`
}

func promptPRFromPR(pr gitprovider.PR) promptPR {
	return promptPR{
		Ref:    pr.Ref,
		Title:  pr.Title,
		URL:    pr.URL,
		State:  pr.State,
		Author: pr.Author,
		Head:   pr.Head,
		Base:   pr.Base,
	}
}

func selectionAgentPromptFromAgent(agent agents.Agent) selectionAgentPrompt {
	return selectionAgentPrompt{
		ID:                   agent.ID,
		Name:                 agent.Name,
		Category:             agent.Category.Name,
		FileGlobs:            append([]string(nil), agent.FileGlobs...),
		AppliesWhen:          append([]string(nil), agent.AppliesWhen...),
		NeedsFullFileContent: agent.NeedsFullFileContent,
	}
}

func selectionAgentPromptsFromCatalog(catalog agents.Catalog) []selectionAgentPrompt {
	out := make([]selectionAgentPrompt, 0, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		out = append(out, selectionAgentPromptFromAgent(agent))
	}
	return out
}

type reviewerAgentPrompt struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Category    agents.Category `json:"category"`
	Description string          `json:"description,omitempty"`
	Prompt      string          `json:"prompt,omitempty"`
}

func reviewerAgentPromptFromAgent(agent agents.Agent) reviewerAgentPrompt {
	return reviewerAgentPrompt{
		ID:          agent.ID,
		Name:        agent.Name,
		Category:    agent.Category,
		Description: agent.Description,
		Prompt:      agent.Prompt,
	}
}

type reviewerPromptDossier struct {
	PRIntent     string `json:"pr_intent"`
	ChangeMap    string `json:"change_map"`
	RepoGuidance string `json:"repo_guidance"`
	Discussion   string `json:"discussion"`
}

type reviewerPromptWorkbench struct {
	CheckoutMode string                  `json:"checkout_mode"`
	PR           workbenchPRIdentity     `json:"pr"`
	Base         workbenchBranchArtifact `json:"base"`
	Head         workbenchBranchArtifact `json:"head"`
}

type reviewerPromptAssignment struct {
	AgentID      string   `json:"agent_id"`
	Rationale    string   `json:"rationale,omitempty"`
	Files        []string `json:"files"`
	AllowedFiles []string `json:"allowed_files,omitempty"`
}

type reviewerPromptInput struct {
	PR         promptPR                 `json:"pr"`
	Dossier    reviewerPromptDossier    `json:"dossier"`
	Workbench  reviewerPromptWorkbench  `json:"workbench"`
	Assignment reviewerPromptAssignment `json:"assignment"`
}

const defaultSelectionTask = "select reviewer agents from dossier/workbench context; return selection JSON only"

func buildSelectionPrompt(catalog agents.Catalog, input selectionPromptInput, maxAgents int, selectionInstructions string) (string, error) {
	threadIDs := make([]string, 0, len(input.Threads))
	for _, thread := range input.Threads {
		threadIDs = append(threadIDs, thread.ThreadID)
	}
	payload := map[string]any{
		"task":                defaultSelectionTask,
		"output_contract":     selectionOutputContract(catalog.Agents, input.ChangedFiles, threadIDs, maxAgents),
		"schema":              "selection",
		"max_selected_agents": maxAgents,
		"agents":              selectionAgentPromptsFromCatalog(catalog),
		"changed_files":       append([]string(nil), input.ChangedFiles...),
		"dossier":             input.Dossier,
		"workbench":           input.Workbench,
		"threads":             input.Threads,
	}
	if instructions := strings.TrimSpace(selectionInstructions); instructions != "" {
		payload["selection_instructions"] = instructions
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("pipeline: build selection prompt: %w", err)
	}
	return string(body), nil
}

type dossierPromptCore struct {
	PRIntent     string
	ChangeMap    string
	RepoGuidance string
	Discussion   string
	Metadata     workbenchMetadataArtifact
	Dependencies []string
}

func loadDossierPromptCore(paths ArtifactPaths) (dossierPromptCore, error) {
	prIntentPath, err := paths.DossierFinalPath("pr-intent.md")
	if err != nil {
		return dossierPromptCore{}, err
	}
	changeMapPath, err := paths.DossierFinalPath("change-map.md")
	if err != nil {
		return dossierPromptCore{}, err
	}
	repoGuidancePath, err := paths.DossierFinalPath("repo-guidance.md")
	if err != nil {
		return dossierPromptCore{}, err
	}
	discussionPath, err := paths.DossierFinalPath("discussion.md")
	if err != nil {
		return dossierPromptCore{}, err
	}
	prIntent, err := selectionPromptContentFromPath(prIntentPath)
	if err != nil {
		return dossierPromptCore{}, err
	}
	changeMap, err := selectionPromptContentFromPath(changeMapPath)
	if err != nil {
		return dossierPromptCore{}, err
	}
	repoGuidance, err := selectionPromptContentFromPath(repoGuidancePath)
	if err != nil {
		return dossierPromptCore{}, err
	}
	discussion, err := selectionPromptContentFromPath(discussionPath)
	if err != nil {
		return dossierPromptCore{}, err
	}

	indexBytes, err := os.ReadFile(paths.DossierIndexPath()) // #nosec G304 -- artifact path is pipeline-owned under the selected run/workbench root.
	if err != nil {
		return dossierPromptCore{}, fmt.Errorf("pipeline: read dossier artifact %s: %w", filepath.Base(paths.DossierIndexPath()), err)
	}
	metaPath := paths.WorkbenchMetadataPath()
	metaBytes, err := os.ReadFile(metaPath) // #nosec G304 -- artifact path is pipeline-owned under the selected run/workbench root.
	if err != nil {
		return dossierPromptCore{}, fmt.Errorf("pipeline: read workbench metadata: %w", err)
	}
	var meta workbenchMetadataArtifact
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return dossierPromptCore{}, fmt.Errorf("pipeline: decode workbench metadata: %w", err)
	}
	return dossierPromptCore{
		PRIntent:     prIntent,
		ChangeMap:    changeMap,
		RepoGuidance: repoGuidance,
		Discussion:   discussion,
		Metadata:     meta,
		Dependencies: []string{
			"dossier_index=" + sha256Hex(indexBytes),
			"workbench_metadata=" + sha256Hex(metaBytes),
		},
	}, nil
}

func selectionPromptInputFromArtifacts(paths ArtifactPaths, threads []gitprovider.InlineThread) (selectionPromptInput, []string, error) {
	core, err := loadDossierPromptCore(paths)
	if err != nil {
		return selectionPromptInput{}, nil, err
	}
	summary, err := dossier.ReadDiscussionSummary(paths)
	if err != nil {
		return selectionPromptInput{}, nil, err
	}

	input := selectionPromptInput{
		ChangedFiles: append([]string(nil), core.Metadata.FingerprintInputs.ChangedFiles...),
		Dossier: selectionPromptDossier{
			PRIntent:     core.PRIntent,
			ChangeMap:    core.ChangeMap,
			RepoGuidance: core.RepoGuidance,
			Discussion:   core.Discussion,
		},
		Workbench: selectionPromptWorkbench{
			CheckoutMode: core.Metadata.CheckoutMode,
			PR:           core.Metadata.PR,
			Base:         core.Metadata.Base,
			Head:         core.Metadata.Head,
		},
		Threads: selectionThreadPrompts(threads, summary),
	}
	return input, core.Dependencies, nil
}

func selectionPromptInputFromThreadContext(paths ArtifactPaths, threads []threadcontext.Thread) (selectionPromptInput, []string, error) {
	input, deps, err := selectionPromptInputFromArtifacts(paths, nil)
	if err != nil {
		return selectionPromptInput{}, nil, err
	}
	summary, err := dossier.ReadDiscussionSummary(paths)
	if err != nil {
		return selectionPromptInput{}, nil, err
	}
	input.Threads = selectionThreadPromptsFromContext(threads, summary)
	return input, deps, nil
}

func selectionPromptContentFromPath(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- artifact path is pipeline-owned under the selected run/workbench root.
	if err != nil {
		return "", fmt.Errorf("pipeline: read dossier artifact %s: %w", filepath.Base(path), err)
	}
	return string(data), nil
}

func reviewerPromptInputFromArtifacts(paths ArtifactPaths, pr gitprovider.PR, selected llm.SelectedAgent, agent agents.Agent) (reviewerPromptInput, []string, error) {
	core, err := loadDossierPromptCore(paths)
	if err != nil {
		return reviewerPromptInput{}, nil, err
	}
	input := reviewerPromptInput{
		PR: promptPRFromPR(pr),
		Dossier: reviewerPromptDossier{
			PRIntent:     core.PRIntent,
			ChangeMap:    core.ChangeMap,
			RepoGuidance: core.RepoGuidance,
			Discussion:   core.Discussion,
		},
		Workbench: reviewerPromptWorkbench{
			CheckoutMode: core.Metadata.CheckoutMode,
			PR:           core.Metadata.PR,
			Base:         core.Metadata.Base,
			Head:         core.Metadata.Head,
		},
		Assignment: reviewerPromptAssignment{
			AgentID:      agent.ID,
			Rationale:    selected.Rationale,
			Files:        append([]string(nil), selected.Files...),
			AllowedFiles: append([]string(nil), selected.AllowedFiles...),
		},
	}
	return input, core.Dependencies, nil
}

func dossierInlineThreadSummaryIndexes(summary dossier.DiscussionSummary) (map[string]dossier.InlineThreadSummary, map[string]dossier.InlineThreadSummary) {
	byAnchor := make(map[string]dossier.InlineThreadSummary, len(summary.InlineThreads))
	byThreadID := make(map[string]dossier.InlineThreadSummary, len(summary.InlineThreads))
	for _, thread := range summary.InlineThreads {
		if strings.TrimSpace(thread.ThreadID) != "" {
			byThreadID[strings.TrimSpace(thread.ThreadID)] = thread
		} else {
			key := dossierInlineThreadAnchorKey(thread.Path, thread.Side, thread.Line, thread.AnchorKind)
			byAnchor[key] = thread
		}
	}
	return byAnchor, byThreadID
}

func selectionThreadPrompts(threads []gitprovider.InlineThread, summary dossier.DiscussionSummary) []selectionThreadPrompt {
	summaryByAnchor, summaryByThreadID := dossierInlineThreadSummaryIndexes(summary)
	out := make([]selectionThreadPrompt, 0, len(threads))
	for _, thread := range threads {
		promptThread := selectionThreadPrompt{
			ThreadID:   string(thread.ID),
			Path:       thread.Path,
			Line:       thread.Line,
			Side:       string(thread.Side),
			AnchorKind: string(thread.SubjectType),
			Resolved:   thread.Resolved,
		}
		if summarized, ok := summaryByThreadID[string(thread.ID)]; ok {
			promptThread.Status = summarized.Status
			promptThread.Summary = summarized.Summary
		} else if summarized, ok := summaryByAnchor[dossierInlineThreadAnchorKey(thread.Path, string(thread.Side), thread.Line, string(thread.SubjectType))]; ok {
			promptThread.Status = summarized.Status
			promptThread.Summary = summarized.Summary
		} else if len(thread.Comments) > 0 {
			promptThread.Summary = singleLineExcerpt(thread.Comments[0].Body, dossierFinalExcerptRunes)
		}
		out = append(out, promptThread)
	}
	return out
}

func selectionThreadPromptsFromContext(threads []threadcontext.Thread, summary dossier.DiscussionSummary) []selectionThreadPrompt {
	summaryByAnchor, summaryByThreadID := dossierInlineThreadSummaryIndexes(summary)
	out := make([]selectionThreadPrompt, 0, len(threads))
	for _, thread := range threads {
		promptThread := selectionThreadPrompt{
			ThreadID:   string(thread.ID),
			Path:       thread.Anchor.Path,
			Line:       thread.Anchor.Line,
			Side:       string(thread.Anchor.Side),
			AnchorKind: string(thread.Anchor.SubjectType),
			Resolved:   thread.Resolved,
		}
		if settled, ok := thread.EffectiveSettledSummary(); ok {
			promptThread.Status = "settled"
			promptThread.Summary = singleLineExcerpt(settled.Body, dossierFinalExcerptRunes)
		} else if summarized, ok := summaryByThreadID[string(thread.ID)]; ok {
			promptThread.Status = summarized.Status
			promptThread.Summary = summarized.Summary
		} else if summarized, ok := summaryByAnchor[dossierInlineThreadAnchorKey(thread.Anchor.Path, string(thread.Anchor.Side), thread.Anchor.Line, string(thread.Anchor.SubjectType))]; ok {
			promptThread.Status = summarized.Status
			promptThread.Summary = summarized.Summary
		} else {
			promptThread.Status = selectionThreadStatus(thread)
			if len(thread.Comments) > 0 {
				promptThread.Summary = singleLineExcerpt(thread.Comments[0].Body, dossierFinalExcerptRunes)
			}
		}
		out = append(out, promptThread)
	}
	return out
}

func selectionThreadStatus(thread threadcontext.Thread) string {
	switch {
	case thread.Resolved:
		return "settled"
	case thread.Status.PendingHumanReply:
		return "pending_human_reply"
	case thread.Status.CRAuthoredFinding:
		return "cr_authored"
	default:
		return "unresolved"
	}
}

func buildRollupPrompt(pr gitprovider.PR, findings []review.Finding, reviewerFailures []ReviewerFailure, reviewerCoverage []reviewplan.ReviewerCoverageSummary) (string, error) {
	payload := map[string]any{
		"task":              "dedupe findings and return rollup JSON only",
		"output_contract":   rollupOutputContract(findings),
		"schema":            "rollup",
		"pr":                promptPRFromPR(pr),
		"findings":          rollupFindingsPrompt(findings),
		"reviewer_failures": reviewerFailures,
		"reviewer_coverage": rollupCoveragePrompt(reviewerCoverage),
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("pipeline: build rollup prompt: %w", err)
	}
	return string(body), nil
}

type rollupCoveragePromptEntry struct {
	AgentID        string   `json:"agent_id"`
	Status         string   `json:"status"`
	Scope          []string `json:"scope,omitempty"`
	InspectedFiles []string `json:"inspected_files,omitempty"`
	SkippedFiles   []string `json:"skipped_files,omitempty"`
	Constraints    []string `json:"constraints,omitempty"`
	Diagnostic     string   `json:"diagnostic,omitempty"`
}

func rollupCoveragePrompt(coverage []reviewplan.ReviewerCoverageSummary) []rollupCoveragePromptEntry {
	out := make([]rollupCoveragePromptEntry, 0, len(coverage))
	for _, entry := range coverage {
		out = append(out, rollupCoveragePromptEntry{
			AgentID:        entry.AgentID,
			Status:         entry.Status,
			Scope:          append([]string(nil), entry.Scope...),
			InspectedFiles: append([]string(nil), entry.InspectedFiles...),
			SkippedFiles:   append([]string(nil), entry.SkippedFiles...),
			Constraints:    append([]string(nil), entry.Constraints...),
			Diagnostic:     entry.Diagnostic,
		})
	}
	return out
}

type rollupFindingPrompt struct {
	ID       string                      `json:"id"`
	Severity string                      `json:"severity"`
	FilePath string                      `json:"file_path"`
	Location rollupFindingLocationPrompt `json:"location"`
	Body     string                      `json:"body"`
}

type rollupFindingLocationPrompt struct {
	Kind string `json:"kind"`
	Side string `json:"side,omitempty"`
	Line int    `json:"line,omitempty"`
}

func rollupFindingsPrompt(findings []review.Finding) []rollupFindingPrompt {
	out := make([]rollupFindingPrompt, 0, len(findings))
	for _, finding := range findings {
		out = append(out, rollupFindingPrompt{
			ID:       finding.ID.String(),
			Severity: finding.Severity.String(),
			FilePath: finding.FilePath,
			Location: rollupFindingLocationPrompt{
				Kind: finding.Anchor.Kind.String(),
				Side: finding.Anchor.Side.String(),
				Line: finding.Anchor.Line,
			},
			Body: finding.Body,
		})
	}
	return out
}

type agentSourcesArtifact struct {
	Sources []agents.SourceInfo       `json:"sources"`
	Agents  []agentProvenanceArtifact `json:"agents"`
}

type agentProvenanceArtifact struct {
	ID              string                     `json:"id"`
	Provenance      string                     `json:"provenance"`
	Source          agents.SourceInfo          `json:"source"`
	ReviewerRuntime *reviewerRuntimeResolution `json:"reviewer_runtime,omitempty"`
}

type reviewerRuntimeResolution struct {
	Mode           string                `json:"mode"`
	FloorTier      string                `json:"floor_tier,omitempty"`
	BaselineTier   string                `json:"baseline_tier,omitempty"`
	EffectiveTier  string                `json:"effective_tier,omitempty"`
	ResolvedModel  string                `json:"resolved_model"`
	ModelMapSource config.ModelMapSource `json:"model_map_source,omitempty"`
	Fast           bool                  `json:"fast,omitempty"`
}

type outputContract struct {
	Instructions   []string `json:"instructions"`
	ResponseSchema any      `json:"response_schema"`
	AllowedValues  any      `json:"allowed_values,omitempty"`
	Example        any      `json:"example"`
}

func selectionOutputContract(agents []agents.Agent, changedFiles []string, threadIDs []string, maxAgents int) outputContract {
	agentIDs := make([]string, 0, len(agents))
	for _, agent := range agents {
		agentIDs = append(agentIDs, agent.ID)
	}
	example := map[string]any{
		"schema_version":  1,
		"selected_agents": selectionExampleAgents(agentIDs, changedFiles),
		"thread_actions":  []map[string]any{},
		"reasoning":       "Selected the relevant reviewers for the changed files.",
	}
	if len(agentIDs) == 0 {
		example["reasoning"] = "No reviewer agents are available."
	}
	return outputContract{
		Instructions: []string{
			"Return exactly one raw JSON object. Do not wrap it in Markdown fences.",
			"Use only the keys shown in response_schema. Unknown keys are rejected.",
			"allowed_values is context only; do not include allowed_values keys in the response.",
			"schema_version must be 1.",
			"selected_agents[].agent_id must be one of the allowed_agent_ids.",
			"selected_agents[].files must contain only paths from changed_files.",
			"selected_agents[].allowed_files must contain only paths from changed_files when present.",
			"selected_agents must contain at most max_selected_agents entries, ordered from highest to lowest review value.",
			"thread_actions must always be an empty array. Thread lifecycle replies and resolution are handled by the thread_analysis stage.",
		},
		ResponseSchema: map[string]any{
			"schema_version":  "number, required, must be 1",
			"selected_agents": "array of {agent_id: string, rationale: string, files: string[], allowed_files?: string[]}",
			"thread_actions":  "empty array, required",
			"reasoning":       "string",
		},
		AllowedValues: map[string]any{
			"allowed_agent_ids":   agentIDs,
			"changed_files":       changedFiles,
			"known_thread_ids":    threadIDs,
			"max_selected_agents": maxAgents,
		},
		Example: example,
	}
}

func selectionExampleAgents(agentIDs []string, changedFiles []string) []map[string]any {
	if len(agentIDs) == 0 {
		return []map[string]any{}
	}
	return []map[string]any{{
		"agent_id":  agentIDs[0],
		"rationale": "This agent applies to the changed files.",
		"files":     firstNOrPlaceholder(changedFiles, "path/to/changed-file.ext", 1),
	}}
}

func findingsOutputContract(agentID string, changedFiles []string) outputContract {
	return outputContract{
		Instructions: []string{
			"Return exactly one raw JSON object. Do not wrap it in Markdown fences.",
			"Use only the keys shown in response_schema. Unknown keys are rejected.",
			"allowed_values is context only; do not include allowed_values keys in the response.",
			"schema_version must be 1.",
			"agent_id must match the provided agent id.",
			"inspected_files must list assigned changed files you actually inspected, even when findings is empty.",
			"skipped_files must list assigned changed files you intentionally did not inspect or could not inspect.",
			"At least one of inspected_files or skipped_files must be non-empty.",
			"constraints must list any material review constraints, such as intentionally narrow scope, missing context, or tool limitations.",
			"findings must be an empty array when there are no actionable findings.",
			"file_path must be one of changed_files.",
			"Do not provide finding_id; the harness assigns IDs.",
		},
		ResponseSchema: map[string]any{
			"schema_version":  "number, required, must be 1",
			"agent_id":        "string, required",
			"inspected_files": "string[], assigned changed files inspected by this reviewer",
			"skipped_files":   "string[], assigned changed files intentionally not inspected or not inspectable",
			"constraints":     "string[], material scope/tool/context constraints",
			"findings":        "array of {severity: string, file_path: string, anchor: {kind: 'file'} or {kind: 'line', side: 'RIGHT'|'LEFT', line: positive number}, body: string}",
		},
		AllowedValues: map[string]any{
			"severities":    []string{"blocking", "major", "minor", "nits"},
			"changed_files": changedFiles,
		},
		Example: map[string]any{
			"schema_version":  1,
			"agent_id":        agentID,
			"inspected_files": firstNOrPlaceholder(changedFiles, "path/to/changed-file.ext", 1),
			"skipped_files":   []string{},
			"constraints":     []string{},
			"findings": []map[string]any{{
				"severity":  "major",
				"file_path": firstOrPlaceholder(changedFiles, "path/to/changed-file.ext"),
				"anchor": map[string]any{
					"kind": "file",
				},
				"body": "Explain the issue and the concrete impact. Include the suggested fix in the same body.",
			}},
		},
	}
}

func rollupOutputContract(findings []review.Finding) outputContract {
	findingIDs := make([]string, 0, len(findings))
	for _, finding := range findings {
		findingIDs = append(findingIDs, finding.ID.String())
	}
	return outputContract{
		Instructions: []string{
			"Return exactly one raw JSON object. Do not wrap it in Markdown fences.",
			"Use only the keys shown in response_schema. Unknown keys are rejected.",
			"allowed_values is context only; do not include allowed_values keys in the response.",
			"schema_version must be 1.",
			"ordered_findings must contain finding ID strings only and include every kept finding exactly once.",
			"dedupe_log kept and dropped values must contain finding ID strings only, never finding objects.",
			"Use finding location only to distinguish findings during dedupe; do not include finding fields such as severity, file_path, location, body, anchor, or finding_id in the response.",
			"dedupe_log must be an empty array when no findings are duplicates.",
		},
		ResponseSchema: map[string]any{
			"schema_version":         "number, required, must be 1",
			"review_event":           "string: approve, comment, or request_changes",
			"review_event_rationale": "string",
			"dedupe_log":             "array of {kept: finding_id, dropped: finding_id[], reason: string}",
			"ordered_findings":       "array of finding ids after dedupe",
		},
		AllowedValues: map[string]any{
			"available_finding_ids": findingIDs,
		},
		Example: map[string]any{
			"schema_version":         1,
			"review_event":           "comment",
			"review_event_rationale": "Commenting because findings remain for human review.",
			"dedupe_log":             []map[string]any{},
			"ordered_findings":       findingIDs,
		},
	}
}

func firstOrPlaceholder(values []string, placeholder string) string {
	if len(values) > 0 {
		return values[0]
	}
	return placeholder
}

func firstNOrPlaceholder(values []string, placeholder string, count int) []string {
	if len(values) == 0 {
		return []string{placeholder}
	}
	if count > len(values) {
		count = len(values)
	}
	return append([]string(nil), values[:count]...)
}

func dossierInlineThreadAnchorKey(path, side string, line int, anchorKind string) string {
	return fmt.Sprintf("%s|%s|%d|%s", strings.TrimSpace(path), strings.TrimSpace(side), line, strings.TrimSpace(anchorKind))
}

func singleLineExcerpt(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" {
		return "(empty)"
	}
	runes := []rune(value)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return value
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
