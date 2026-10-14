package interfaces

import (
	"context"
	"encoding/json"
	"time"
)

// JobRun is the persisted state of one background job across replicas (lifecycle.Singleton, ADR 0023): written by
// the replica that runs the job, read by every replica that skips it, so "no successful run anywhere for too long"
// can be told apart from "another replica holds the lock", which is normal.
type JobRun struct {
	Name string
	// LastStartedAt is when the last run started, and Holder the instance that started it. Zero: never started.
	LastStartedAt time.Time
	Holder        string
	// LastSuccessAt is when the last successful run ended. Zero: never succeeded.
	LastSuccessAt time.Time
	// LastErrorAt and LastError are the last failed run's end and error; a later success leaves them as history.
	LastErrorAt time.Time
	LastError   string
	// Detail is the document the job last left with RecordJobDetail (for the legacy data migration, its result); nil
	// when it left none or cleared it. Valid JSON, as the store holds it (Postgres normalises the layout).
	Detail json.RawMessage
}

// JobRunStore keeps the background jobs' run state, one record per job name (the jobs' declared names, a closed
// set). The writes overwrite: the last run wins. Its first three methods are lifecycle.JobState, which the
// Singleton takes (that package cannot import this one).
type JobRunStore interface {
	// RecordJobStart records that holder started a run of the job at at.
	RecordJobStart(ctx context.Context, name, holder string, at time.Time) error
	// RecordJobEnd records the end of a run at at: a success when failure is "", else a failure with that error.
	RecordJobEnd(ctx context.Context, name string, at time.Time, failure string) error
	// JobLastSuccess returns when the job last succeeded on any replica; zero when it never did (or has no record).
	JobLastSuccess(ctx context.Context, name string) (time.Time, error)
	// GetJobRun returns the job's whole record; ErrNotFound when none was ever written.
	GetJobRun(ctx context.Context, name string) (*JobRun, error)
	// RecordJobDetail stores detail as the job's last-run document, replacing the previous one, and leaves the run
	// fields alone; nil or empty clears it. detail must be valid JSON. The write creates the record when there is none.
	RecordJobDetail(ctx context.Context, name string, detail json.RawMessage) error
}
