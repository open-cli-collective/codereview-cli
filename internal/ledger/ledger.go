// Package ledger stores cr review runs in SQLite.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-cli-collective/codereview-cli/internal/dbmig"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	sqlite "modernc.org/sqlite" // register the SQLite database/sql driver.
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// SchemaVersion is the current ledger schema version.
	SchemaVersion = 2
	// DefaultBusyTimeout is the SQLite busy timeout configured at open.
	DefaultBusyTimeout = 5 * time.Second
	writeQueueSize     = 64
)

var (
	// ErrClosed means a mutating operation was attempted after Store.Close.
	ErrClosed = errors.New("ledger: store closed")
	// ErrNotFound means a requested ledger row does not exist.
	ErrNotFound = errors.New("ledger: not found")
	// ErrInvalidInput means a caller supplied an invalid storage value.
	ErrInvalidInput = errors.New("ledger: invalid input")
	// ErrRunExists means AllocateRun recovery mode received an existing run id.
	ErrRunExists = errors.New("ledger: run already exists")
)

// PostMode records whether a run is live or dry-run.
type PostMode string

// PostMode values.
const (
	PostModeLive   PostMode = "live"
	PostModeDryRun PostMode = "dry_run"
)

func (m PostMode) String() string { return string(m) }

// Valid reports whether m is a known post mode.
func (m PostMode) Valid() bool {
	switch m {
	case PostModeLive, PostModeDryRun:
		return true
	default:
		return false
	}
}

// ParsePostMode parses a storage post mode.
func ParsePostMode(value string) (PostMode, error) {
	mode := PostMode(normalizeStorageValue(value))
	if !mode.Valid() {
		return "", invalidInput("post_mode", value)
	}
	return mode, nil
}

// Outcome records a finalized run outcome.
type Outcome string

// Outcome values.
const (
	OutcomeIncomplete      Outcome = "incomplete"
	OutcomeApproved        Outcome = "approved"
	OutcomeRequestChanges  Outcome = "request_changes"
	OutcomeComment         Outcome = "comment"
	OutcomeNothingToReview Outcome = "nothing_to_review"
	OutcomeDryRun          Outcome = "dry_run"
	OutcomeAborted         Outcome = "aborted"
	OutcomeFailed          Outcome = "failed"
)

func (o Outcome) String() string { return string(o) }

// Valid reports whether o is a known run outcome.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeIncomplete, OutcomeApproved, OutcomeRequestChanges, OutcomeComment, OutcomeNothingToReview, OutcomeDryRun, OutcomeAborted, OutcomeFailed:
		return true
	default:
		return false
	}
}

// ParseOutcome parses a storage outcome.
func ParseOutcome(value string) (Outcome, error) {
	outcome := Outcome(normalizeStorageValue(value))
	if !outcome.Valid() {
		return "", invalidInput("outcome", value)
	}
	return outcome, nil
}

// SessionRole records the role attached to an LLM session row.
type SessionRole string

// SessionRole values.
const (
	SessionRoleOrchestrator SessionRole = "orchestrator"
	SessionRoleReviewer     SessionRole = "reviewer"
)

func (r SessionRole) String() string { return string(r) }

// Valid reports whether r is a known session role.
func (r SessionRole) Valid() bool {
	switch r {
	case SessionRoleOrchestrator, SessionRoleReviewer:
		return true
	default:
		return false
	}
}

// ParseSessionRole parses a storage session role.
func ParseSessionRole(value string) (SessionRole, error) {
	role := SessionRole(normalizeStorageValue(value))
	if !role.Valid() {
		return "", invalidInput("session role", value)
	}
	return role, nil
}

// PlannedActionKind identifies the host-side action to execute later.
type PlannedActionKind string

// PlannedActionKind values.
const (
	PlannedActionInlineComment PlannedActionKind = "inline_comment"
	PlannedActionThreadReply   PlannedActionKind = "thread_reply"
	PlannedActionResolveThread PlannedActionKind = "resolve_thread"
	PlannedActionRollupComment PlannedActionKind = "rollup_comment"
	PlannedActionSubmitReview  PlannedActionKind = "submit_review"
)

func (k PlannedActionKind) String() string { return string(k) }

// Valid reports whether k is a known planned action kind.
func (k PlannedActionKind) Valid() bool {
	switch k {
	case PlannedActionInlineComment, PlannedActionThreadReply, PlannedActionResolveThread, PlannedActionRollupComment, PlannedActionSubmitReview:
		return true
	default:
		return false
	}
}

// ParsePlannedActionKind parses a storage planned action kind.
func ParsePlannedActionKind(value string) (PlannedActionKind, error) {
	kind := PlannedActionKind(normalizeStorageValue(value))
	if !kind.Valid() {
		return "", invalidInput("planned action kind", value)
	}
	return kind, nil
}

// PlannedActionStatus records the durable outbox status for a planned action.
type PlannedActionStatus string

// PlannedActionStatus values.
const (
	PlannedActionPending        PlannedActionStatus = "pending"
	PlannedActionPosted         PlannedActionStatus = "posted"
	PlannedActionFailedTerminal PlannedActionStatus = "failed_terminal"
	PlannedActionSuperseded     PlannedActionStatus = "superseded"
	PlannedActionPlannedOnly    PlannedActionStatus = "planned_only"
)

func (s PlannedActionStatus) String() string { return string(s) }

