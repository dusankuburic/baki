package contract

import (
	"context"
	"errors"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// runJobRunScenarios holds the background jobs' run state (ADR 0023): one record per job name, the last start with
// its holder, the last success and the last failure with its error, each write overwriting its own fields only, so a
// later success keeps the failure as history. Times come back as stored (UTC, microseconds).
func runJobRunScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("JobRuns_record_start_success_failure", func(t *testing.T) {
		name := "contract_job_" + runID
		at := func(min int) time.Time {
			return time.Date(2026, 10, 10, 12, min, 30, 123456000, time.UTC)
		}
		if _, err := b.GetJobRun(ctx, name); !errors.Is(err, interfaces.ErrNotFound) {
			t.Fatalf("GetJobRun of a job never recorded = %v, want ErrNotFound", err)
		}
		if last, err := b.JobLastSuccess(ctx, name); err != nil || !last.IsZero() {
			t.Fatalf("JobLastSuccess of a job never recorded = %v, %v; want zero", last, err)
		}

		mustRecordJob(t, b.RecordJobStart(ctx, name, "replica-a", at(0)))
		got := mustJobRun(t, b, name)
		if !got.LastStartedAt.Equal(at(0)) || got.Holder != "replica-a" || !got.LastSuccessAt.IsZero() || !got.LastErrorAt.IsZero() {
			t.Fatalf("after a start: %+v", got)
		}

		mustRecordJob(t, b.RecordJobEnd(ctx, name, at(1), "boom"))
		mustRecordJob(t, b.RecordJobStart(ctx, name, "replica-b", at(2)))
		mustRecordJob(t, b.RecordJobEnd(ctx, name, at(3), ""))
		got = mustJobRun(t, b, name)
		switch {
		case !got.LastStartedAt.Equal(at(2)) || got.Holder != "replica-b":
			t.Errorf("last start = %v by %q, want %v by replica-b", got.LastStartedAt, got.Holder, at(2))
		case !got.LastSuccessAt.Equal(at(3)):
			t.Errorf("last success = %v, want %v", got.LastSuccessAt, at(3))
		case !got.LastErrorAt.Equal(at(1)) || got.LastError != "boom":
			t.Errorf("the earlier failure = %v %q, want it kept as history (%v boom)", got.LastErrorAt, got.LastError, at(1))
		}
		if last, err := b.JobLastSuccess(ctx, name); err != nil || !last.Equal(at(3)) {
			t.Errorf("JobLastSuccess = %v, %v; want %v", last, err, at(3))
		}

		// The last write wins, also an earlier time (the replicas' clocks are not one clock).
		mustRecordJob(t, b.RecordJobEnd(ctx, name, at(2), ""))
		if last, _ := b.JobLastSuccess(ctx, name); !last.Equal(at(2)) {
			t.Errorf("JobLastSuccess after a later write of an earlier time = %v, want %v", last, at(2))
		}
	})

	run("JobRuns_end_without_start_and_per_name", func(t *testing.T) {
		a, other := "contract_job_a_"+runID, "contract_job_b_"+runID
		mustRecordJob(t, b.RecordJobEnd(ctx, a, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), ""))
		got := mustJobRun(t, b, a)
		if got.LastSuccessAt.IsZero() || !got.LastStartedAt.IsZero() || got.Holder != "" {
			t.Errorf("an end without a recorded start: %+v", got)
		}
		if _, err := b.GetJobRun(ctx, other); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("another job's record = %v, want ErrNotFound", err)
		}
	})
}

func mustRecordJob(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustJobRun(t *testing.T, b interfaces.StorageBackend, name string) *interfaces.JobRun {
	t.Helper()
	r, err := b.GetJobRun(context.Background(), name)
	if err != nil {
		t.Fatalf("GetJobRun(%s): %v", name, err)
	}
	if r.Name != name {
		t.Fatalf("GetJobRun(%s) returned the record of %q", name, r.Name)
	}
	return r
}
