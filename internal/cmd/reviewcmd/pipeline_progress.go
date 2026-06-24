package reviewcmd

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

type pipelineTaskProgress struct {
	logger *progress.Logger
}

type pipelineTaskSpan struct {
	span *progress.Span
}

func newPipelineTaskProgress(logger *progress.Logger) pipeline.LLMTaskProgress {
	if logger == nil {
		return nil
	}
	return pipelineTaskProgress{logger: logger}
}

func (p pipelineTaskProgress) StartLLMTask(event pipeline.LLMTaskProgressEvent) pipeline.LLMTaskProgressSpan {
	return pipelineTaskSpan{
		span: p.logger.StartFields("review", "run_llm_task", "llm_task", pipelineTaskProgressFields(event)...),
	}
}

func (p pipelineTaskProgress) LoadLLMTask(event pipeline.LLMTaskProgressEvent, result pipeline.LLMTaskProgressResult) {
	span := p.logger.StartFields("review", "load_llm_task", "llm_task", pipelineTaskProgressFields(event)...)
	span.EndFields(nil, pipelineTaskProgressResultFields(result)...)
}

func (s pipelineTaskSpan) End(err error, result pipeline.LLMTaskProgressResult) {
	if s.span == nil {
		return
	}
	s.span.EndFields(err, pipelineTaskProgressResultFields(result)...)
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
	return fields
}