// Valid reports whether s is a known planned action status.
func (s PlannedActionStatus) Valid() bool {
	switch s {
	case PlannedActionPending, PlannedActionPosted, PlannedActionFailedTerminal, PlannedActionSuperseded, PlannedActionPlannedOnly:
		return true
	default:
		return false
	}
}

// ParsePlannedActionStatus parses a storage planned action status.
func ParsePlannedActionStatus(value string) (PlannedActionStatus, error) {
	status := PlannedActionStatus(normalizeStorageValue(value))
	if !status.Valid() {
		return "", invalidInput("planned action status", value)
	}
	return status, nil
}

// PlannedActionFailureClass values stored for terminal post-phase failures.
const (
	PlannedActionFailureClassTerminal = "terminal"
	PlannedActionFailureClassAuth     = "auth"
)

// Store owns the SQLite connection and serializes mutating ledger writes.
type Store struct {
	db       *sql.DB
	writes   chan writeRequest
	done     chan struct{}
	closeErr error
	mu       sync.Mutex
	closed   bool
}

type writeRequest struct {
	ctx context.Context
	fn  func(context.Context, *sql.DB) error
	res chan error
}

// PR is the stable pull-request identity row.
type PR struct {
	PRKey       string
	PRURL       string
	FirstSeenAt time.Time
}

// Run is one cr review invocation recorded in the ledger.
type Run struct {
	RunID           string
	PRKey           string
	SHA             string
	BaseSHA         string
	Attempt         int
	Profile         string
	PostingIdentity string
	PostMode        PostMode
	StartedAt       time.Time
	HeartbeatAt     *time.Time
	CompletedAt     *time.Time
	Outcome         *Outcome
	ArtifactPath    string
	BlockingCount   int
	MajorCount      int
	MinorCount      int
	NitsCount       int
}

// AllocateRunParams contains the inputs needed to create a run row.
type AllocateRunParams struct {
	PRKey           string
	PRURL           string
	RunID           string
	SHA             string
	BaseSHA         string
	Profile         string
	PostingIdentity string
	PostMode        PostMode
	StartedAt       time.Time
	ArtifactPath    string
}

// ListRunsForHeadScopeParams identifies local rows for one PR/head/profile/identity scope.
type ListRunsForHeadScopeParams struct {
	PRKey           string
	SHA             string
	Profile         string
	PostingIdentity string
}

// Session records LLM session usage for a run.
type Session struct {
	SessionRowID      string
	RunID             string
	ProviderSessionID string
	Role              SessionRole
	AgentID           *string
	Adapter           string
	Model             string
	Effort            *string
	StartedAt         time.Time
	CompletedAt       *time.Time
	DurationMS        *int64
	TokensIn          *int64
	TokensOut         *int64
	CacheRead         *int64
	CacheCreate       *int64
	CostUSD           *float64
}

// Finding records one harness-assigned review finding.
type Finding struct {
	FindingID    review.FindingID
	RunID        string
	SessionRowID string
	Severity     review.Severity
	FilePath     string
	Side         *review.DiffSide
	Line         *int64
	DiffPosition *int64
	Anchoring    review.Anchoring
	Body         string
}

// PlannedAction records one outbox action planned for a run.
type PlannedAction struct {
	ActionID     string
	RunID        string
	Kind         PlannedActionKind
	FindingID    *string
	ThreadID     *string
	PlannedAt    time.Time
	PayloadJSON  string
	Status       PlannedActionStatus
	Required     bool
	Attempts     int
	AttemptedAt  *time.Time
	PostedAt     *time.Time
	UpstreamID   *string
	Error        *string
	FailureClass *string
}

// NamedSession records cross-run provider session reuse metadata.
type NamedSession struct {
	Name              string
	Profile           string
	Provider          string
	Adapter           string
	Model             string
	Host              string
	ProviderSessionID string
	CreatedAt         time.Time
	LastUsedAt        time.Time
}

// Open opens or creates a ledger database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, invalidInput("path", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ledger: create db parent: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("ledger: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := configureSQLite(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := dbmig.Apply(ctx, db, migrations()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ledger: migrate: %w", err)
	}

	store := &Store{
		db:     db,
		writes: make(chan writeRequest, writeQueueSize),
		done:   make(chan struct{}),
	}
	go store.writer()
	return store, nil
}

// Close stops the writer goroutine and closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	close(s.writes)
	s.mu.Unlock()

	<-s.done
	err := s.db.Close()

	s.mu.Lock()
	s.closeErr = err
	s.mu.Unlock()
	return err
}

func (s *Store) writer() {
	defer close(s.done)
	for req := range s.writes {
		req.res <- req.fn(req.ctx, s.db)
		close(req.res)
	}
}

func (s *Store) write(ctx context.Context, fn func(context.Context, *sql.DB) error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	req := writeRequest{ctx: ctx, fn: fn, res: make(chan error, 1)}
	select {
	case s.writes <- req:
		s.mu.Unlock()
	case <-ctx.Done():
		s.mu.Unlock()
		return ctx.Err()
	}

	// Once dispatched, report the writer result rather than racing context
	// cancellation against a write that may already have committed.
	return <-req.res
}

func (s *Store) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}

func configureSQLite(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("ledger: enable foreign keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("ledger: enable WAL: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", DefaultBusyTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("ledger: set busy timeout: %w", err)
	}
	return nil
}

func migrations() []dbmig.Migration {
	return []dbmig.Migration{
		{
			Version: 1,
			Name:    "ledger schema",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				for _, statement := range schemaStatements {
					if _, err := tx.ExecContext(ctx, statement); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			Version: 2,
			Name:    "planned action failure class",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `ALTER TABLE planned_actions ADD COLUMN failure_class TEXT`)
				return err
			},
		},
	}
}

