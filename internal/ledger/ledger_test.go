package ledger

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/review"
	_ "modernc.org/sqlite"
)

func TestOpenMigratesFreshDatabaseAndAppliesStartupContract(t *testing.T) {
	store := openStore(t)

	if version := queryInt(t, store.db, "SELECT schema_version FROM meta"); version != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", version, SchemaVersion)
	}
	if got := queryInt(t, store.db, "PRAGMA foreign_keys"); got != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1", got)
	}
	if got := queryString(t, store.db, "PRAGMA journal_mode"); got != "wal" {
		t.Fatalf("PRAGMA journal_mode = %q, want wal", got)
	}
	if got := queryInt(t, store.db, "PRAGMA busy_timeout"); int64(got) != DefaultBusyTimeout.Milliseconds() {
		t.Fatalf("PRAGMA busy_timeout = %d, want %d", got, DefaultBusyTimeout.Milliseconds())
	}

	for _, table := range []string{"prs", "runs", "sessions", "findings", "planned_actions", "named_sessions"} {
		if !tableExists(t, store.db, table) {
			t.Fatalf("table %s does not exist", table)
		}
	}
	for _, index := range []string{"runs_pr_sha", "runs_resume", "runs_started_at", "sessions_run", "sessions_provider", "findings_run", "planned_actions_run", "planned_actions_status"} {
		if !indexExists(t, store.db, index) {
			t.Fatalf("index %s does not exist", index)
		}
	}
}

func TestForeignKeyCascadeDeletesRunChildren(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	run := allocateRun(t, store, validAllocateRunParams())
	session := validSession(run.RunID)
	insertSession(t, store, session)
	insertFinding(t, store, validFinding(run.RunID, session.SessionRowID))
	insertPlannedAction(t, store, validPlannedAction(run.RunID))

	if err := store.DeleteRun(ctx, run.RunID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}

	if _, err := store.GetRun(ctx, run.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRun after delete error = %v, want ErrNotFound", err)
	}
	if count := queryInt(t, store.db, "SELECT COUNT(*) FROM sessions"); count != 0 {
		t.Fatalf("sessions count = %d, want 0", count)
	}
	if count := queryInt(t, store.db, "SELECT COUNT(*) FROM findings"); count != 0 {
		t.Fatalf("findings count = %d, want 0", count)
	}
	if count := queryInt(t, store.db, "SELECT COUNT(*) FROM planned_actions"); count != 0 {
		t.Fatalf("planned_actions count = %d, want 0", count)
	}
}

