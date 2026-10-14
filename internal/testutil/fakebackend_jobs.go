package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// fakeJobRuns backs the background jobs' run state (job_runs, migration 0041): one record per job name, the writes
// overwriting like the Postgres upserts. Its own lock: jobs record from their own goroutines.
type fakeJobRuns struct {
	mu   sync.Mutex
	runs map[string]*interfaces.JobRun
	// err, when set, fails every call (SetJobRunsError), as an unreachable database does.
	err error
}

// SetJobRunsError makes every job-state call fail with err (nil restores them).
func (m *FakeBackend) SetJobRunsError(err error) {
	m.jobRuns.mu.Lock()
	m.jobRuns.err = err
	m.jobRuns.mu.Unlock()
}

// record returns the job's record, created empty when absent. The caller holds j.mu.
func (j *fakeJobRuns) record(name string) *interfaces.JobRun {
	if j.runs == nil {
		j.runs = map[string]*interfaces.JobRun{}
	}
	r, ok := j.runs[name]
	if !ok {
		r = &interfaces.JobRun{Name: name}
		j.runs[name] = r
	}
	return r
}

// RecordJobStart mirrors the upsert of last_started_at and holder_instance.
func (m *FakeBackend) RecordJobStart(_ context.Context, name, holder string, at time.Time) error {
	j := &m.jobRuns
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return j.err
	}
	r := j.record(name)
	r.LastStartedAt, r.Holder = pgTime(at), holder
	return nil
}

// RecordJobEnd mirrors the upsert of last_success_at, or of last_error_at and last_error.
func (m *FakeBackend) RecordJobEnd(_ context.Context, name string, at time.Time, failure string) error {
	j := &m.jobRuns
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return j.err
	}
	r := j.record(name)
	if failure == "" {
		r.LastSuccessAt = pgTime(at)
	} else {
		r.LastErrorAt, r.LastError = pgTime(at), failure
	}
	return nil
}

// JobLastSuccess returns the record's last success; zero without one.
func (m *FakeBackend) JobLastSuccess(_ context.Context, name string) (time.Time, error) {
	j := &m.jobRuns
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return time.Time{}, j.err
	}
	if r, ok := j.runs[name]; ok {
		return r.LastSuccessAt, nil
	}
	return time.Time{}, nil
}

// GetJobRun returns a copy of the record; ErrNotFound without one.
func (m *FakeBackend) GetJobRun(_ context.Context, name string) (*interfaces.JobRun, error) {
	j := &m.jobRuns
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return nil, j.err
	}
	r, ok := j.runs[name]
	if !ok {
		return nil, interfaces.ErrNotFound
	}
	out := *r
	out.Detail = append(json.RawMessage(nil), r.Detail...)
	if len(r.Detail) == 0 {
		out.Detail = nil
	}
	return &out, nil
}

// pgTime is what a timestamptz column gives back: UTC, at microsecond precision. The zero time stays zero (NULL).
func pgTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

// RecordJobDetail mirrors the upsert of last_detail: a document that is not JSON is refused as the jsonb column
// refuses it, and nil or empty clears it.
func (m *FakeBackend) RecordJobDetail(_ context.Context, name string, detail json.RawMessage) error {
	j := &m.jobRuns
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return j.err
	}
	if len(detail) > 0 && !json.Valid(detail) {
		return fmt.Errorf("record job detail: invalid JSON")
	}
	r := j.record(name)
	r.Detail = append(json.RawMessage(nil), detail...)
	if len(detail) == 0 {
		r.Detail = nil
	}
	return nil
}