// AllocateRun creates a run and allocates its attempt transactionally.
func (s *Store) AllocateRun(ctx context.Context, params AllocateRunParams) (Run, error) {
	if err := validateAllocateRunParams(params); err != nil {
		return Run{}, err
	}

	var run Run
	err := s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		inserted, err := allocateRunTx(ctx, db, params)
		if err != nil {
			return err
		}
		run = inserted
		return nil
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

func allocateRunTx(ctx context.Context, db *sql.DB, params AllocateRunParams) (Run, error) {
	for {
		run, err := allocateRunOnce(ctx, db, params)
		if err == nil {
			return run, nil
		}
		retry, classifyErr := classifyAllocateConstraint(ctx, db, params, err)
		if classifyErr != nil {
			return Run{}, classifyErr
		}
		if !retry {
			return Run{}, err
		}
	}
}

func classifyAllocateConstraint(ctx context.Context, db *sql.DB, params AllocateRunParams, err error) (bool, error) {
	if !isSQLiteConstraintError(err) {
		return false, err
	}
	if params.RunID != "" {
		exists, existsErr := runIDExists(ctx, db, params.RunID)
		if existsErr != nil {
			return false, existsErr
		}
		if exists {
			return false, fmt.Errorf("%w: %s", ErrRunExists, params.RunID)
		}
	}
	if isResumeAttemptConstraintError(err) {
		return true, nil
	}
	return false, err
}

func runIDExists(ctx context.Context, db *sql.DB, runID string) (bool, error) {
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE run_id = ?", runID).Scan(&exists); err != nil {
		return false, fmt.Errorf("ledger: check run id after constraint: %w", err)
	}
	return exists > 0, nil
}

