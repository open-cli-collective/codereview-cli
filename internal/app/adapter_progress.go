package app

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

type progressAdapter struct {
	adapter  llm.Adapter
	logger   *progress.Logger
	command  string
	provider string
	harness  string
}

type progressStream struct {
	stream        llm.Stream
	span          *progress.Span
	fastRequested bool
}

func withProgressAdapter(logger *progress.Logger, command string, adapter llm.Adapter, provider, harness string) llm.Adapter {
	if adapter == nil || logger == nil {
		return adapter
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = "review"
	}
	return progressAdapter{
		adapter:  adapter,
		logger:   logger,
		command:  command,
		provider: strings.TrimSpace(provider),
		harness:  strings.TrimSpace(harness),
	}
}

func (a progressAdapter) Name() string { return a.adapter.Name() }

func (a progressAdapter) SupportsResume() bool { return a.adapter.SupportsResume() }

func (a progressAdapter) ReviewerWorkspaceMode() llm.ReviewerWorkspaceMode {
	return llm.AdapterReviewerWorkspaceMode(a.adapter)
}

func (a progressAdapter) SupportsCacheAccounting() bool { return a.adapter.SupportsCacheAccounting() }

func (a progressAdapter) SupportsCostReporting() bool { return a.adapter.SupportsCostReporting() }

func (a progressAdapter) Quota(ctx context.Context) (llm.Quota, bool, error) {
	return a.adapter.Quota(ctx)
}

func (a progressAdapter) Start(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return a.start(ctx, "start_llm", req, "")
}

func (a progressAdapter) Resume(ctx context.Context, sessionID string, req llm.Request) (llm.Stream, error) {
	return a.start(ctx, "resume_llm", req, sessionID)
}

func (a progressAdapter) start(ctx context.Context, op string, req llm.Request, resumeSessionID string) (llm.Stream, error) {
	fields := llmProgressFields(a.provider, a.harness, req)
	if req.ReviewerWorkspace != nil {
		fields = append(fields, progress.Field{Key: "reviewer_workspace", Value: string(a.ReviewerWorkspaceMode())})
	}
	span := a.logger.StartFields(a.command, op, "llm", fields...)
	var (
		stream llm.Stream
		err    error
	)
	if strings.TrimSpace(resumeSessionID) != "" {
		stream, err = a.adapter.Resume(ctx, resumeSessionID, req)
	} else {
		stream, err = a.adapter.Start(ctx, req)
	}
	if err != nil {
		_ = span.End(err)
		return nil, err
	}
	return progressStream{stream: stream, span: span, fastRequested: req.Fast}, nil
}

func (s progressStream) SessionID() string {
	return s.stream.SessionID()
}

func (s progressStream) Wait(ctx context.Context) (llm.Response, error) {
	resp, err := s.stream.Wait(ctx)
	fields := usageFields(resp.Usage)
	if s.fastRequested && resp.Usage.Speed != "fast" && resp.Usage.Speed != "standard" {
		fields = append(fields, fastDeliveredField(resp.Usage))
	}
	s.span.EndFields(err, fields...)
	return resp, err
}

func fastDeliveredField(usage llm.Usage) progress.Field {
	delivered := usage.Speed
	if delivered != "fast" && delivered != "standard" {
		delivered = "unknown"
	}
	return progress.Field{Key: "fast_delivered", Value: delivered}
}

func llmProgressFields(provider, harness string, req llm.Request) []progress.Field {
	fields := []progress.Field{}
	if provider = strings.TrimSpace(provider); provider != "" {
		fields = append(fields, progress.Field{Key: "provider", Value: provider})
	}
	if harness = strings.TrimSpace(harness); harness != "" {
		fields = append(fields, progress.Field{Key: "harness", Value: harness})
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		fields = append(fields, progress.Field{Key: "model", Value: model})
	}
	if effort := strings.TrimSpace(req.Effort); effort != "" {
		fields = append(fields, progress.Field{Key: "effort", Value: effort})
	}
	if req.Fast {
		fields = append(fields, progress.Field{Key: "fast", Value: "true"})
	}
	if logPath := strings.TrimSpace(req.LogPath); logPath != "" {
		fields = append(fields, progress.Field{Key: "log_file", Value: filepath.Base(logPath)})
	}
	return fields
}

func usageFields(usage llm.Usage) []progress.Field {
	fields := []progress.Field{}
	if usage.TokensIn != nil {
		fields = append(fields, progress.Field{Key: "tokens_in", Value: strconv.Itoa(*usage.TokensIn)})
	}
	if usage.TokensOut != nil {
		fields = append(fields, progress.Field{Key: "tokens_out", Value: strconv.Itoa(*usage.TokensOut)})
	}
	if usage.CacheRead != nil {
		fields = append(fields, progress.Field{Key: "cache_read", Value: strconv.Itoa(*usage.CacheRead)})
	}
	if usage.CacheCreate != nil {
		fields = append(fields, progress.Field{Key: "cache_create", Value: strconv.Itoa(*usage.CacheCreate)})
	}
	if usage.Speed == "fast" || usage.Speed == "standard" {
		fields = append(fields, fastDeliveredField(usage))
	}
	return fields
}
