package database

import (
	"context"
	"testing"
)

// The CI webhook's replay record (migration 0039) beyond the contract: a record is kept while a replay could still
// pass the signature's freshness check, removed after by the sweep and by its caller's next record, and only the
// pool sees the table. DATABASE_URL-gated.

// TestPurgeExpiredData_CIDeliveries: records older than an hour go; one eleven minutes old, a replay of which could
// still arrive under a clock five minutes ahead, stays. A purged pair can be recorded again.
func TestPurgeExpiredData_CIDeliveries(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	f := &r2Fixture{t: t, b: b, ctx: context.Background(), run: r2stamp()}
	caller := "credential:" + f.id("delivery")
	ages := map[string]string{"old": "61 minutes", "window": "11 minutes", "new": "0 minutes"}
	for label := range ages {
		if fresh, err := b.RecordCIDelivery(f.ctx, caller, "delivery-"+label+"-"+f.run); err != nil || !fresh {
			t.Fatalf("RecordCIDelivery %s = %v, %v", label, fresh, err)
		}
	}
	for label, age := range ages {
		f.backdateDelivery(caller, "delivery-"+label+"-"+f.run, age)
	}
	res, err := b.PurgeExpiredData(f.ctx, 0)
	if err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	if res.CIDeliveries != 1 {
		t.Errorf("purged %d delivery records, want 1", res.CIDeliveries)
	}
	for label := range ages {
		fresh, err := b.RecordCIDelivery(f.ctx, caller, "delivery-"+label+"-"+f.run)
		if err != nil {
			t.Fatal(err)
		}
		if want := label == "old"; fresh != want {
			t.Errorf("%s: recording it again is fresh=%v, want %v", label, fresh, want)
		}
	}
}

// TestRecordCIDelivery_RemovesTheCallersOldRecords: where no retention sweep runs (local+db without accounts) a key's
// record stays bounded, as each insert removes that caller's records past the hour, and never another caller's.
func TestRecordCIDelivery_RemovesTheCallersOldRecords(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	f := &r2Fixture{t: t, b: b, ctx: context.Background(), run: r2stamp()}
	mine, theirs, id := "secret", "token:"+f.id("other"), "delivery-aged-"+f.run
	for _, caller := range []string{mine, theirs} {
		if _, err := b.RecordCIDelivery(f.ctx, caller, id); err != nil {
			t.Fatal(err)
		}
		f.backdateDelivery(caller, id, "61 minutes")
	}
	if fresh, err := b.RecordCIDelivery(f.ctx, mine, "delivery-next-"+f.run); err != nil || !fresh {
		t.Fatalf("RecordCIDelivery = %v, %v", fresh, err)
	}
	s := &scopedFixture{r2Fixture: f}
	if n := s.rows("ci_webhook_deliveries", "caller = $1 AND delivery_id = $2", mine, id); n != 0 {
		t.Errorf("the caller's aged record survived its next record")
	}
	if n := s.rows("ci_webhook_deliveries", "caller = $1", theirs); n != 1 {
		t.Errorf("another caller's aged record went with this caller's insert (%d left)", n)
	}
}

// backdateDelivery sets a recorded delivery's age, an SQL interval.
func (f *r2Fixture) backdateDelivery(caller, id, age string) {
	f.t.Helper()
	if _, err := f.b.db.ExecContext(f.ctx, `UPDATE ci_webhook_deliveries SET received_at = NOW() - $1::interval
		WHERE caller = $2 AND delivery_id = $3`, age, caller, id); err != nil {
		f.t.Fatal(err)
	}
}

// TestRLS_CIDeliveries_PoolOnly: inside a user's request transaction the replay record is invisible and cannot be
// written; the webhook writes it on the pool, before any user is known.
func TestRLS_CIDeliveries_PoolOnly(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 39 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	skipIfRLSBypassedInternal(t, f.b)
	s := &scopedFixture{r2Fixture: f}
	user := f.user("delivery-rls")
	caller, id := "token:"+f.id("delivery-rls"), "delivery-rls-"+f.run
	if fresh, err := f.b.RecordCIDelivery(f.ctx, caller, id); err != nil || !fresh {
		t.Fatalf("pool RecordCIDelivery = %v, %v", fresh, err)
	}
	t.Cleanup(func() {
		_, _ = f.b.db.ExecContext(f.ctx, `DELETE FROM ci_webhook_deliveries WHERE caller = $1`, caller)
	})
	s.as(user, func(ctx context.Context) {
		var n int
		if err := f.b.query(ctx).QueryRowContext(ctx, `SELECT count(*) FROM ci_webhook_deliveries WHERE caller = $1`, caller).Scan(&n); err != nil || n != 0 {
			t.Errorf("a user's scope sees %d record(s) (%v), want none", n, err)
		}
	})
	s.as(user, func(ctx context.Context) {
		if fresh, err := f.b.RecordCIDelivery(ctx, caller, id+"-scoped"); err == nil || fresh {
			t.Errorf("a user's scope recorded a delivery: %v, %v", fresh, err)
		}
	})
	if n := s.rows("ci_webhook_deliveries", "caller = $1", caller); n != 1 {
		t.Errorf("%d record(s) on the pool, want the one written there", n)
	}
}
