package dbmig

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestApplyFreshDBAppliesMigrationsAndRecordsVersion(t *testing.T) {
	db := openTestDB(t)

	result, err := Apply(context.Background(), db, []Migration{
		sqlMigration(1, "create widgets", "CREATE TABLE widgets (id INTEGER PRIMARY KEY)"),
		sqlMigration(2, "create reviews", "CREATE TABLE reviews (id INTEGER PRIMARY KEY)"),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	wantApplied := []AppliedMigration{
		{Version: 1, Name: "create widgets"},
		{Version: 2, Name: "create reviews"},
	}
	if result.PreviousVersion != 0 {
		t.Fatalf("PreviousVersion = %d, want 0", result.PreviousVersion)
	}
	if result.CurrentVersion != 2 {
		t.Fatalf("CurrentVersion = %d, want 2", result.CurrentVersion)
	}
	if !reflect.DeepEqual(result.Applied, wantApplied) {
		t.Fatalf("Applied = %#v, want %#v", result.Applied, wantApplied)
	}
	if version := currentSchemaVersion(t, db); version != 2 {
		t.Fatalf("schema_version = %d, want 2", version)
	}
	if !tableExists(t, db, "widgets") {
		t.Fatal("widgets table does not exist")
	}
	if !tableExists(t, db, "reviews") {
		t.Fatal("reviews table does not exist")
	}

	createdAt := metaCreatedAt(t, db)
	if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
		t.Fatalf("created_at = %q, want RFC3339Nano: %v", createdAt, err)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	appliedCount := 0
	migrations := []Migration{
		countedMigration(1, "create widgets", &appliedCount, "CREATE TABLE widgets (id INTEGER PRIMARY KEY)"),
		countedMigration(2, "create reviews", &appliedCount, "CREATE TABLE reviews (id INTEGER PRIMARY KEY)"),
	}

	if _, err := Apply(context.Background(), db, migrations); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	result, err := Apply(context.Background(), db, migrations)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	if appliedCount != 2 {
		t.Fatalf("migration function calls = %d, want 2", appliedCount)
	}
	if result.PreviousVersion != 2 {
		t.Fatalf("PreviousVersion = %d, want 2", result.PreviousVersion)
	}
	if result.CurrentVersion != 2 {
		t.Fatalf("CurrentVersion = %d, want 2", result.CurrentVersion)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("Applied = %#v, want none", result.Applied)
	}
}

func TestApplyRefusesDatabaseNewerThanCode(t *testing.T) {
	db := openTestDB(t)
	seedMeta(t, db, 2)

	_, err := Apply(context.Background(), db, []Migration{
		sqlMigration(1, "create widgets", "CREATE TABLE widgets (id INTEGER PRIMARY KEY)"),
	})
	if !errors.Is(err, ErrDowngrade) {
		t.Fatalf("Apply error = %v, want ErrDowngrade", err)
	}
	if version := currentSchemaVersion(t, db); version != 2 {
		t.Fatalf("schema_version = %d, want 2", version)
	}
	if tableExists(t, db, "widgets") {
		t.Fatal("widgets table exists after downgrade refusal")
	}
}

func TestApplyRollsBackFailedMigration(t *testing.T) {
	db := openTestDB(t)
	boom := errors.New("boom")

	_, err := Apply(context.Background(), db, []Migration{
		sqlMigration(1, "create widgets", "CREATE TABLE widgets (id INTEGER PRIMARY KEY)"),
		{
			Version: 2,
			Name:    "fails after DDL",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, "CREATE TABLE transient (id INTEGER PRIMARY KEY)"); err != nil {
					return err
				}
				return boom
			},
		},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Apply error = %v, want boom", err)
	}
	if version := currentSchemaVersion(t, db); version != 1 {
		t.Fatalf("schema_version = %d, want 1", version)
	}
	if !tableExists(t, db, "widgets") {
		t.Fatal("widgets table does not exist after committed migration")
	}
	if tableExists(t, db, "transient") {
		t.Fatal("transient table exists after failed migration rollback")
	}
}

func TestApplyRejectsInvalidPlansBeforeMutation(t *testing.T) {
	tests := []struct {
		name       string
		migrations []Migration
	}{
		{
			name: "duplicate version",
			migrations: []Migration{
				noopMigration(1, "one"),
				noopMigration(1, "duplicate"),
			},
		},
		{
			name: "out of order",
			migrations: []Migration{
				noopMigration(2, "two"),
				noopMigration(1, "one"),
			},
		},
		{
			name: "gap",
			migrations: []Migration{
				noopMigration(1, "one"),
				noopMigration(3, "three"),
			},
		},
		{
			name: "zero version",
			migrations: []Migration{
				noopMigration(0, "zero"),
			},
		},
		{
			name: "negative version",
			migrations: []Migration{
				noopMigration(-1, "negative"),
			},
		},
		{
			name: "empty name",
			migrations: []Migration{
				noopMigration(1, ""),
			},
		},
		{
			name: "nil up",
			migrations: []Migration{
				{Version: 1, Name: "nil up"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			_, err := Apply(context.Background(), db, tt.migrations)
			if !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("Apply error = %v, want ErrInvalidPlan", err)
			}
			if tableExists(t, db, "meta") {
				t.Fatal("meta table exists after invalid plan")
			}
		})
	}
}

func TestApplyRejectsMalformedMeta(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *sql.DB)
	}{
		{
			name: "multiple rows",
			setup: func(t *testing.T, db *sql.DB) {
				createMeta(t, db)
				insertMeta(t, db, 0)
				insertMeta(t, db, 0)
			},
		},
		{
			name: "negative version",
			setup: func(t *testing.T, db *sql.DB) {
				seedMeta(t, db, -1)
			},
		},
		{
			name: "missing created_at column",
			setup: func(t *testing.T, db *sql.DB) {
				execSQL(t, db, "CREATE TABLE meta (schema_version INTEGER NOT NULL)")
				execSQL(t, db, "INSERT INTO meta (schema_version) VALUES (?)", 0)
			},
		},
		{
			name: "nullable created_at column",
			setup: func(t *testing.T, db *sql.DB) {
				execSQL(t, db, "CREATE TABLE meta (schema_version INTEGER NOT NULL, created_at TEXT)")
				execSQL(t, db, "INSERT INTO meta (schema_version, created_at) VALUES (?, ?)", 0, time.Now().UTC().Format(time.RFC3339Nano))
			},
		},
		{
			name: "wrong schema_version type",
			setup: func(t *testing.T, db *sql.DB) {
				execSQL(t, db, "CREATE TABLE meta (schema_version TEXT NOT NULL, created_at TEXT NOT NULL)")
				execSQL(t, db, "INSERT INTO meta (schema_version, created_at) VALUES (?, ?)", "0", time.Now().UTC().Format(time.RFC3339Nano))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			tt.setup(t, db)

			_, err := Apply(context.Background(), db, []Migration{
				sqlMigration(1, "create widgets", "CREATE TABLE widgets (id INTEGER PRIMARY KEY)"),
			})
			if !errors.Is(err, ErrInvalidMeta) {
				t.Fatalf("Apply error = %v, want ErrInvalidMeta", err)
			}
			if tableExists(t, db, "widgets") {
				t.Fatal("widgets table exists after malformed meta refusal")
			}
		})
	}
}

