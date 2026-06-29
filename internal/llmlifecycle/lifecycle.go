// Package llmlifecycle owns durable structured LLM task execution.
package llmlifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

// SchemaVersion identifies the on-disk LLM task artifact schema.
const SchemaVersion = 1

// Status is the durable outcome recorded for one structured LLM task.
type Status string

const (
	// StatusSucceeded marks a task with validated reusable output.
	StatusSucceeded Status = "succeeded"
	// StatusFailedIsolated marks a recoverable task-local failure.
	StatusFailedIsolated Status = "failed_isolated"
	// StatusFailedBlocking marks a failure that must be retried before progress.
	StatusFailedBlocking Status = "failed_blocking"
)

// Paths identifies the artifact root for durable LLM tasks.
type Paths struct {
	LLMTasksDir string
}

// TaskDir returns the encoded artifact directory for a task.
func (p Paths) TaskDir(taskID string) (string, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "", fmt.Errorf("llmlifecycle: task ID is required")
	}
	if strings.TrimSpace(p.LLMTasksDir) == "" {
		return "", fmt.Errorf("llmlifecycle: task directory is required")
	}
	return filepath.Join(p.LLMTasksDir, encodePathComponent(taskID)), nil
}

// Metadata returns the metadata path for a task.
func (p Paths) Metadata(taskID string) (string, error) {
	dir, err := p.TaskDir(taskID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "metadata.json"), nil
}

// ValidatedOutput returns the accepted structured-output payload path.
func (p Paths) ValidatedOutput(taskID string) (string, error) {
	dir, err := p.TaskDir(taskID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "validated-output.json"), nil
}

// RawAttempt returns the raw structured-output attempt payload path.
func (p Paths) RawAttempt(taskID, label string) (string, error) {
	dir, err := p.TaskDir(taskID)
	if err != nil {
		return "", err
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return "", fmt.Errorf("llmlifecycle: attempt label is required")
	}
	return filepath.Join(dir, encodePathComponent(label)+".json"), nil
}

// Store is the durable session store required by run-owned LLM tasks.
type Store interface {
	InsertSession(context.Context, ledger.Session) error
	GetSession(context.Context, string) (ledger.Session, error)
}

// Progress observes LLM task execution and cache-load events.
type Progress interface {
	StartLLMTask(ProgressEvent) ProgressSpan
	LoadLLMTask(ProgressEvent, ProgressResult)
}

// ProgressSpan is an in-flight LLM task progress event.
type ProgressSpan interface {
	End(error, ProgressResult)
}

// ProgressEvent describes the task being executed or loaded.
type ProgressEvent struct {
	TaskID          string
	Phase           string
	AgentID         string
	Model           string
	Effort          string
	LogPath         string
	ResumeSessionID string
	Source          string
}

// ProgressResult records the observable result for a task progress event.
type ProgressResult struct {
	ProviderSessionID  string
	Status             string
	ValidationAttempts int
	Cached             bool
	Usage              llm.Usage
}

// Request contains all dependencies and inputs for one structured LLM task.
type Request struct {
	Store             Store
	Adapter           llm.Adapter
	RunID             string
	TaskID            string
	Phase             string
	DependencyTaskIDs []string
	AllowNoRunCache   bool
	InputFingerprint  string
	Paths             Paths
	Role              ledger.SessionRole
	AgentID           *string
	Model             string
	Effort            string
	LogPath           string
	Prompt            string
	BaseRequest       llm.Request
	ResumeSessionID   string
	FailureStatus     Status
	Progress          Progress
	Now               func() time.Time
	NewSessionRowID   func() string
}

// Result contains either a decoded task value or durable failure/session state.
type Result[T any] struct {
	Value   T
	Draft   SessionDraft
	Session ledger.Session
	Cached  bool
}

