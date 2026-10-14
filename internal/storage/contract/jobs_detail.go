package contract

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// runJobDetailScenarios holds the job's last-run detail (job_runs.last_detail, migration 0046): a JSON document a job
// leaves for the replicas that did not run it, written by RecordJobDetail alone, and read back by GetJobRun.
func runJobDetailScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	sameJSON := func(t *testing.T, got json.RawMessage, want string) {
		t.Helper()
		var g, w any
		if err := json.Unmarshal(got, &g); err != nil {
			t.Fatalf("detail %q is not JSON: %v", got, err)
		}
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("detail = %s, want %s", got, want)
		}
	}

	run("JobRuns_detail_round_trip_overwrite_clear", func(t *testing.T) {
		name := "contract_job_detail_" + runID
		at := time.Date(2026, 10, 10, 13, 5, 30, 0, time.UTC)

		// No record yet: the write creates it, with no other field set.
		mustRecordJob(t, b.RecordJobDetail(ctx, name, json.RawMessage(`{"flowsMigrated":2,"notMigrated":["users"]}`)))
		got := mustJobRun(t, b, name)
		sameJSON(t, got.Detail, `{"flowsMigrated":2,"notMigrated":["users"]}`)
		if !got.LastStartedAt.IsZero() || !got.LastSuccessAt.IsZero() || !got.LastErrorAt.IsZero() || got.Holder != "" {
			t.Errorf("a detail alone set run fields: %+v", got)
		}

		// The run fields and the detail do not touch each other.
		mustRecordJob(t, b.RecordJobStart(ctx, name, "replica-a", at))
		mustRecordJob(t, b.RecordJobEnd(ctx, name, at.Add(time.Minute), ""))
		got = mustJobRun(t, b, name)
		sameJSON(t, got.Detail, `{"flowsMigrated":2,"notMigrated":["users"]}`)
		if !got.LastStartedAt.Equal(at) || got.Holder != "replica-a" || !got.LastSuccessAt.Equal(at.Add(time.Minute)) {
			t.Errorf("a detail changed the run fields: %+v", got)
		}
		mustRecordJob(t, b.RecordJobDetail(ctx, name, json.RawMessage(`[1, "two", {"three": 3}]`)))
		got = mustJobRun(t, b, name)
		sameJSON(t, got.Detail, `[1,"two",{"three":3}]`)
		if !got.LastStartedAt.Equal(at) || got.Holder != "replica-a" || !got.LastSuccessAt.Equal(at.Add(time.Minute)) {
			t.Errorf("a second detail changed the run fields: %+v", got)
		}

		// nil clears it; the run fields stay.
		mustRecordJob(t, b.RecordJobDetail(ctx, name, nil))
		got = mustJobRun(t, b, name)
		if len(got.Detail) != 0 {
			t.Errorf("detail after a clear = %s, want none", got.Detail)
		}
		if !got.LastStartedAt.Equal(at) {
			t.Errorf("a clear changed the run fields: %+v", got)
		}
	})

	run("JobRuns_detail_rejects_invalid_json_and_is_per_name", func(t *testing.T) {
		a, other := "contract_job_detail_a_"+runID, "contract_job_detail_b_"+runID
		mustRecordJob(t, b.RecordJobDetail(ctx, a, json.RawMessage(`{"ok":true}`)))
		if err := b.RecordJobDetail(ctx, a, json.RawMessage(`{not json`)); err == nil {
			t.Error("a detail that is not JSON was accepted")
		}
		sameJSON(t, mustJobRun(t, b, a).Detail, `{"ok":true}`)
		if _, err := b.GetJobRun(ctx, other); err == nil {
			t.Error("another job's record appeared")
		}
	})
}