func TestRunUniqueResumeAttemptConstraint(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, validAllocateRunParams())

	_, err := store.db.ExecContext(
		context.Background(),
		`INSERT INTO runs (
			run_id, pr_key, sha, base_sha, attempt, profile, posting_identity, post_mode,
			started_at, artifact_path
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"manual-run", run.PRKey, run.SHA, run.BaseSHA, run.Attempt, run.Profile, run.PostingIdentity,
		run.PostMode.String(), encodeTime(run.StartedAt), "/tmp/manual",
	)
	if err == nil {
		t.Fatal("duplicate resume attempt insert error = nil, want unique constraint failure")
	}
}

func TestAllocateRunAllocatesSequentialAttempts(t *testing.T) {
	store := openStore(t)
	params := validAllocateRunParams()

	first := allocateRun(t, store, params)
	params.RunID = ""
	params.StartedAt = params.StartedAt.Add(time.Minute)
	params.ArtifactPath = "/tmp/run-2"
	second := allocateRun(t, store, params)

	if first.Attempt != 1 {
		t.Fatalf("first attempt = %d, want 1", first.Attempt)
	}
	if second.Attempt != 2 {
		t.Fatalf("second attempt = %d, want 2", second.Attempt)
	}
}

func TestAllocateRunSeparatesAttemptSequencesByResumeKey(t *testing.T) {
	store := openStore(t)
	base := validAllocateRunParams()
	baseRun := allocateRun(t, store, base)
	if baseRun.Attempt != 1 {
		t.Fatalf("base attempt = %d, want 1", baseRun.Attempt)
	}

	tests := []struct {
		name   string
		mutate func(*AllocateRunParams)
	}{
		{name: "pr key", mutate: func(p *AllocateRunParams) { p.PRKey = "github_other_repo_1" }},
		{name: "sha", mutate: func(p *AllocateRunParams) { p.SHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" }},
		{name: "base sha", mutate: func(p *AllocateRunParams) { p.BaseSHA = "cccccccccccccccccccccccccccccccccccccccc" }},
		{name: "profile", mutate: func(p *AllocateRunParams) { p.Profile = "other" }},
		{name: "posting identity", mutate: func(p *AllocateRunParams) { p.PostingIdentity = "other@example.com" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := base
			params.RunID = ""
			params.ArtifactPath = filepath.Join("/tmp", tt.name)
			tt.mutate(&params)

			run := allocateRun(t, store, params)
			if run.Attempt != 1 {
				t.Fatalf("attempt = %d, want independent sequence at 1", run.Attempt)
			}
		})
	}
}

func TestAllocateRunConcurrentSameKey(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	params := validAllocateRunParams()

	const callers = 2
	var wg sync.WaitGroup
	results := make(chan Run, callers)
	errs := make(chan error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := params
			p.RunID = ""
			p.ArtifactPath = filepath.Join("/tmp", "concurrent", strconv.Itoa(i))
			run, err := store.AllocateRun(ctx, p)
			if err != nil {
				errs <- err
				return
			}
			results <- run
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("AllocateRun concurrent error: %v", err)
	}
	var attempts []int
	for run := range results {
		attempts = append(attempts, run.Attempt)
	}
	slices.Sort(attempts)
	if !reflect.DeepEqual(attempts, []int{1, 2}) {
		t.Fatalf("attempts = %v, want [1 2]", attempts)
	}
}

func TestAllocateRunRecoveryMode(t *testing.T) {
	store := openStore(t)
	params := validAllocateRunParams()
	params.RunID = "recovered-run"

	run := allocateRun(t, store, params)
	if run.RunID != "recovered-run" {
		t.Fatalf("RunID = %q, want recovered-run", run.RunID)
	}
	if run.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1", run.Attempt)
	}

	got, err := store.GetRun(context.Background(), "recovered-run")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunID != run.RunID || got.Attempt != run.Attempt {
		t.Fatalf("GetRun = %#v, want run %#v", got, run)
	}
}

func TestAllocateRunRecoveryModeUsesNextAttempt(t *testing.T) {
	store := openStore(t)
	params := validAllocateRunParams()
	first := allocateRun(t, store, params)

	params.RunID = "recovered-run"
	params.StartedAt = params.StartedAt.Add(time.Minute)
	params.ArtifactPath = "/tmp/recovered-run"
	recovered := allocateRun(t, store, params)

	if first.Attempt != 1 {
		t.Fatalf("first attempt = %d, want 1", first.Attempt)
	}
	if recovered.Attempt != 2 {
		t.Fatalf("recovered attempt = %d, want 2", recovered.Attempt)
	}
}

func TestAllocateRunRecoveryRejectsExistingRunID(t *testing.T) {
	store := openStore(t)
	params := validAllocateRunParams()
	params.RunID = "same-run"
	original := allocateRun(t, store, params)

	params.PRURL = "https://example.test/changed"
	params.ArtifactPath = "/tmp/changed"
	_, err := store.AllocateRun(context.Background(), params)
	if !errors.Is(err, ErrRunExists) {
		t.Fatalf("AllocateRun duplicate error = %v, want ErrRunExists", err)
	}

	got, err := store.GetRun(context.Background(), original.RunID)
	if err != nil {
		t.Fatalf("GetRun original: %v", err)
	}
	if got.ArtifactPath != original.ArtifactPath {
		t.Fatalf("ArtifactPath = %q, want original %q", got.ArtifactPath, original.ArtifactPath)
	}
}

func TestClassifyAllocateConstraintForRecoveryMode(t *testing.T) {
	store := openStore(t)
	params := validAllocateRunParams()
	params.RunID = "existing-run"
	allocateRun(t, store, params)

	retry, err := classifyAllocateConstraint(context.Background(), store.db, params, errors.New("constraint failed"))
	if !errors.Is(err, ErrRunExists) {
		t.Fatalf("classify existing run error = %v, want ErrRunExists", err)
	}
	if retry {
		t.Fatal("classify existing run retry = true, want false")
	}

	params.RunID = "missing-run"
	retry, err = classifyAllocateConstraint(context.Background(), store.db, params, errors.New("constraint failed"))
	if err != nil {
		t.Fatalf("classify missing run error = %v, want nil", err)
	}
	if !retry {
		t.Fatal("classify missing run retry = false, want true")
	}
}

func TestAllocateRunPreservesPRFirstSeenAndUpdatesURL(t *testing.T) {
	store := openStore(t)
	params := validAllocateRunParams()
	first := allocateRun(t, store, params)

	params.RunID = ""
	params.PRURL = "https://example.test/new-url"
	params.StartedAt = params.StartedAt.Add(time.Minute)
	params.ArtifactPath = "/tmp/second"
	allocateRun(t, store, params)

	pr, err := store.GetPR(context.Background(), first.PRKey)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.PRURL != "https://example.test/new-url" {
		t.Fatalf("PRURL = %q, want updated URL", pr.PRURL)
	}
	if !pr.FirstSeenAt.Equal(first.StartedAt) {
		t.Fatalf("FirstSeenAt = %s, want first run started_at %s", pr.FirstSeenAt, first.StartedAt)
	}
}

func TestTypedPersistenceRoundTrips(t *testing.T) {
	store := openStore(t)
	run := allocateRun(t, store, validAllocateRunParams())

	session := validSession(run.RunID)
	insertSession(t, store, session)
	gotSession, err := store.GetSession(context.Background(), session.SessionRowID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !reflect.DeepEqual(gotSession, session) {
		t.Fatalf("GetSession = %#v, want %#v", gotSession, session)
	}

	finding := validFinding(run.RunID, session.SessionRowID)
	insertFinding(t, store, finding)
	findings, err := store.ListFindings(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if !reflect.DeepEqual(findings, []Finding{finding}) {
		t.Fatalf("ListFindings = %#v, want %#v", findings, []Finding{finding})
	}

	action := validPlannedAction(run.RunID)
	insertPlannedAction(t, store, action)
	actions, err := store.ListPlannedActions(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("ListPlannedActions: %v", err)
	}
	if !reflect.DeepEqual(actions, []PlannedAction{action}) {
		t.Fatalf("ListPlannedActions = %#v, want %#v", actions, []PlannedAction{action})
	}

	named := validNamedSession()
	if err := store.UpsertNamedSession(context.Background(), named); err != nil {
		t.Fatalf("UpsertNamedSession: %v", err)
	}
	gotNamed, err := store.GetNamedSession(context.Background(), named.Name)
	if err != nil {
		t.Fatalf("GetNamedSession: %v", err)
	}
	if !reflect.DeepEqual(gotNamed, named) {
		t.Fatalf("GetNamedSession = %#v, want %#v", gotNamed, named)
	}
}

func TestInvalidInputsReturnErrInvalidInputBeforeMutation(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Store) error
	}{
		{name: "allocate empty pr key", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.PRKey = ""
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "allocate empty pr url", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.PRURL = ""
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "allocate empty sha", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.SHA = ""
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "allocate empty base sha", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.BaseSHA = ""
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "allocate empty profile", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.Profile = ""
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "allocate empty posting identity", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.PostingIdentity = ""
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "allocate zero started_at", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.StartedAt = time.Time{}
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "allocate empty artifact path", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.ArtifactPath = ""
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "allocate bad post mode", run: func(_ *testing.T, s *Store) error {
			params := validAllocateRunParams()
			params.PostMode = PostMode("preview")
			_, err := s.AllocateRun(context.Background(), params)
			return err
		}},
		{name: "session missing id", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			session.SessionRowID = ""
			return s.InsertSession(context.Background(), session)
		}},
		{name: "session missing run id", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			session.RunID = ""
			return s.InsertSession(context.Background(), session)
		}},
		{name: "session missing provider session id", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			session.ProviderSessionID = ""
			return s.InsertSession(context.Background(), session)
		}},
		{name: "session bad role", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			session.Role = SessionRole("manager")
			return s.InsertSession(context.Background(), session)
		}},
		{name: "session missing adapter", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			session.Adapter = ""
			return s.InsertSession(context.Background(), session)
		}},
		{name: "session missing model", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			session.Model = ""
			return s.InsertSession(context.Background(), session)
		}},
		{name: "session zero started_at", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			session.StartedAt = time.Time{}
			return s.InsertSession(context.Background(), session)
		}},
		{name: "finding missing id", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			insertSession(t, s, session)
			finding := validFinding(run.RunID, session.SessionRowID)
			finding.FindingID = ""
			return s.InsertFinding(context.Background(), finding)
		}},
		{name: "finding missing run id", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			insertSession(t, s, session)
			finding := validFinding(run.RunID, session.SessionRowID)
			finding.RunID = ""
			return s.InsertFinding(context.Background(), finding)
		}},
		{name: "finding missing session row id", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			insertSession(t, s, session)
			finding := validFinding(run.RunID, session.SessionRowID)
			finding.SessionRowID = ""
			return s.InsertFinding(context.Background(), finding)
		}},
		{name: "finding bad severity", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			insertSession(t, s, session)
			finding := validFinding(run.RunID, session.SessionRowID)
			finding.Severity = review.Severity("medium")
			return s.InsertFinding(context.Background(), finding)
		}},
		{name: "finding missing file path", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			insertSession(t, s, session)
			finding := validFinding(run.RunID, session.SessionRowID)
			finding.FilePath = ""
			return s.InsertFinding(context.Background(), finding)
		}},
		{name: "finding bad side", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			insertSession(t, s, session)
			finding := validFinding(run.RunID, session.SessionRowID)
			side := review.DiffSide("BOTH")
			finding.Side = &side
			return s.InsertFinding(context.Background(), finding)
		}},
		{name: "finding bad anchoring", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			insertSession(t, s, session)
			finding := validFinding(run.RunID, session.SessionRowID)
			finding.Anchoring = review.Anchoring("file")
			return s.InsertFinding(context.Background(), finding)
		}},
		{name: "finding missing body", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			session := validSession(run.RunID)
			insertSession(t, s, session)
			finding := validFinding(run.RunID, session.SessionRowID)
			finding.Body = ""
			return s.InsertFinding(context.Background(), finding)
		}},
		{name: "planned action missing action id", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			action := validPlannedAction(run.RunID)
			action.ActionID = ""
			return s.InsertPlannedAction(context.Background(), action)
		}},
		{name: "planned action missing run id", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			action := validPlannedAction(run.RunID)
			action.RunID = ""
			return s.InsertPlannedAction(context.Background(), action)
		}},
		{name: "planned action bad kind", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			action := validPlannedAction(run.RunID)
			action.Kind = PlannedActionKind("comment")
			return s.InsertPlannedAction(context.Background(), action)
		}},
		{name: "planned action zero planned_at", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			action := validPlannedAction(run.RunID)
			action.PlannedAt = time.Time{}
			return s.InsertPlannedAction(context.Background(), action)
		}},
		{name: "planned action missing payload", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			action := validPlannedAction(run.RunID)
			action.PayloadJSON = ""
			return s.InsertPlannedAction(context.Background(), action)
		}},
		{name: "planned action bad status", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			action := validPlannedAction(run.RunID)
			action.Status = PlannedActionStatus("done")
			return s.InsertPlannedAction(context.Background(), action)
		}},
		{name: "planned action negative attempts", run: func(t *testing.T, s *Store) error {
			run := allocateRun(t, s, validAllocateRunParams())
			action := validPlannedAction(run.RunID)
			action.Attempts = -1
			return s.InsertPlannedAction(context.Background(), action)
		}},
		{name: "named session missing name", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.Name = ""
			return s.UpsertNamedSession(context.Background(), named)
		}},
		{name: "named session missing profile", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.Profile = ""
			return s.UpsertNamedSession(context.Background(), named)
		}},
		{name: "named session missing provider", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.Provider = ""
			return s.UpsertNamedSession(context.Background(), named)
		}},
		{name: "named session missing adapter", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.Adapter = ""
			return s.UpsertNamedSession(context.Background(), named)
		}},
		{name: "named session missing model", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.Model = ""
			return s.UpsertNamedSession(context.Background(), named)
		}},
		{name: "named session missing host", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.Host = ""
			return s.UpsertNamedSession(context.Background(), named)
		}},
		{name: "named session missing provider session id", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.ProviderSessionID = ""
			return s.UpsertNamedSession(context.Background(), named)
		}},
		{name: "named session zero created_at", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.CreatedAt = time.Time{}
			return s.UpsertNamedSession(context.Background(), named)
		}},
		{name: "named session zero last_used_at", run: func(_ *testing.T, s *Store) error {
			named := validNamedSession()
			named.LastUsedAt = time.Time{}
			return s.UpsertNamedSession(context.Background(), named)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t)
			err := tt.run(t, store)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestStorageValueParsers(t *testing.T) {
	if got, err := ParseOutcome(" request_changes "); err != nil || got != OutcomeRequestChanges {
		t.Fatalf("ParseOutcome = %q, %v; want request_changes, nil", got, err)
	}
	if _, err := ParseOutcome("changes_requested"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ParseOutcome invalid error = %v, want ErrInvalidInput", err)
	}
	if got, err := ParsePlannedActionKind("ROLLUP_COMMENT"); err != nil || got != PlannedActionRollupComment {
		t.Fatalf("ParsePlannedActionKind = %q, %v; want rollup_comment, nil", got, err)
	}
}

func TestCloseStopsWriterAndRejectsMutation(t *testing.T) {
	store := openStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	_, err := store.AllocateRun(context.Background(), validAllocateRunParams())
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("AllocateRun after Close error = %v, want ErrClosed", err)
	}
}

func openStore(t *testing.T) *Store {
	t.Helper()

	return openStoreAt(t, filepath.Join(t.TempDir(), "ledger.db"))
}

func openStoreAt(t *testing.T, path string) *Store {
	t.Helper()

	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return store
}

func validAllocateRunParams() AllocateRunParams {
	return AllocateRunParams{
		PRKey:           "github_open-cli_codereview-cli_36",
		PRURL:           "https://example.test/pr/36",
		RunID:           "run-1",
		SHA:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Profile:         "default",
		PostingIdentity: "reviewer@example.com",
		PostMode:        PostModeLive,
		StartedAt:       time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
		ArtifactPath:    "/tmp/run-1",
	}
}

func allocateRun(t *testing.T, store *Store, params AllocateRunParams) Run {
	t.Helper()

	run, err := store.AllocateRun(context.Background(), params)
	if err != nil {
		t.Fatalf("AllocateRun: %v", err)
	}
	return run
}

func validSession(runID string) Session {
	completed := time.Date(2026, 5, 30, 12, 3, 0, 0, time.UTC)
	duration := int64(1200)
	tokensIn := int64(100)
	tokensOut := int64(20)
	cacheRead := int64(5)
	cacheCreate := int64(7)
	cost := 0.42

	return Session{
		SessionRowID:      "session-row-1",
		RunID:             runID,
		ProviderSessionID: "provider-session-1",
		Role:              SessionRoleReviewer,
		AgentID:           strPtr("harness:architecture"),
		Adapter:           "codex_cli",
		Model:             "gpt-5.5",
		Effort:            strPtr("xhigh"),
		StartedAt:         time.Date(2026, 5, 30, 12, 1, 0, 0, time.UTC),
		CompletedAt:       &completed,
		DurationMS:        &duration,
		TokensIn:          &tokensIn,
		TokensOut:         &tokensOut,
		CacheRead:         &cacheRead,
		CacheCreate:       &cacheCreate,
		CostUSD:           &cost,
	}
}

func insertSession(t *testing.T, store *Store, session Session) {
	t.Helper()
	if err := store.InsertSession(context.Background(), session); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
}

func validFinding(runID, sessionRowID string) Finding {
	side := review.DiffSideRight
	line := int64(42)
	diffPosition := int64(17)

	return Finding{
		FindingID:    review.FindingID("finding-1"),
		RunID:        runID,
		SessionRowID: sessionRowID,
		Severity:     review.SeverityMajor,
		FilePath:     "internal/ledger/ledger.go",
		Side:         &side,
		Line:         &line,
		DiffPosition: &diffPosition,
		Anchoring:    review.AnchoringInline,
		Body:         "finding body",
	}
}

func insertFinding(t *testing.T, store *Store, finding Finding) {
	t.Helper()
	if err := store.InsertFinding(context.Background(), finding); err != nil {
		t.Fatalf("InsertFinding: %v", err)
	}
}

func validPlannedAction(runID string) PlannedAction {
	attemptedAt := time.Date(2026, 5, 30, 12, 4, 0, 0, time.UTC)
	return PlannedAction{
		ActionID:    "action-1",
		RunID:       runID,
		Kind:        PlannedActionInlineComment,
		FindingID:   strPtr("finding-1"),
		ThreadID:    nil,
		PlannedAt:   time.Date(2026, 5, 30, 12, 2, 0, 0, time.UTC),
		PayloadJSON: `{"body":"hello"}`,
		Status:      PlannedActionPending,
		Required:    true,
		Attempts:    1,
		AttemptedAt: &attemptedAt,
		PostedAt:    nil,
		UpstreamID:  nil,
		Error:       strPtr("retry later"),
	}
}

func insertPlannedAction(t *testing.T, store *Store, action PlannedAction) {
	t.Helper()
	if err := store.InsertPlannedAction(context.Background(), action); err != nil {
		t.Fatalf("InsertPlannedAction: %v", err)
	}
}

func validNamedSession() NamedSession {
	return NamedSession{
		Name:              "daily",
		Profile:           "default",
		Provider:          "openai",
		Adapter:           "codex_cli",
		Model:             "gpt-5.5",
		Host:              "github.com",
		ProviderSessionID: "provider-session-1",
		CreatedAt:         time.Date(2026, 5, 30, 11, 0, 0, 0, time.UTC),
		LastUsedAt:        time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
	}
}

func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var got int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("query int %q: %v", query, err)
	}
	return got
}

func queryString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var got string
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("query string %q: %v", query, err)
	}
	return got
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	return queryInt(t, db, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name) == 1
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	return queryInt(t, db, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name) == 1
}

func strPtr(value string) *string {
	return &value
}