// SessionDraft captures provider telemetry before it is persisted to the ledger.
type SessionDraft struct {
	RowID                     string
	ProviderReportedSessionID string
	ProviderSessionID         string
	Role                      ledger.SessionRole
	AgentID                   *string
	Adapter                   string
	Model                     string
	Effort                    string
	StartedAt                 time.Time
	CompletedAt               time.Time
	Response                  llm.Response
}

// ToLedger converts a completed draft into a ledger session for a run.
func (d SessionDraft) ToLedger(runID string) ledger.Session {
	completed := d.CompletedAt
	effort := strings.TrimSpace(d.Effort)
	session := ledger.Session{
		SessionRowID:      d.RowID,
		RunID:             runID,
		ProviderSessionID: d.ProviderSessionID,
		Role:              d.Role,
		AgentID:           d.AgentID,
		Adapter:           d.Adapter,
		Model:             d.Model,
		StartedAt:         d.StartedAt,
		CompletedAt:       &completed,
		DurationMS:        &d.Response.DurationMS,
		TokensIn:          intPtrToInt64(d.Response.Usage.TokensIn),
		TokensOut:         intPtrToInt64(d.Response.Usage.TokensOut),
		CacheRead:         intPtrToInt64(d.Response.Usage.CacheRead),
		CacheCreate:       intPtrToInt64(d.Response.Usage.CacheCreate),
		CostUSD:           d.Response.Usage.CostUSD,
	}
	if effort != "" {
		session.Effort = &effort
	}
	return session
}

// Metadata is the durable descriptor for one structured LLM task.
type Metadata struct {
	SchemaVersion       int               `json:"schema_version"`
	TaskID              string            `json:"task_id"`
	Phase               string            `json:"phase"`
	DependencyTaskIDs   []string          `json:"dependency_task_ids,omitempty"`
	InputFingerprint    string            `json:"input_fingerprint"`
	AgentID             string            `json:"agent_id,omitempty"`
	Status              Status            `json:"status"`
	SessionRowID        string            `json:"session_row_id,omitempty"`
	ProviderSessionID   string            `json:"provider_session_id,omitempty"`
	Adapter             string            `json:"adapter"`
	Model               string            `json:"model"`
	Effort              string            `json:"effort,omitempty"`
	LogPath             string            `json:"log_path,omitempty"`
	ValidatedOutputPath string            `json:"validated_output_path,omitempty"`
	Error               string            `json:"error,omitempty"`
	TokensIn            *int              `json:"tokens_in,omitempty"`
	TokensOut           *int              `json:"tokens_out,omitempty"`
	CacheRead           *int              `json:"cache_read,omitempty"`
	CacheCreate         *int              `json:"cache_create,omitempty"`
	CostUSD             *float64          `json:"cost_usd,omitempty"`
	Attempts            []AttemptMetadata `json:"attempts,omitempty"`
}

// AttemptMetadata records one invalid structured-output attempt.
type AttemptMetadata struct {
	Attempt           string `json:"attempt"`
	ProviderSessionID string `json:"provider_session_id,omitempty"`
	RawOutputPath     string `json:"raw_output_path,omitempty"`
	DecodeError       string `json:"decode_error,omitempty"`
}

// TaskError wraps an LLM task error with its durable status.
type TaskError struct {
	status Status
	err    error
}

