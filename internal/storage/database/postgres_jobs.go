package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/storage/interfaces"
)

// The background jobs' run state (job_runs, migration 0041, ADR 0023). Written and read by the jobs on the pool,
// never inside a request, so the table has no row-level security (tablesWithoutRLS).

// The Singleton takes the role as its JobState.
var _ lifecycle.JobState = interfaces.JobRunStore(nil)

// jobRuns is the store on a pool: the backend's methods, and the jobs that only have the pool (the token-blacklist
// cleanup), share it.
type jobRuns struct{ db *sql.DB }

func (j jobRuns) RecordJobStart(ctx context.Context, name, holder string, at time.Time) error {
	if _, err := j.db.ExecContext(ctx, `
		INSERT INTO job_runs (name, last_started_at, holder_instance) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET last_started_at = EXCLUDED.last_started_at, holder_instance = EXCLUDED.holder_instance`,
		name, at.UTC(), holder); err != nil {
		return fmt.Errorf("record job start: %w", err)
	}
	return nil
}

func (j jobRuns) RecordJobEnd(ctx context.Context, name string, at time.Time, failure string) error {
	var err error
	if failure == "" {
		_, err = j.db.ExecContext(ctx, `
			INSERT INTO job_runs (name, last_success_at) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET last_success_at = EXCLUDED.last_success_at`, name, at.UTC())
	} else {
		_, err = j.db.ExecContext(ctx, `
			INSERT INTO job_runs (name, last_error_at, last_error) VALUES ($1, $2, $3)
			ON CONFLICT (name) DO UPDATE SET last_error_at = EXCLUDED.last_error_at, last_error = EXCLUDED.last_error`,
			name, at.UTC(), failure)
	}
	if err != nil {
		return fmt.Errorf("record job end: %w", err)
	}
	return nil
}

func (j jobRuns) JobLastSuccess(ctx context.Context, name string) (time.Time, error) {
	var at sql.NullTime
	err := j.db.QueryRowContext(ctx, `SELECT last_success_at FROM job_runs WHERE name = $1`, name).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, fmt.Errorf("read job last success: %w", err)
	}
	return utcOrZero(at), nil
}

func (j jobRuns) GetJobRun(ctx context.Context, name string) (*interfaces.JobRun, error) {
	var started, success, failed sql.NullTime
	var detail []byte
	r := interfaces.JobRun{Name: name}
	err := j.db.QueryRowContext(ctx, `
		SELECT last_started_at, holder_instance, last_success_at, last_error_at, last_error, last_detail
		  FROM job_runs WHERE name = $1`, name).Scan(&started, &r.Holder, &success, &failed, &r.LastError, &detail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, interfaces.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("read job run: %w", err)
	}
	r.LastStartedAt, r.LastSuccessAt, r.LastErrorAt = utcOrZero(started), utcOrZero(success), utcOrZero(failed)
	if len(detail) > 0 {
		r.Detail = json.RawMessage(detail)
	}
	return &r, nil
}

func (j jobRuns) RecordJobDetail(ctx context.Context, name string, detail json.RawMessage) error {
	var doc any // NULL clears
	if len(detail) > 0 {
		doc = string(detail)
	}
	if _, err := j.db.ExecContext(ctx, `
		INSERT INTO job_runs (name, last_detail) VALUES ($1, $2::jsonb)
		ON CONFLICT (name) DO UPDATE SET last_detail = EXCLUDED.last_detail`, name, doc); err != nil {
		return fmt.Errorf("record job detail: %w", err)
	}
	return nil
}

func utcOrZero(t sql.NullTime) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

// RecordJobStart implements interfaces.JobRunStore.
func (b *PostgresStorageBackend) RecordJobStart(ctx context.Context, name, holder string, at time.Time) error {
	return jobRuns{b.db}.RecordJobStart(ctx, name, holder, at)
}

// RecordJobEnd implements interfaces.JobRunStore.
func (b *PostgresStorageBackend) RecordJobEnd(ctx context.Context, name string, at time.Time, failure string) error {
	return jobRuns{b.db}.RecordJobEnd(ctx, name, at, failure)
}

// JobLastSuccess implements interfaces.JobRunStore.
func (b *PostgresStorageBackend) JobLastSuccess(ctx context.Context, name string) (time.Time, error) {
	return jobRuns{b.db}.JobLastSuccess(ctx, name)
}

// GetJobRun implements interfaces.JobRunStore.
func (b *PostgresStorageBackend) GetJobRun(ctx context.Context, name string) (*interfaces.JobRun, error) {
	return jobRuns{b.db}.GetJobRun(ctx, name)
}

// RecordJobDetail implements interfaces.JobRunStore.
func (b *PostgresStorageBackend) RecordJobDetail(ctx context.Context, name string, detail json.RawMessage) error {
	return jobRuns{b.db}.RecordJobDetail(ctx, name, detail)
}
