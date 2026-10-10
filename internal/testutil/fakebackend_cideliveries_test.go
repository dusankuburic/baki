package testutil

import (
	"context"
	"testing"
	"time"
)

// The retention sweep removes delivery records older than an hour, as the Postgres sweep does; a purged pair can be
// recorded again. A record also removes its own caller's old records, never another caller's.
func TestFakeBackend_CIDeliveryPurge(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	ages := map[string]time.Duration{"delivery-old-0123": 61 * time.Minute, "delivery-window-0": 11 * time.Minute, "delivery-new-0123": 0}
	for id := range ages {
		if fresh, err := b.RecordCIDelivery(ctx, "secret", id); err != nil || !fresh {
			t.Fatalf("RecordCIDelivery(%s) = %v, %v", id, fresh, err)
		}
	}
	for id, age := range ages {
		b.SetCIDeliveryReceived("secret", id, time.Now().Add(-age))
	}
	res, err := b.PurgeExpiredData(ctx, 0)
	if err != nil || res.CIDeliveries != 1 {
		t.Fatalf("PurgeExpiredData = %+v, %v; want 1 delivery record removed", res, err)
	}
	for id := range ages {
		fresh, err := b.RecordCIDelivery(ctx, "secret", id)
		if err != nil {
			t.Fatal(err)
		}
		if want := id == "delivery-old-0123"; fresh != want {
			t.Errorf("%s: recording it again is fresh=%v, want %v", id, fresh, want)
		}
	}

	for _, caller := range []string{"token:a", "token:b"} {
		if _, err := b.RecordCIDelivery(ctx, caller, "delivery-aged-0123"); err != nil {
			t.Fatal(err)
		}
		b.SetCIDeliveryReceived(caller, "delivery-aged-0123", time.Now().Add(-61*time.Minute))
	}
	if _, err := b.RecordCIDelivery(ctx, "token:a", "delivery-next-0123"); err != nil {
		t.Fatal(err)
	}
	if res, _ := b.PurgeExpiredData(ctx, 0); res.CIDeliveries != 1 {
		t.Errorf("the sweep found %d aged records, want only token:b's (token:a's went with its next record)", res.CIDeliveries)
	}
}
