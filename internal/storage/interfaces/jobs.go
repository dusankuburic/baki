package interfaces

import (
	"context"
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
}