// Error returns the underlying task error message.
func (e *TaskError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

// Unwrap returns the underlying task error.
func (e *TaskError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// Status returns the durable task status associated with the error.
func (e *TaskError) Status() Status {
	if e == nil {
		return ""
	}
	return e.status
}

// IsTaskStatus reports whether err is a TaskError with the requested status.
func IsTaskStatus(err error, status Status) bool {
	var taskErr *TaskError
	return errors.As(err, &taskErr) && taskErr.Status() == status
}

// RunStructured executes or loads one durable structured LLM task.
func RunStructured[T any](ctx context.Context, req Request, decode llm.Decoder[T]) (Result[T], error) {
	var zero Result[T]
	if err := validateRequest(req, decode); err != nil {
		return zero, err
	}
	if strings.TrimSpace(req.RunID) == "" && !req.AllowNoRunCache {
		return zero, fmt.Errorf("llmlifecycle: structured task %q requires a persisted run", req.TaskID)
	}
	if loaded, ok, err := LoadStructured(ctx, req, decode); err != nil || ok {
		return loaded, err
	}

	resumeSessionID := req.ResumeSessionID
	if meta, ok, err := ReadMetadata(req.Paths, req.TaskID); err != nil {
		return zero, err
	} else if ok && meta.Status == StatusFailedBlocking {
		if taskSessionID := ResumeSessionID(meta); strings.TrimSpace(taskSessionID) != "" {
			resumeSessionID = taskSessionID
		}
	}
	progressEvent := NewProgressEvent(req, resumeSessionID)
	progressSpan := startProgress(req.Progress, progressEvent)

	now := req.Now
	started := now()
	request := req.BaseRequest
	request.Model = req.Model
	request.Effort = req.Effort
	request.Prompt = req.Prompt
	request.LogPath = req.LogPath
	structured, runErr := llm.RunStructuredWithSessionResume(ctx, req.Adapter, resumeSessionID, request, decode)
	completed := now()
	draft := SessionDraft{
		RowID:                     req.NewSessionRowID(),
		ProviderReportedSessionID: structured.SessionID,
		ProviderSessionID:         structured.SessionID,
		Role:                      req.Role,
		AgentID:                   req.AgentID,
		Adapter:                   req.Adapter.Name(),
		Model:                     req.Model,
		Effort:                    req.Effort,
		StartedAt:                 started,
		CompletedAt:               completed,
		Response:                  structured.Response,
	}
	if strings.TrimSpace(draft.ProviderSessionID) == "" && runErr == nil {
		draft.ProviderSessionID = draft.RowID
	}
	if strings.TrimSpace(draft.Model) == "" {
		draft.Model = "default"
	}
	role := req.Role
	if strings.TrimSpace(role.String()) == "" {
		role = ledger.SessionRoleOrchestrator
	}
	draft.Role = role

	hasRun := strings.TrimSpace(req.RunID) != ""
	meta := BaseMetadata(req, draft)
	if !hasRun {
		meta.SessionRowID = ""
	}
	var session ledger.Session
	if hasRun && strings.TrimSpace(draft.ProviderSessionID) != "" {
		if req.Store == nil {
			err := fmt.Errorf("llmlifecycle: store is required")
			endProgress(progressSpan, err, progressResult(meta, structured, false))
			return zero, err
		}
		session = draft.ToLedger(req.RunID)
		if err := req.Store.InsertSession(ctx, session); err != nil {
			meta.Status = StatusFailedBlocking
			meta.ProviderSessionID = draft.ProviderSessionID
			endProgress(progressSpan, err, progressResult(meta, structured, false))
			return zero, err
		}
	}

	if runErr == nil {
		meta.Status = StatusSucceeded
		meta.SessionRowID = session.SessionRowID
		meta.ProviderSessionID = draft.ProviderSessionID
		if err := WriteSuccess(req.Paths, &meta, structured.AcceptedOutput); err != nil {
			endProgress(progressSpan, err, progressResult(meta, structured, false))
			return zero, err
		}
		endProgress(progressSpan, nil, progressResult(meta, structured, false))
		return Result[T]{Value: structured.Value, Draft: draft, Session: session}, nil
	}

	meta.Error = SanitizeError(runErr)
	meta.Status = failureStatus(ctx, req, runErr, structured.SessionID, len(structured.ValidationAttempts) > 0)
	meta.SessionRowID = session.SessionRowID
	meta.ProviderSessionID = draft.ProviderSessionID
	if err := WriteFailure(req.Paths, &meta, structured.ValidationAttempts); err != nil {
		endProgress(progressSpan, err, progressResult(meta, structured, false))
		return Result[T]{Draft: draft, Session: session}, err
	}
	endProgress(progressSpan, runErr, progressResult(meta, structured, false))
	return Result[T]{Draft: draft, Session: session}, &TaskError{status: meta.Status, err: runErr}
}

// LoadStructured loads a reusable task result without calling the provider.
func LoadStructured[T any](ctx context.Context, req Request, decode llm.Decoder[T]) (Result[T], bool, error) {
	var zero Result[T]
	meta, ok, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil || !ok {
		return zero, ok, err
	}
	if err := ValidateMetadata(meta, req, adapterName(req.Adapter)); err != nil {
		return zero, true, err
	}
	switch meta.Status {
	case StatusSucceeded:
		outputPath, err := req.Paths.ValidatedOutput(req.TaskID)
		if err != nil {
			return zero, true, err
		}
		if strings.TrimSpace(meta.ValidatedOutputPath) != "" {
			outputPath, err = ValidatePayloadPath(req.Paths, req.TaskID, "validated_output_path", meta.ValidatedOutputPath)
			if err != nil {
				return zero, true, err
			}
		}
		data, err := os.ReadFile(outputPath) // #nosec G304 -- validated task output path is scoped to task artifacts.
		if err != nil {
			return zero, true, fmt.Errorf("llmlifecycle: read task %q output: %w", req.TaskID, err)
		}
		value, err := decode(data)
		if err != nil {
			return zero, true, fmt.Errorf("llmlifecycle: decode stored task %q output: %w", req.TaskID, err)
		}
		if strings.TrimSpace(req.RunID) == "" {
			draft := SessionDraftFromMetadata(meta)
			progress := progressResult(meta, llm.StructuredResult[T]{SessionID: meta.ProviderSessionID}, true)
			progress.Usage = draft.Response.Usage
			loadProgress(req.Progress, NewProgressEvent(req, ResumeSessionID(meta)), progress)
			return Result[T]{Value: value, Draft: draft, Cached: true}, true, nil
		}
		if req.Store == nil {
			return zero, true, fmt.Errorf("llmlifecycle: store is required")
		}
		session, err := loadTaskSession(ctx, req.Store, req.RunID, meta)
		if err != nil {
			return zero, true, err
		}
		draft := SessionDraftFromLedger(session)
		progress := progressResult(meta, llm.StructuredResult[T]{SessionID: meta.ProviderSessionID}, true)
		progress.Usage = draft.Response.Usage
		loadProgress(req.Progress, NewProgressEvent(req, ResumeSessionID(meta)), progress)
		return Result[T]{Value: value, Draft: draft, Session: session, Cached: true}, true, nil
	case StatusFailedIsolated:
		if req.FailureStatus != StatusFailedIsolated {
			return zero, true, fmt.Errorf("llmlifecycle: task %q has isolated failure status outside isolated phase", req.TaskID)
		}
		if strings.TrimSpace(req.RunID) == "" {
			draft := SessionDraftFromMetadata(meta)
			progress := progressResult(meta, llm.StructuredResult[T]{SessionID: meta.ProviderSessionID}, true)
			progress.Usage = draft.Response.Usage
			loadProgress(req.Progress, NewProgressEvent(req, ResumeSessionID(meta)), progress)
			return Result[T]{Draft: draft, Cached: true}, true, &TaskError{status: StatusFailedIsolated, err: errors.New(TaskErrorText(meta))}
		}
		if req.Store == nil {
			return zero, true, fmt.Errorf("llmlifecycle: store is required")
		}
		session, draft, err := loadOptionalTaskSession(ctx, req.Store, req.RunID, meta)
		if err != nil {
			return zero, true, err
		}
		progress := progressResult(meta, llm.StructuredResult[T]{SessionID: meta.ProviderSessionID}, true)
		progress.Usage = draft.Response.Usage
		loadProgress(req.Progress, NewProgressEvent(req, ResumeSessionID(meta)), progress)
		return Result[T]{Draft: draft, Session: session, Cached: true}, true, &TaskError{status: StatusFailedIsolated, err: errors.New(TaskErrorText(meta))}
	case StatusFailedBlocking:
		return zero, false, nil
	default:
		return zero, true, fmt.Errorf("llmlifecycle: task %q has unknown status %q", req.TaskID, meta.Status)
	}
}

func validateRequest[T any](req Request, decode llm.Decoder[T]) error {
	if req.Adapter == nil {
		return fmt.Errorf("llmlifecycle: adapter is required")
	}
	if decode == nil {
		return fmt.Errorf("llmlifecycle: decoder is required")
	}
	if req.Now == nil {
		return fmt.Errorf("llmlifecycle: clock is required")
	}
	if req.NewSessionRowID == nil {
		return fmt.Errorf("llmlifecycle: session row ID generator is required")
	}
	for field, value := range map[string]string{
		"task_id": req.TaskID,
		"phase":   req.Phase,
		"model":   req.Model,
		"prompt":  req.Prompt,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("llmlifecycle: %s is required", field)
		}
	}
	return nil
}

func failureStatus(ctx context.Context, req Request, err error, providerSessionID string, hasValidationAttempt bool) Status {
	supportsIsolation := req.FailureStatus != ""
	callerContextActive := ctx.Err() == nil && !isContextError(err)
	hasTaskExecutionEvidence := errors.Is(err, llm.ErrStructuredOutputInvalidAfterRetry) || strings.TrimSpace(providerSessionID) != "" || hasValidationAttempt
	if supportsIsolation && callerContextActive && hasTaskExecutionEvidence {
		return req.FailureStatus
	}
	return StatusFailedBlocking
}

// ValidatePayloadPath ensures a recorded payload path stays inside the task directory.
func ValidatePayloadPath(paths Paths, taskID, field, recordedPath string) (string, error) {
	recordedPath = strings.TrimSpace(recordedPath)
	if recordedPath == "" {
		return "", fmt.Errorf("llmlifecycle: task %q %s is empty", taskID, field)
	}
	taskDir, err := paths.TaskDir(taskID)
	if err != nil {
		return "", err
	}
	absDir, err := filepath.Abs(taskDir)
	if err != nil {
		return "", fmt.Errorf("llmlifecycle: resolve task %q directory: %w", taskID, err)
	}
	absPath, err := filepath.Abs(recordedPath)
	if err != nil {
		return "", fmt.Errorf("llmlifecycle: resolve task %q %s: %w", taskID, field, err)
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return "", fmt.Errorf("llmlifecycle: compare task %q %s: %w", taskID, field, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("llmlifecycle: task %q %s points outside the task artifact directory; pass --rerun to start a fresh review", taskID, field)
	}
	return absPath, nil
}

// ValidateMetadata checks whether task metadata is reusable for the request.
func ValidateMetadata(meta Metadata, req Request, adapter string) error {
	if meta.SchemaVersion != SchemaVersion {
		return fmt.Errorf("llmlifecycle: task %q schema version = %d, want %d", req.TaskID, meta.SchemaVersion, SchemaVersion)
	}
	if meta.TaskID != req.TaskID {
		return fmt.Errorf("llmlifecycle: task metadata ID %q does not match %q", meta.TaskID, req.TaskID)
	}
	if meta.Phase != req.Phase {
		return fmt.Errorf("llmlifecycle: task %q phase = %q, want %q", req.TaskID, meta.Phase, req.Phase)
	}
	if !slices.Equal(meta.DependencyTaskIDs, req.DependencyTaskIDs) {
		return fmt.Errorf("llmlifecycle: task %q dependency task ids = %#v, want %#v; pass --rerun to start a fresh review", req.TaskID, meta.DependencyTaskIDs, req.DependencyTaskIDs)
	}
	if strings.TrimSpace(adapter) != "" && meta.Adapter != adapter {
		return fmt.Errorf("llmlifecycle: task %q adapter = %q, want %q; pass --rerun to start a fresh review", req.TaskID, meta.Adapter, adapter)
	}
	fingerprint := strings.TrimSpace(req.InputFingerprint)
	if fingerprint == "" {
		fingerprint = Fingerprint(adapter, req.TaskID, req.Phase, req.Model, req.Effort, req.Prompt, req.DependencyTaskIDs)
	}
	if meta.InputFingerprint != fingerprint {
		return fmt.Errorf("llmlifecycle: task %q input fingerprint changed; pass --rerun to start a fresh review", req.TaskID)
	}
	return nil
}

// ReadMetadata reads task metadata when it exists.
func ReadMetadata(paths Paths, taskID string) (Metadata, bool, error) {
	path, err := paths.Metadata(taskID)
	if err != nil {
		return Metadata{}, false, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- metadata path is derived from task paths.
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, false, nil
	}
	if err != nil {
		return Metadata{}, false, fmt.Errorf("llmlifecycle: read task %q metadata: %w", taskID, err)
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return Metadata{}, false, fmt.Errorf("llmlifecycle: decode task %q metadata: %w", taskID, err)
	}
	return meta, true, nil
}

// WriteSuccess writes accepted output and metadata for a succeeded task.
func WriteSuccess(paths Paths, meta *Metadata, output []byte) error {
	outputPath, err := paths.ValidatedOutput(meta.TaskID)
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(outputPath, append(append([]byte(nil), output...), '\n')); err != nil {
		return err
	}
	meta.ValidatedOutputPath = outputPath
	return WriteMetadata(paths, *meta)
}

// WriteFailure writes invalid attempts and metadata for a failed task.
func WriteFailure(paths Paths, meta *Metadata, attempts []llm.StructuredValidationAttempt) error {
	for _, attempt := range attempts {
		attemptMeta := AttemptMetadata{
			Attempt:           attempt.Label,
			ProviderSessionID: attempt.SessionID,
			DecodeError:       SanitizeError(attempt.DecodeError),
		}
		if len(attempt.Response.StructuredOutput) > 0 {
			rawPath, err := paths.RawAttempt(meta.TaskID, attempt.Label)
			if err != nil {
				return err
			}
			if err := WriteFileAtomic(rawPath, append(append([]byte(nil), attempt.Response.StructuredOutput...), '\n')); err != nil {
				return err
			}
			attemptMeta.RawOutputPath = rawPath
		}
		meta.Attempts = append(meta.Attempts, attemptMeta)
	}
	return WriteMetadata(paths, *meta)
}

// WriteMetadata writes task metadata atomically.
func WriteMetadata(paths Paths, meta Metadata) error {
	path, err := paths.Metadata(meta.TaskID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'))
}

// WriteFileAtomic writes one artifact file via rename.
func WriteFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// BaseMetadata builds metadata shared by success and failure paths.
func BaseMetadata(req Request, draft SessionDraft) Metadata {
	agentID := ""
	if req.AgentID != nil {
		agentID = *req.AgentID
	}
	fingerprint := strings.TrimSpace(req.InputFingerprint)
	if fingerprint == "" {
		fingerprint = Fingerprint(adapterName(req.Adapter), req.TaskID, req.Phase, req.Model, req.Effort, req.Prompt, req.DependencyTaskIDs)
	}
	return Metadata{
		SchemaVersion:     SchemaVersion,
		TaskID:            req.TaskID,
		Phase:             req.Phase,
		DependencyTaskIDs: append([]string(nil), req.DependencyTaskIDs...),
		InputFingerprint:  fingerprint,
		AgentID:           agentID,
		SessionRowID:      draft.RowID,
		ProviderSessionID: draft.ProviderSessionID,
		Adapter:           adapterName(req.Adapter),
		Model:             draft.Model,
		Effort:            draft.Effort,
		LogPath:           req.LogPath,
		TokensIn:          draft.Response.Usage.TokensIn,
		TokensOut:         draft.Response.Usage.TokensOut,
		CacheRead:         draft.Response.Usage.CacheRead,
		CacheCreate:       draft.Response.Usage.CacheCreate,
		CostUSD:           draft.Response.Usage.CostUSD,
	}
}

// Fingerprint returns the stable reuse fingerprint for task inputs.
func Fingerprint(adapter, taskID, phase, model, effort, prompt string, deps []string) string {
	hash := sha256.New()
	for _, part := range []string{
		fmt.Sprintf("schema=%d", SchemaVersion),
		"adapter=" + adapter,
		"task=" + taskID,
		"phase=" + phase,
		"model=" + model,
		"effort=" + effort,
		"prompt=" + prompt,
	} {
		_, _ = io.WriteString(hash, part)
		_, _ = io.WriteString(hash, "\n")
	}
	for _, dep := range deps {
		_, _ = io.WriteString(hash, "dep="+dep+"\n")
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ResumeSessionID returns the best provider session ID for retrying a task.
func ResumeSessionID(meta Metadata) string {
	if strings.TrimSpace(meta.ProviderSessionID) != "" {
		return meta.ProviderSessionID
	}
	for i := len(meta.Attempts) - 1; i >= 0; i-- {
		if strings.TrimSpace(meta.Attempts[i].ProviderSessionID) != "" {
			return meta.Attempts[i].ProviderSessionID
		}
	}
	return ""
}

// TaskErrorText returns the persisted error text for a failed task.
func TaskErrorText(meta Metadata) string {
	if strings.TrimSpace(meta.Error) != "" {
		return meta.Error
	}
	return fmt.Sprintf("LLM task %q failed", meta.TaskID)
}

// SanitizeError prepares provider errors for durable metadata and markdown.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.Join(strings.Fields(strings.TrimSpace(err.Error())), " ")
	value = strings.ReplaceAll(value, "<!-- codereview:", "&lt;!-- codereview:")
	if utf8.RuneCountInString(value) > 1000 {
		runes := []rune(value)
		value = string(runes[:1000]) + "..."
	}
	return value
}

// NewProgressEvent builds a progress event from a task request.
func NewProgressEvent(req Request, resumeSessionID string) ProgressEvent {
	agentID := ""
	if req.AgentID != nil {
		agentID = *req.AgentID
	}
	source := "execute"
	if strings.TrimSpace(resumeSessionID) != "" {
		source = "resume"
	}
	return ProgressEvent{
		TaskID:          req.TaskID,
		Phase:           req.Phase,
		AgentID:         agentID,
		Model:           req.Model,
		Effort:          req.Effort,
		LogPath:         req.LogPath,
		ResumeSessionID: resumeSessionID,
		Source:          source,
	}
}

// SessionDraftFromLedger rebuilds a lifecycle draft from a ledger session.
func SessionDraftFromLedger(session ledger.Session) SessionDraft {
	completedAt := session.StartedAt
	if session.CompletedAt != nil {
		completedAt = *session.CompletedAt
	}
	draft := SessionDraft{
		RowID:                     session.SessionRowID,
		ProviderReportedSessionID: session.ProviderSessionID,
		ProviderSessionID:         session.ProviderSessionID,
		Role:                      session.Role,
		AgentID:                   session.AgentID,
		Adapter:                   session.Adapter,
		Model:                     session.Model,
		StartedAt:                 session.StartedAt,
		CompletedAt:               completedAt,
		Response: llm.Response{
			Usage: llm.Usage{
				TokensIn:    int64PtrToInt(session.TokensIn),
				TokensOut:   int64PtrToInt(session.TokensOut),
				CacheRead:   int64PtrToInt(session.CacheRead),
				CacheCreate: int64PtrToInt(session.CacheCreate),
				CostUSD:     session.CostUSD,
			},
		},
	}
	if session.DurationMS != nil {
		draft.Response.DurationMS = *session.DurationMS
	}
	if session.Effort != nil {
		draft.Effort = *session.Effort
	}
	return draft
}

// SessionDraftFromMetadata rebuilds a lifecycle draft from task metadata.
func SessionDraftFromMetadata(meta Metadata) SessionDraft {
	return SessionDraft{
		RowID:                     meta.SessionRowID,
		ProviderReportedSessionID: meta.ProviderSessionID,
		ProviderSessionID:         meta.ProviderSessionID,
		Adapter:                   meta.Adapter,
		Model:                     meta.Model,
		Effort:                    meta.Effort,
		Response: llm.Response{
			Usage: llm.Usage{
				TokensIn:    meta.TokensIn,
				TokensOut:   meta.TokensOut,
				CacheRead:   meta.CacheRead,
				CacheCreate: meta.CacheCreate,
				CostUSD:     meta.CostUSD,
			},
		},
	}
}

func loadTaskSession(ctx context.Context, store Store, runID string, meta Metadata) (ledger.Session, error) {
	if strings.TrimSpace(meta.SessionRowID) == "" {
		return ledger.Session{}, fmt.Errorf("llmlifecycle: task %q is missing session row id", meta.TaskID)
	}
	session, err := store.GetSession(ctx, meta.SessionRowID)
	if err != nil {
		return ledger.Session{}, fmt.Errorf("llmlifecycle: load task %q session %q: %w", meta.TaskID, meta.SessionRowID, err)
	}
	if session.RunID != runID {
		return ledger.Session{}, fmt.Errorf("llmlifecycle: task %q session belongs to run %q, want %q", meta.TaskID, session.RunID, runID)
	}
	return session, nil
}

func loadOptionalTaskSession(ctx context.Context, store Store, runID string, meta Metadata) (ledger.Session, SessionDraft, error) {
	if strings.TrimSpace(meta.SessionRowID) == "" {
		return ledger.Session{}, SessionDraftFromMetadata(meta), nil
	}
	session, err := loadTaskSession(ctx, store, runID, meta)
	if err != nil {
		return ledger.Session{}, SessionDraft{}, err
	}
	return session, SessionDraftFromLedger(session), nil
}

func progressResult[T any](meta Metadata, result llm.StructuredResult[T], cached bool) ProgressResult {
	providerSessionID := meta.ProviderSessionID
	if providerSessionID == "" {
		providerSessionID = result.SessionID
	}
	return ProgressResult{
		ProviderSessionID:  providerSessionID,
		Status:             string(meta.Status),
		ValidationAttempts: len(result.ValidationAttempts),
		Cached:             cached,
		Usage:              result.Response.Usage,
	}
}

func startProgress(progress Progress, event ProgressEvent) ProgressSpan {
	if progress == nil {
		return nil
	}
	return progress.StartLLMTask(event)
}

func endProgress(span ProgressSpan, err error, result ProgressResult) {
	if span != nil {
		span.End(err, result)
	}
}

func loadProgress(progress Progress, event ProgressEvent, result ProgressResult) {
	if progress == nil {
		return
	}
	progress.LoadLLMTask(event, result)
}

func adapterName(adapter llm.Adapter) string {
	if adapter == nil {
		return ""
	}
	return adapter.Name()
}

func intPtrToInt64(value *int) *int64 {
	if value == nil {
		return nil
	}
	converted := int64(*value)
	return &converted
}

func int64PtrToInt(value *int64) *int {
	if value == nil {
		return nil
	}
	converted := int(*value)
	return &converted
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func encodePathComponent(value string) string {
	return statepaths.Encode(value)
}