func allocateRunOnce(ctx context.Context, db *sql.DB, params AllocateRunParams) (Run, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, fmt.Errorf("ledger: begin allocate run: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if params.RunID != "" {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE run_id = ?", params.RunID).Scan(&exists); err != nil {
			return Run{}, fmt.Errorf("ledger: check run id: %w", err)
		}
		if exists > 0 {
			return Run{}, fmt.Errorf("%w: %s", ErrRunExists, params.RunID)
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO prs (pr_key, pr_url, first_seen_at)
VALUES (?, ?, ?)
ON CONFLICT(pr_key) DO UPDATE SET pr_url = excluded.pr_url`,
		params.PRKey, params.PRURL, encodeTime(params.StartedAt),
	); err != nil {
		return Run{}, fmt.Errorf("ledger: upsert pr: %w", err)
	}

	var attempt int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(attempt), 0) + 1
FROM runs
WHERE pr_key = ? AND sha = ? AND base_sha = ? AND profile = ? AND posting_identity = ?`,
		params.PRKey, params.SHA, params.BaseSHA, params.Profile, params.PostingIdentity,
	).Scan(&attempt); err != nil {
		return Run{}, fmt.Errorf("ledger: allocate attempt: %w", err)
	}

	runID := params.RunID
	if runID == "" {
		runID = uuid.NewString()
	}
	run := Run{
		RunID:           runID,
		PRKey:           params.PRKey,
		SHA:             params.SHA,
		BaseSHA:         params.BaseSHA,
		Attempt:         attempt,
		Profile:         params.Profile,
		PostingIdentity: params.PostingIdentity,
		PostMode:        params.PostMode,
		StartedAt:       params.StartedAt.UTC(),
		ArtifactPath:    params.ArtifactPath,
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO runs (
	run_id, pr_key, sha, base_sha, attempt, profile, posting_identity, post_mode,
	started_at, heartbeat_at, completed_at, outcome, artifact_path,
	blocking_count, major_count, minor_count, nits_count
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.RunID, run.PRKey, run.SHA, run.BaseSHA, run.Attempt, run.Profile, run.PostingIdentity,
		run.PostMode.String(), encodeTime(run.StartedAt), nil, nil, nil, run.ArtifactPath,
		run.BlockingCount, run.MajorCount, run.MinorCount, run.NitsCount,
	); err != nil {
		return Run{}, fmt.Errorf("ledger: insert run: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("ledger: commit allocate run: %w", err)
	}
	return run, nil
}

// GetPR returns a pull-request identity row by key.
func (s *Store) GetPR(ctx context.Context, prKey string) (PR, error) {
	if strings.TrimSpace(prKey) == "" {
		return PR{}, invalidInput("pr_key", prKey)
	}
	if err := s.checkOpen(); err != nil {
		return PR{}, err
	}
	var pr PR
	var firstSeenAt string
	err := s.db.QueryRowContext(ctx, "SELECT pr_key, pr_url, first_seen_at FROM prs WHERE pr_key = ?", prKey).
		Scan(&pr.PRKey, &pr.PRURL, &firstSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PR{}, ErrNotFound
	}
	if err != nil {
		return PR{}, fmt.Errorf("ledger: get pr: %w", err)
	}
	parsed, err := parseTime(firstSeenAt)
	if err != nil {
		return PR{}, err
	}
	pr.FirstSeenAt = parsed
	return pr, nil
}

// GetRun returns a run by id.
func (s *Store) GetRun(ctx context.Context, runID string) (Run, error) {
	if strings.TrimSpace(runID) == "" {
		return Run{}, invalidInput("run_id", runID)
	}
	if err := s.checkOpen(); err != nil {
		return Run{}, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT run_id, pr_key, sha, base_sha, attempt, profile, posting_identity, post_mode,
	started_at, heartbeat_at, completed_at, outcome, artifact_path,
	blocking_count, major_count, minor_count, nits_count
FROM runs WHERE run_id = ?`, runID)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("ledger: get run: %w", err)
	}
	return run, nil
}

// ListRunsForHeadScope lists runs for one PR/head/profile/identity across bases.
func (s *Store) ListRunsForHeadScope(ctx context.Context, params ListRunsForHeadScopeParams) ([]Run, error) {
	if err := validateListRunsForHeadScopeParams(params); err != nil {
		return nil, err
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, pr_key, sha, base_sha, attempt, profile, posting_identity, post_mode,
	started_at, heartbeat_at, completed_at, outcome, artifact_path,
	blocking_count, major_count, minor_count, nits_count
FROM runs
WHERE pr_key = ? AND sha = ? AND profile = ? AND posting_identity = ?
ORDER BY base_sha, attempt DESC, started_at DESC, run_id`,
		params.PRKey, params.SHA, params.Profile, params.PostingIdentity,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: list runs for head scope: %w", err)
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list runs for head scope rows: %w", err)
	}
	return runs, nil
}

// DeleteRun deletes a run and lets SQLite cascade child rows.
func (s *Store) DeleteRun(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return invalidInput("run_id", runID)
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		result, err := db.ExecContext(ctx, "DELETE FROM runs WHERE run_id = ?", runID)
		if err != nil {
			return fmt.Errorf("ledger: delete run: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("ledger: delete run rows affected: %w", err)
		}
		if affected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// InsertSession inserts an LLM session row.
func (s *Store) InsertSession(ctx context.Context, session Session) error {
	if err := validateSession(session); err != nil {
		return err
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
INSERT INTO sessions (
	session_row_id, run_id, provider_session_id, role, agent_id, adapter, model, effort,
	started_at, completed_at, duration_ms, tokens_in, tokens_out, cache_read, cache_create, cost_usd
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			session.SessionRowID, session.RunID, session.ProviderSessionID, session.Role.String(), session.AgentID,
			session.Adapter, session.Model, session.Effort, encodeTime(session.StartedAt), encodeOptionalTime(session.CompletedAt),
			session.DurationMS, session.TokensIn, session.TokensOut, session.CacheRead, session.CacheCreate, session.CostUSD,
		)
		if err != nil {
			return fmt.Errorf("ledger: insert session: %w", err)
		}
		return nil
	})
}

// GetSession returns a session by internal row id.
func (s *Store) GetSession(ctx context.Context, sessionRowID string) (Session, error) {
	if strings.TrimSpace(sessionRowID) == "" {
		return Session{}, invalidInput("session_row_id", sessionRowID)
	}
	if err := s.checkOpen(); err != nil {
		return Session{}, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT session_row_id, run_id, provider_session_id, role, agent_id, adapter, model, effort,
	started_at, completed_at, duration_ms, tokens_in, tokens_out, cache_read, cache_create, cost_usd
FROM sessions WHERE session_row_id = ?`, sessionRowID)
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("ledger: get session: %w", err)
	}
	return session, nil
}

// ListSessionsForRun lists sessions for a run in stable order.
func (s *Store) ListSessionsForRun(ctx context.Context, runID string) ([]Session, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, invalidInput("run_id", runID)
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT session_row_id, run_id, provider_session_id, role, agent_id, adapter, model, effort,
	started_at, completed_at, duration_ms, tokens_in, tokens_out, cache_read, cache_create, cost_usd
FROM sessions WHERE run_id = ? ORDER BY session_row_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("ledger: list sessions for run: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list sessions for run rows: %w", err)
	}
	return sessions, nil
}

// InsertFinding inserts a validated harness finding row.
func (s *Store) InsertFinding(ctx context.Context, finding Finding) error {
	if err := validateFinding(finding); err != nil {
		return err
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
INSERT INTO findings (
	finding_id, run_id, session_row_id, severity, file_path, side, line, diff_position, anchoring, body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			finding.FindingID.String(), finding.RunID, finding.SessionRowID, finding.Severity.String(), finding.FilePath,
			finding.Side, finding.Line, finding.DiffPosition, finding.Anchoring.String(), finding.Body,
		)
		if err != nil {
			return fmt.Errorf("ledger: insert finding: %w", err)
		}
		return nil
	})
}

// ListFindings lists findings for a run in stable order.
func (s *Store) ListFindings(ctx context.Context, runID string) ([]Finding, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, invalidInput("run_id", runID)
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT finding_id, run_id, session_row_id, severity, file_path, side, line, diff_position, anchoring, body
FROM findings WHERE run_id = ? ORDER BY finding_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("ledger: list findings: %w", err)
	}
	defer rows.Close()

	var findings []Finding
	for rows.Next() {
		finding, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list findings rows: %w", err)
	}
	return findings, nil
}

// InsertPlannedAction inserts an outbox planned-action row.
func (s *Store) InsertPlannedAction(ctx context.Context, action PlannedAction) error {
	if err := validatePlannedAction(action); err != nil {
		return err
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		required := 0
		if action.Required {
			required = 1
		}
		_, err := db.ExecContext(ctx, `
INSERT INTO planned_actions (
	action_id, run_id, kind, finding_id, thread_id, planned_at, payload_json, status,
	required, attempts, attempted_at, posted_at, upstream_id, error, failure_class
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			action.ActionID, action.RunID, action.Kind.String(), action.FindingID, action.ThreadID, encodeTime(action.PlannedAt),
			action.PayloadJSON, action.Status.String(), required, action.Attempts, encodeOptionalTime(action.AttemptedAt),
			encodeOptionalTime(action.PostedAt), action.UpstreamID, action.Error, action.FailureClass,
		)
		if err != nil {
			return fmt.Errorf("ledger: insert planned action: %w", err)
		}
		return nil
	})
}

// ListPlannedActions lists planned actions for a run in stable order.
func (s *Store) ListPlannedActions(ctx context.Context, runID string) ([]PlannedAction, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, invalidInput("run_id", runID)
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT action_id, run_id, kind, finding_id, thread_id, planned_at, payload_json, status,
	required, attempts, attempted_at, posted_at, upstream_id, error, failure_class
FROM planned_actions WHERE run_id = ? ORDER BY action_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("ledger: list planned actions: %w", err)
	}
	defer rows.Close()

	var actions []PlannedAction
	for rows.Next() {
		action, err := scanPlannedAction(rows)
		if err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list planned action rows: %w", err)
	}
	return actions, nil
}

// UpdatePlannedAction updates the mutable outbox state for an existing action.
func (s *Store) UpdatePlannedAction(ctx context.Context, action PlannedAction) error {
	if err := validatePlannedAction(action); err != nil {
		return err
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		result, err := db.ExecContext(ctx, `
UPDATE planned_actions
SET status = ?, attempts = ?, attempted_at = ?, posted_at = ?, upstream_id = ?, error = ?, failure_class = ?
WHERE action_id = ? AND run_id = ?`,
			action.Status.String(), action.Attempts, encodeOptionalTime(action.AttemptedAt),
			encodeOptionalTime(action.PostedAt), action.UpstreamID, action.Error, action.FailureClass, action.ActionID, action.RunID,
		)
		if err != nil {
			return fmt.Errorf("ledger: update planned action: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("ledger: update planned action rows affected: %w", err)
		}
		if affected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CompleteRun records the terminal post-phase outcome for a run.
func (s *Store) CompleteRun(ctx context.Context, runID string, outcome Outcome, completedAt time.Time) error {
	if strings.TrimSpace(runID) == "" {
		return invalidInput("run_id", runID)
	}
	if !outcome.Valid() {
		return invalidInput("outcome", outcome.String())
	}
	if completedAt.IsZero() {
		return invalidInput("completed_at", "")
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		result, err := db.ExecContext(ctx, `
UPDATE runs
SET outcome = ?, completed_at = ?
WHERE run_id = ?`,
			outcome.String(), encodeTime(completedAt), runID,
		)
		if err != nil {
			return fmt.Errorf("ledger: complete run: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("ledger: complete run rows affected: %w", err)
		}
		if affected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// UpdateHeartbeat records the latest liveness timestamp for a run.
func (s *Store) UpdateHeartbeat(ctx context.Context, runID string, heartbeatAt time.Time) error {
	if strings.TrimSpace(runID) == "" {
		return invalidInput("run_id", runID)
	}
	if heartbeatAt.IsZero() {
		return invalidInput("heartbeat_at", "")
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		result, err := db.ExecContext(ctx, `
UPDATE runs
SET heartbeat_at = ?
WHERE run_id = ?`,
			encodeTime(heartbeatAt), runID,
		)
		if err != nil {
			return fmt.Errorf("ledger: update heartbeat: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("ledger: update heartbeat rows affected: %w", err)
		}
		if affected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// UpsertNamedSession inserts or updates a named provider session row.
func (s *Store) UpsertNamedSession(ctx context.Context, session NamedSession) error {
	if err := validateNamedSession(session); err != nil {
		return err
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
INSERT INTO named_sessions (
	name, profile, provider, adapter, model, host, provider_session_id, created_at, last_used_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
	profile = excluded.profile,
	provider = excluded.provider,
	adapter = excluded.adapter,
	model = excluded.model,
	host = excluded.host,
	provider_session_id = excluded.provider_session_id,
	last_used_at = excluded.last_used_at`,
			session.Name, session.Profile, session.Provider, session.Adapter, session.Model, session.Host,
			session.ProviderSessionID, encodeTime(session.CreatedAt), encodeTime(session.LastUsedAt),
		)
		if err != nil {
			return fmt.Errorf("ledger: upsert named session: %w", err)
		}
		return nil
	})
}

// GetNamedSession returns a named provider session row.
func (s *Store) GetNamedSession(ctx context.Context, name string) (NamedSession, error) {
	if strings.TrimSpace(name) == "" {
		return NamedSession{}, invalidInput("name", name)
	}
	if err := s.checkOpen(); err != nil {
		return NamedSession{}, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT name, profile, provider, adapter, model, host, provider_session_id, created_at, last_used_at
FROM named_sessions WHERE name = ?`, name)
	session, err := scanNamedSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return NamedSession{}, ErrNotFound
	}
	if err != nil {
		return NamedSession{}, fmt.Errorf("ledger: get named session: %w", err)
	}
	return session, nil
}

// ListNamedSessions returns named provider sessions in stable name order.
func (s *Store) ListNamedSessions(ctx context.Context) ([]NamedSession, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT name, profile, provider, adapter, model, host, provider_session_id, created_at, last_used_at
FROM named_sessions ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("ledger: list named sessions: %w", err)
	}
	defer rows.Close()

	var sessions []NamedSession
	for rows.Next() {
		session, err := scanNamedSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list named sessions rows: %w", err)
	}
	return sessions, nil
}

// DeleteNamedSession deletes a named provider session row.
func (s *Store) DeleteNamedSession(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidInput("name", name)
	}
	return s.write(ctx, func(ctx context.Context, db *sql.DB) error {
		result, err := db.ExecContext(ctx, "DELETE FROM named_sessions WHERE name = ?", name)
		if err != nil {
			return fmt.Errorf("ledger: delete named session: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("ledger: delete named session rows affected: %w", err)
		}
		if affected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func validateListRunsForHeadScopeParams(params ListRunsForHeadScopeParams) error {
	required := map[string]string{
		"pr_key":           params.PRKey,
		"sha":              params.SHA,
		"profile":          params.Profile,
		"posting_identity": params.PostingIdentity,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return invalidInput(name, value)
		}
	}
	return nil
}

func validateAllocateRunParams(params AllocateRunParams) error {
	for field, value := range map[string]string{
		"pr_key":           params.PRKey,
		"pr_url":           params.PRURL,
		"sha":              params.SHA,
		"base_sha":         params.BaseSHA,
		"profile":          params.Profile,
		"posting_identity": params.PostingIdentity,
		"artifact_path":    params.ArtifactPath,
	} {
		if strings.TrimSpace(value) == "" {
			return invalidInput(field, value)
		}
	}
	if params.StartedAt.IsZero() {
		return invalidInput("started_at", "")
	}
	if !params.PostMode.Valid() {
		return invalidInput("post_mode", params.PostMode.String())
	}
	return nil
}

func validateSession(session Session) error {
	for field, value := range map[string]string{
		"session_row_id":      session.SessionRowID,
		"run_id":              session.RunID,
		"provider_session_id": session.ProviderSessionID,
		"adapter":             session.Adapter,
		"model":               session.Model,
	} {
		if strings.TrimSpace(value) == "" {
			return invalidInput(field, value)
		}
	}
	if !session.Role.Valid() {
		return invalidInput("role", session.Role.String())
	}
	if session.StartedAt.IsZero() {
		return invalidInput("started_at", "")
	}
	return nil
}

func validateFinding(finding Finding) error {
	for field, value := range map[string]string{
		"finding_id":     finding.FindingID.String(),
		"run_id":         finding.RunID,
		"session_row_id": finding.SessionRowID,
		"file_path":      finding.FilePath,
		"body":           finding.Body,
	} {
		if strings.TrimSpace(value) == "" {
			return invalidInput(field, value)
		}
	}
	if !finding.Severity.Valid() {
		return invalidInput("severity", finding.Severity.String())
	}
	if finding.Side != nil && !finding.Side.Valid() {
		return invalidInput("side", finding.Side.String())
	}
	if !finding.Anchoring.Valid() {
		return invalidInput("anchoring", finding.Anchoring.String())
	}
	return nil
}

func validatePlannedAction(action PlannedAction) error {
	for field, value := range map[string]string{
		"action_id":    action.ActionID,
		"run_id":       action.RunID,
		"payload_json": action.PayloadJSON,
	} {
		if strings.TrimSpace(value) == "" {
			return invalidInput(field, value)
		}
	}
	if !action.Kind.Valid() {
		return invalidInput("kind", action.Kind.String())
	}
	if !action.Status.Valid() {
		return invalidInput("status", action.Status.String())
	}
	if action.PlannedAt.IsZero() {
		return invalidInput("planned_at", "")
	}
	if action.Attempts < 0 {
		return invalidInput("attempts", fmt.Sprint(action.Attempts))
	}
	if action.FailureClass != nil && !validPlannedActionFailureClass(*action.FailureClass) {
		return invalidInput("failure_class", *action.FailureClass)
	}
	return nil
}

func validPlannedActionFailureClass(value string) bool {
	switch value {
	case PlannedActionFailureClassTerminal, PlannedActionFailureClassAuth:
		return true
	default:
		return false
	}
}

func validateNamedSession(session NamedSession) error {
	for field, value := range map[string]string{
		"name":                session.Name,
		"profile":             session.Profile,
		"provider":            session.Provider,
		"adapter":             session.Adapter,
		"model":               session.Model,
		"host":                session.Host,
		"provider_session_id": session.ProviderSessionID,
	} {
		if strings.TrimSpace(value) == "" {
			return invalidInput(field, value)
		}
	}
	if session.CreatedAt.IsZero() {
		return invalidInput("created_at", "")
	}
	if session.LastUsedAt.IsZero() {
		return invalidInput("last_used_at", "")
	}
	return nil
}

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var (
		run             Run
		postMode        string
		startedAt       string
		heartbeatAt     sql.NullString
		completedAt     sql.NullString
		outcome         sql.NullString
		blocking, major int
		minor, nits     int
	)
	if err := row.Scan(
		&run.RunID, &run.PRKey, &run.SHA, &run.BaseSHA, &run.Attempt, &run.Profile, &run.PostingIdentity,
		&postMode, &startedAt, &heartbeatAt, &completedAt, &outcome, &run.ArtifactPath,
		&blocking, &major, &minor, &nits,
	); err != nil {
		return Run{}, err
	}
	parsedPostMode, err := ParsePostMode(postMode)
	if err != nil {
		return Run{}, err
	}
	run.PostMode = parsedPostMode
	run.StartedAt, err = parseTime(startedAt)
	if err != nil {
		return Run{}, err
	}
	if heartbeatAt.Valid {
		parsed, err := parseTime(heartbeatAt.String)
		if err != nil {
			return Run{}, err
		}
		run.HeartbeatAt = &parsed
	}
	if completedAt.Valid {
		parsed, err := parseTime(completedAt.String)
		if err != nil {
			return Run{}, err
		}
		run.CompletedAt = &parsed
	}
	if outcome.Valid {
		parsed, err := ParseOutcome(outcome.String)
		if err != nil {
			return Run{}, err
		}
		run.Outcome = &parsed
	}
	run.BlockingCount = blocking
	run.MajorCount = major
	run.MinorCount = minor
	run.NitsCount = nits
	return run, nil
}

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var (
		session     Session
		role        string
		agentID     sql.NullString
		effort      sql.NullString
		startedAt   string
		completedAt sql.NullString
		durationMS  sql.NullInt64
		tokensIn    sql.NullInt64
		tokensOut   sql.NullInt64
		cacheRead   sql.NullInt64
		cacheCreate sql.NullInt64
		costUSD     sql.NullFloat64
	)
	if err := row.Scan(
		&session.SessionRowID, &session.RunID, &session.ProviderSessionID, &role, &agentID, &session.Adapter,
		&session.Model, &effort, &startedAt, &completedAt, &durationMS, &tokensIn, &tokensOut,
		&cacheRead, &cacheCreate, &costUSD,
	); err != nil {
		return Session{}, err
	}
	parsedRole, err := ParseSessionRole(role)
	if err != nil {
		return Session{}, err
	}
	session.Role = parsedRole
	session.AgentID = stringPtrFromNull(agentID)
	session.Effort = stringPtrFromNull(effort)
	session.StartedAt, err = parseTime(startedAt)
	if err != nil {
		return Session{}, err
	}
	session.CompletedAt, err = timePtrFromNull(completedAt)
	if err != nil {
		return Session{}, err
	}
	session.DurationMS = int64PtrFromNull(durationMS)
	session.TokensIn = int64PtrFromNull(tokensIn)
	session.TokensOut = int64PtrFromNull(tokensOut)
	session.CacheRead = int64PtrFromNull(cacheRead)
	session.CacheCreate = int64PtrFromNull(cacheCreate)
	session.CostUSD = float64PtrFromNull(costUSD)
	return session, nil
}

func scanFinding(row interface{ Scan(...any) error }) (Finding, error) {
	var (
		finding      Finding
		findingID    string
		severity     string
		side         sql.NullString
		line         sql.NullInt64
		diffPosition sql.NullInt64
		anchoring    string
	)
	if err := row.Scan(
		&findingID, &finding.RunID, &finding.SessionRowID, &severity, &finding.FilePath,
		&side, &line, &diffPosition, &anchoring, &finding.Body,
	); err != nil {
		return Finding{}, err
	}
	finding.FindingID = review.FindingID(findingID)
	parsedSeverity, err := review.ParseSeverity(severity)
	if err != nil {
		return Finding{}, err
	}
	finding.Severity = parsedSeverity
	if side.Valid {
		parsed, err := review.ParseDiffSide(side.String)
		if err != nil {
			return Finding{}, err
		}
		finding.Side = &parsed
	}
	finding.Line = int64PtrFromNull(line)
	finding.DiffPosition = int64PtrFromNull(diffPosition)
	parsedAnchoring, err := review.ParseAnchoring(anchoring)
	if err != nil {
		return Finding{}, err
	}
	finding.Anchoring = parsedAnchoring
	return finding, nil
}

func scanPlannedAction(row interface{ Scan(...any) error }) (PlannedAction, error) {
	var (
		action       PlannedAction
		kind         string
		findingID    sql.NullString
		threadID     sql.NullString
		plannedAt    string
		status       string
		required     int
		attemptedAt  sql.NullString
		postedAt     sql.NullString
		upstreamID   sql.NullString
		errorText    sql.NullString
		failureClass sql.NullString
	)
	if err := row.Scan(
		&action.ActionID, &action.RunID, &kind, &findingID, &threadID, &plannedAt, &action.PayloadJSON,
		&status, &required, &action.Attempts, &attemptedAt, &postedAt, &upstreamID, &errorText, &failureClass,
	); err != nil {
		return PlannedAction{}, err
	}
	parsedKind, err := ParsePlannedActionKind(kind)
	if err != nil {
		return PlannedAction{}, err
	}
	action.Kind = parsedKind
	parsedStatus, err := ParsePlannedActionStatus(status)
	if err != nil {
		return PlannedAction{}, err
	}
	action.Status = parsedStatus
	action.FindingID = stringPtrFromNull(findingID)
	action.ThreadID = stringPtrFromNull(threadID)
	action.PlannedAt, err = parseTime(plannedAt)
	if err != nil {
		return PlannedAction{}, err
	}
	action.Required = required != 0
	action.AttemptedAt, err = timePtrFromNull(attemptedAt)
	if err != nil {
		return PlannedAction{}, err
	}
	action.PostedAt, err = timePtrFromNull(postedAt)
	if err != nil {
		return PlannedAction{}, err
	}
	action.UpstreamID = stringPtrFromNull(upstreamID)
	action.Error = stringPtrFromNull(errorText)
	if failureClass.Valid {
		if !validPlannedActionFailureClass(failureClass.String) {
			return PlannedAction{}, invalidInput("failure_class", failureClass.String)
		}
		value := failureClass.String
		action.FailureClass = &value
	}
	return action, nil
}

func scanNamedSession(row interface{ Scan(...any) error }) (NamedSession, error) {
	var session NamedSession
	var createdAt, lastUsedAt string
	if err := row.Scan(
		&session.Name, &session.Profile, &session.Provider, &session.Adapter, &session.Model, &session.Host,
		&session.ProviderSessionID, &createdAt, &lastUsedAt,
	); err != nil {
		return NamedSession{}, err
	}
	parsedCreated, err := parseTime(createdAt)
	if err != nil {
		return NamedSession{}, err
	}
	parsedLastUsed, err := parseTime(lastUsedAt)
	if err != nil {
		return NamedSession{}, err
	}
	session.CreatedAt = parsedCreated
	session.LastUsedAt = parsedLastUsed
	return session, nil
}

func encodeTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func encodeOptionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return encodeTime(*value)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("ledger: parse time %q: %w", value, err)
	}
	return parsed, nil
}

func timePtrFromNull(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func stringPtrFromNull(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func int64PtrFromNull(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func float64PtrFromNull(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func normalizeStorageValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func invalidInput(field, value string) error {
	return fmt.Errorf("%w: %s %q", ErrInvalidInput, field, value)
}

func isSQLiteConstraintError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
}

func isResumeAttemptConstraintError(err error) bool {
	message := err.Error()
	for _, column := range []string{
		"runs.pr_key",
		"runs.sha",
		"runs.base_sha",
		"runs.profile",
		"runs.posting_identity",
		"runs.attempt",
	} {
		if !strings.Contains(message, column) {
			return false
		}
	}
	return true
}

var schemaStatements = []string{
	`CREATE TABLE prs (
  pr_key         TEXT PRIMARY KEY,
  pr_url         TEXT NOT NULL,
  first_seen_at  TEXT NOT NULL
)`,
	`CREATE TABLE runs (
  run_id         TEXT PRIMARY KEY,
  pr_key         TEXT NOT NULL REFERENCES prs(pr_key),
  sha            TEXT NOT NULL,
  base_sha       TEXT NOT NULL,
  attempt        INTEGER NOT NULL,
  profile        TEXT NOT NULL,
  posting_identity TEXT NOT NULL,
  post_mode      TEXT NOT NULL DEFAULT 'live',
  started_at     TEXT NOT NULL,
  heartbeat_at   TEXT,
  completed_at   TEXT,
  outcome        TEXT,
  artifact_path  TEXT NOT NULL,
  blocking_count INTEGER NOT NULL DEFAULT 0,
  major_count    INTEGER NOT NULL DEFAULT 0,
  minor_count    INTEGER NOT NULL DEFAULT 0,
  nits_count     INTEGER NOT NULL DEFAULT 0,
  UNIQUE(pr_key, sha, base_sha, profile, posting_identity, attempt)
)`,
	`CREATE INDEX runs_pr_sha ON runs(pr_key, sha)`,
	`CREATE INDEX runs_resume ON runs(pr_key, sha, base_sha, profile, posting_identity, post_mode, outcome)`,
	`CREATE INDEX runs_started_at ON runs(started_at)`,
	`CREATE TABLE sessions (
  session_row_id      TEXT PRIMARY KEY,
  run_id              TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  provider_session_id TEXT NOT NULL,
  role                TEXT NOT NULL,
  agent_id            TEXT,
  adapter             TEXT NOT NULL,
  model               TEXT NOT NULL,
  effort              TEXT,
  started_at          TEXT NOT NULL,
  completed_at        TEXT,
  duration_ms         INTEGER,
  tokens_in           INTEGER,
  tokens_out          INTEGER,
  cache_read          INTEGER,
  cache_create        INTEGER,
  cost_usd            REAL
)`,
	`CREATE INDEX sessions_run ON sessions(run_id)`,
	`CREATE INDEX sessions_provider ON sessions(provider_session_id)`,
	`CREATE TABLE findings (
  finding_id     TEXT PRIMARY KEY,
  run_id         TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  session_row_id TEXT NOT NULL REFERENCES sessions(session_row_id) ON DELETE CASCADE,
  severity       TEXT NOT NULL,
  file_path      TEXT NOT NULL,
  side           TEXT,
  line           INTEGER,
  diff_position  INTEGER,
  anchoring      TEXT NOT NULL,
  body           TEXT NOT NULL
)`,
	`CREATE INDEX findings_run ON findings(run_id)`,
	`CREATE TABLE planned_actions (
  action_id      TEXT PRIMARY KEY,
  run_id         TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  kind           TEXT NOT NULL,
  finding_id     TEXT REFERENCES findings(finding_id) ON DELETE CASCADE,
  thread_id      TEXT,
  planned_at     TEXT NOT NULL,
  payload_json   TEXT NOT NULL,
  status         TEXT NOT NULL,
  required       INTEGER NOT NULL DEFAULT 0,
  attempts       INTEGER NOT NULL DEFAULT 0,
  attempted_at   TEXT,
  posted_at      TEXT,
  upstream_id    TEXT,
  error          TEXT
)`,
	`CREATE INDEX planned_actions_run ON planned_actions(run_id)`,
	`CREATE INDEX planned_actions_status ON planned_actions(status)`,
	`CREATE TABLE named_sessions (
  name           TEXT PRIMARY KEY,
  profile        TEXT NOT NULL,
  provider       TEXT NOT NULL,
  adapter        TEXT NOT NULL,
  model          TEXT NOT NULL,
  host           TEXT NOT NULL,
  provider_session_id TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  last_used_at   TEXT NOT NULL
)`,
}
