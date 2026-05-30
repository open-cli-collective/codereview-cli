// Package dbmig applies forward-only SQLite schema migrations.
package dbmig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrInvalidPlan means the compiled migration list is internally
	// inconsistent and should be fixed in code before opening user data.
	ErrInvalidPlan = errors.New("dbmig: invalid migration plan")
	// ErrInvalidMeta means durable schema metadata is malformed; callers should
	// fail loudly rather than trying to repair it implicitly.
	ErrInvalidMeta = errors.New("dbmig: invalid meta state")
	// ErrDowngrade means the database schema is newer than this binary knows how
	// to operate.
	ErrDowngrade = errors.New("dbmig: database schema newer than code")
)

// Migration is one forward-only schema migration.
type Migration struct {
	Version int
	Name    string
	Up      func(context.Context, *sql.Tx) error
}

// AppliedMigration records a migration that ran during Apply.
type AppliedMigration struct {
	Version int
	Name    string
}

// Result summarizes one Apply invocation.
type Result struct {
	PreviousVersion int
	CurrentVersion  int
	Applied         []AppliedMigration
}

// Apply ensures meta exists, refuses downgrades, and applies missing
// migrations in version order.
func Apply(ctx context.Context, db *sql.DB, migrations []Migration) (Result, error) {
	if err := validatePlan(migrations); err != nil {
		return Result{}, err
	}

	current, err := ensureMeta(ctx, db)
	if err != nil {
		return Result{}, err
	}

	target := len(migrations)
	result := Result{
		PreviousVersion: current,
		CurrentVersion:  current,
	}

	if current > target {
		return result, fmt.Errorf("%w: database version %d, code version %d", ErrDowngrade, current, target)
	}
	if current == target {
		return result, nil
	}

	for _, migration := range migrations {
		if migration.Version <= current {
			continue
		}
		if err := applyMigration(ctx, db, migration); err != nil {
			return result, err
		}
		current = migration.Version
		result.CurrentVersion = current
		result.Applied = append(result.Applied, AppliedMigration{
			Version: migration.Version,
			Name:    migration.Name,
		})
	}

	return result, nil
}

func validatePlan(migrations []Migration) error {
	for index, migration := range migrations {
		expectedVersion := index + 1
		if migration.Version != expectedVersion {
			return fmt.Errorf("%w: migration %d has version %d, want %d", ErrInvalidPlan, index, migration.Version, expectedVersion)
		}
		if strings.TrimSpace(migration.Name) == "" {
			return fmt.Errorf("%w: migration %d has empty name", ErrInvalidPlan, migration.Version)
		}
		if migration.Up == nil {
			return fmt.Errorf("%w: migration %d has nil Up", ErrInvalidPlan, migration.Version)
		}
	}
	return nil
}

func ensureMeta(ctx context.Context, db *sql.DB) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("dbmig: begin meta transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (
schema_version INTEGER NOT NULL,
created_at     TEXT NOT NULL
)`); err != nil {
		return 0, fmt.Errorf("%w: creating meta table: %w", ErrInvalidMeta, err)
	}

	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM meta").Scan(&count); err != nil {
		return 0, fmt.Errorf("%w: counting meta rows: %w", ErrInvalidMeta, err)
	}

	var version int
	switch count {
	case 0:
		version = 0
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO meta (schema_version, created_at) VALUES (?, ?)",
			version,
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return 0, fmt.Errorf("%w: inserting initial meta row: %w", ErrInvalidMeta, err)
		}
	case 1:
		if err := tx.QueryRowContext(ctx, "SELECT schema_version FROM meta").Scan(&version); err != nil {
			return 0, fmt.Errorf("%w: reading schema_version: %w", ErrInvalidMeta, err)
		}
	default:
		return 0, fmt.Errorf("%w: expected one meta row, found %d", ErrInvalidMeta, count)
	}

	if version < 0 {
		return 0, fmt.Errorf("%w: negative schema_version %d", ErrInvalidMeta, version)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("dbmig: commit meta transaction: %w", err)
	}
	return version, nil
}

func applyMigration(ctx context.Context, db *sql.DB, migration Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dbmig: begin migration %d %q: %w", migration.Version, migration.Name, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := migration.Up(ctx, tx); err != nil {
		return fmt.Errorf("dbmig: apply migration %d %q: %w", migration.Version, migration.Name, err)
	}

	result, err := tx.ExecContext(ctx, "UPDATE meta SET schema_version = ?", migration.Version)
	if err != nil {
		return fmt.Errorf("%w: updating schema_version for migration %d: %w", ErrInvalidMeta, migration.Version, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("dbmig: checking schema_version update for migration %d: %w", migration.Version, err)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("%w: schema_version update affected %d rows", ErrInvalidMeta, rowsAffected)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dbmig: commit migration %d %q: %w", migration.Version, migration.Name, err)
	}
	return nil
}
