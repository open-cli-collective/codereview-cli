package app

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/hooks"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

type pipelineTaskProgress struct {
	logger  *progress.Logger
	command string
	hooks   *hookDispatcher
}

type pipelineTaskSpan struct {
	span  *progress.Span
	hooks *hookDispatcher
	event pipeline.LLMTaskProgressEvent
	run   ledger.Run
}

func newPipelineTaskProgress(logger *progress.Logger, command string, dispatcher *hookDispatcher) pipeline.LLMTaskProgress {
	if dispatcher != nil && !dispatcher.enabled {
		dispatcher = nil
	}
	if logger == nil && dispatcher == nil {
		return nil
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = "review"
	}
	return pipelineTaskProgress{logger: logger, command: command, hooks: dispatcher}
}

func (p pipelineTaskProgress) StartLLMTask(event pipeline.LLMTaskProgressEvent) pipeline.LLMTaskProgressSpan {
	var span *progress.Span
	if p.logger != nil {
		span = p.logger.StartFields(p.command, "run_llm_task", "llm_task", pipelineTaskProgressFields(event)...)
	}
	run := p.observeStart(event)
	return pipelineTaskSpan{
		span: span, hooks: p.hooks, event: event, run: run,
	}
}

func (p pipelineTaskProgress) LoadLLMTask(event pipeline.LLMTaskProgressEvent, result pipeline.LLMTaskProgressResult) {
	run := p.observeStart(event)
	if p.logger != nil {
		span := p.logger.StartFields(p.command, "load_llm_task", "llm_task", pipelineTaskProgressFields(event)...)
		span.EndFields(nil, pipelineTaskProgressResultFields(result)...)
	}
	emitTaskHook(p.hooks, event, nil, result, run)
}

func (s pipelineTaskSpan) End(err error, result pipeline.LLMTaskProgressResult) {
	if s.span != nil {
		s.span.EndFields(err, pipelineTaskProgressResultFields(result)...)
	}
	emitTaskHook(s.hooks, s.event, err, result, s.run)
}

func (p pipelineTaskProgress) observeStart(event pipeline.LLMTaskProgressEvent) ledger.Run {
	if p.hooks == nil {
		return ledger.Run{}
	}
	run := p.hooks.observeArtifactLog(event.LogPath)
	switch event.Phase {
	case "dossier":
		p.hooks.emitOnce(p.hooks.event("workspace.prepared"), hooks.Payload{}, run)
	case "selection":
		p.hooks.observeSelection()
		p.hooks.emitOnce(p.hooks.event("workspace.prepared"), hooks.Payload{}, run)
		p.hooks.emitOnce(p.hooks.event("dossier.ready"), hooks.Payload{}, run)
	}
	return run
}

func emitTaskHook(dispatcher *hookDispatcher, event pipeline.LLMTaskProgressEvent, err error, result pipeline.LLMTaskProgressResult, run ledger.Run) {
	if dispatcher == nil || event.Phase != "reviewer" || dispatcher.command == "respond" {
		return
	}
	status := result.Status
	if status == "" {
		status = "succeeded"
		if err != nil {
			status = "failed"
		}
	}
	dispatcher.emit(dispatcher.event("reviewer.completed"), hooks.Payload{ReviewerID: event.AgentID, ReviewerStatus: status}, run, false)
}

func pipelineTaskProgressFields(event pipeline.LLMTaskProgressEvent) []progress.Field {
	fields := []progress.Field{
		{Key: "task_id", Value: event.TaskID},
		{Key: "phase", Value: event.Phase},
		{Key: "source", Value: event.Source},
	}
	if agentID := strings.TrimSpace(event.AgentID); agentID != "" {
		fields = append(fields, progress.Field{Key: "agent_id", Value: agentID})
	}
	if model := strings.TrimSpace(event.Model); model != "" {
		fields = append(fields, progress.Field{Key: "model", Value: model})
	}
	if effort := strings.TrimSpace(event.Effort); effort != "" {
		fields = append(fields, progress.Field{Key: "effort", Value: effort})
	}
	if logPath := strings.TrimSpace(event.LogPath); logPath != "" {
		fields = append(fields, progress.Field{Key: "log_file", Value: filepath.Base(logPath)})
	}
	if resumeSessionID := strings.TrimSpace(event.ResumeSessionID); resumeSessionID != "" {
		fields = append(fields, progress.Field{Key: "resume_session_id", Value: resumeSessionID})
	}
	return fields
}

func pipelineTaskProgressResultFields(result pipeline.LLMTaskProgressResult) []progress.Field {
	fields := []progress.Field{
		{Key: "cached", Value: strconv.FormatBool(result.Cached)},
		{Key: "task_status", Value: result.Status},
	}
	if sessionID := strings.TrimSpace(result.ProviderSessionID); sessionID != "" {
		fields = append(fields, progress.Field{Key: "session_id", Value: sessionID})
	}
	if result.ValidationAttempts > 0 {
		fields = append(fields, progress.Field{Key: "validation_attempts", Value: strconv.Itoa(result.ValidationAttempts)})
	}
	fields = append(fields, usageFields(result.Usage)...)
	if result.Usage.CostUSD != nil {
		fields = append(fields, progress.Field{Key: "cost_usd", Value: strconv.FormatFloat(*result.Usage.CostUSD, 'f', -1, 64)})
	}
	return fields
}
