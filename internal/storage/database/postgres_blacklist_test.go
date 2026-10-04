package database_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pad-analyzer/internal/storage/database"
)

// TestPostgresBlacklist_LoadActiveAndIsRevokedCtx pins the two reads
// auth.CachedBlacklist is built on: LoadActive returns exactly the unexpired
// entries with their expiry, IsRevokedCtx answers on the caller's context and
// returns (not swallows) an error.
func TestPostgresBlacklist_LoadActiveAndIsRevokedCtx(t *testing.T) {
	b := openTestDB(t)
	bl := database.NewPostgresBlacklist(b.DB())
	t.Cleanup(bl.Stop)
	ctx := context.Background()

	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	live, expired := "bl-live-"+stamp, "bl-expired-"+stamp
	t.Cleanup(func() {
		_, _ = b.DB().ExecContext(ctx, `DELETE FROM token_blacklist WHERE jti IN ($1, $2)`, live, expired)
	})

	before := time.Now()
	bl.Add(live, time.Minute)
	bl.Add(expired, -time.Minute)

	active, err := bl.LoadActive(ctx)
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	exp, ok := active[live]
	if !ok {
		t.Fatalf("LoadActive is missing the live entry %q", live)
	}
	if exp.Before(before.Add(time.Minute-5*time.Second)) || exp.After(time.Now().Add(time.Minute+5*time.Second)) {
		t.Errorf("live entry expiry %v is not ~1 minute from now", exp)
	}
	if _, ok := active[expired]; ok {
		t.Errorf("LoadActive returned the expired entry %q", expired)
	}

	for jti, want := range map[string]bool{live: true, expired: false, "bl-unknown-" + stamp: false} {
		got, err := bl.IsRevokedCtx(ctx, jti)
		if err != nil {
			t.Fatalf("IsRevokedCtx(%q): %v", jti, err)
		}
		if got != want {
			t.Errorf("IsRevokedCtx(%q) = %v, want %v", jti, got, want)
		}
		if bl.IsRevoked(jti) != want {
			t.Errorf("IsRevoked(%q) != %v", jti, want)
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := bl.IsRevokedCtx(cancelled, live); err == nil {
		t.Error("IsRevokedCtx on a cancelled context returned nil; the error must reach the caller")
	}
	if _, err := bl.LoadActive(cancelled); err == nil {
		t.Error("LoadActive on a cancelled context returned nil; the error must reach the caller")
	}
}
