package migration

import (
	"context"
	"encoding/json"

	"pad-analyzer/internal/storage/interfaces"
)

// DetailMax bounds the result document kept in the job state (job_runs.last_detail). A run that failed many flows
// lists each failure; the document keeps the counts and the first of the errors.
const DetailMax = 64 << 10

// RunStore is the part of the job state a migration run is kept in: the storage backend's JobRunStore.
type RunStore interface {
	GetJobRun(ctx context.Context, name string) (*interfaces.JobRun, error)
	RecordJobDetail(ctx context.Context, name string, detail json.RawMessage) error
}

// RunState keeps the result of the migration run in the job state under the job's name, so every replica can answer
// for a run on any of them (the run's start, end and error are the Singleton's records).
type RunState struct {
	store RunStore
	job   string
}

// NewRunState returns the state of the job named job in store.
func NewRunState(store RunStore, job string) *RunState { return &RunState{store: store, job: job} }

// Load returns the job's record; interfaces.ErrNotFound when no run was ever recorded.
func (s *RunState) Load(ctx context.Context) (*interfaces.JobRun, error) {
	return s.store.GetJobRun(ctx, s.job)
}

// Clear drops the previous run's result, when a new run starts.
func (s *RunState) Clear(ctx context.Context) error { return s.store.RecordJobDetail(ctx, s.job, nil) }

// Save records res as the latest run's result.
func (s *RunState) Save(ctx context.Context, res Result) error {
	return s.store.RecordJobDetail(ctx, s.job, Detail(res))
}

// Detail is res as the document kept for the other replicas: its JSON, with the list of errors cut to the first ones
// when the whole is over DetailMax. The counts stay exact.
func Detail(res Result) json.RawMessage {
	doc, err := json.Marshal(res)
	if err != nil {
		return nil
	}
	for keep := len(res.Errors); len(doc) > DetailMax && keep > 0; {
		keep /= 2
		res.Errors = res.Errors[:keep]
		if doc, err = json.Marshal(res); err != nil {
			return nil
		}
	}
	return doc
}

// ParseDetail reads a document Detail wrote.
func ParseDetail(doc json.RawMessage) (Result, error) {
	var res Result
	err := json.Unmarshal(doc, &res)
	return res, err
}