func TestApplyEmptyMigrationSet(t *testing.T) {
	t.Run("fresh database initializes version zero", func(t *testing.T) {
		db := openTestDB(t)

		result, err := Apply(context.Background(), db, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if result.PreviousVersion != 0 {
			t.Fatalf("PreviousVersion = %d, want 0", result.PreviousVersion)
		}
		if result.CurrentVersion != 0 {
			t.Fatalf("CurrentVersion = %d, want 0", result.CurrentVersion)
		}
		if len(result.Applied) != 0 {
			t.Fatalf("Applied = %#v, want none", result.Applied)
		}
		if version := currentSchemaVersion(t, db); version != 0 {
			t.Fatalf("schema_version = %d, want 0", version)
		}
	})

	t.Run("existing database newer than empty code refuses downgrade", func(t *testing.T) {
		db := openTestDB(t)
		seedMeta(t, db, 1)

		_, err := Apply(context.Background(), db, nil)
		if !errors.Is(err, ErrDowngrade) {
			t.Fatalf("Apply error = %v, want ErrDowngrade", err)
		}
		if version := currentSchemaVersion(t, db); version != 1 {
			t.Fatalf("schema_version = %d, want 1", version)
		}
	})
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close: %v", err)
		}
	})
	return db
}

func sqlMigration(version int, name string, statements ...string) Migration {
	return Migration{
		Version: version,
		Name:    name,
		Up: func(ctx context.Context, tx *sql.Tx) error {
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func countedMigration(version int, name string, count *int, statements ...string) Migration {
	migration := sqlMigration(version, name, statements...)
	migration.Up = func(ctx context.Context, tx *sql.Tx) error {
		(*count)++
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}
	return migration
}

func noopMigration(version int, name string) Migration {
	return Migration{
		Version: version,
		Name:    name,
		Up: func(context.Context, *sql.Tx) error {
			return nil
		},
	}
}

func createMeta(t *testing.T, db *sql.DB) {
	t.Helper()

	execSQL(t, db, "CREATE TABLE meta (schema_version INTEGER NOT NULL, created_at TEXT NOT NULL)")
}

func seedMeta(t *testing.T, db *sql.DB, version int) {
	t.Helper()

	createMeta(t, db)
	insertMeta(t, db, version)
}

func insertMeta(t *testing.T, db *sql.DB, version int) {
	t.Helper()

	execSQL(t, db, "INSERT INTO meta (schema_version, created_at) VALUES (?, ?)", version, time.Now().UTC().Format(time.RFC3339Nano))
}

func currentSchemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()

	var version int
	if err := db.QueryRowContext(context.Background(), "SELECT schema_version FROM meta").Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	return version
}

func metaCreatedAt(t *testing.T, db *sql.DB) string {
	t.Helper()

	var createdAt string
	if err := db.QueryRowContext(context.Background(), "SELECT created_at FROM meta").Scan(&createdAt); err != nil {
		t.Fatalf("query created_at: %v", err)
	}
	return createdAt
}

func tableExists(t *testing.T, db *sql.DB, tableName string) bool {
	t.Helper()

	var count int
	err := db.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
		tableName,
	).Scan(&count)
	if err != nil {
		t.Fatalf("query sqlite_master for %s: %v", tableName, err)
	}
	return count > 0
}

func execSQL(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}
