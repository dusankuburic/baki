package testutil

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// A use older than the throttle is recorded again; the retention sweep removes credentials expired more than 30
// days ago, as the Postgres sweep does.
func TestFakeBackend_CICredentialTouchAndPurge(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: "org", Name: "Org", OwnerID: "u"}); err != nil {
		t.Fatal(err)
	}
	long, recent := time.Now().Add(-31*24*time.Hour), time.Now().Add(-24*time.Hour)
	for id, exp := range map[string]*time.Time{"k-long": &long, "k-recent": &recent, "k-never": nil} {
		if err := b.CreateCICredential(ctx, &interfaces.CICredential{ID: id, OrgID: "org", Secret: "s-" + id, ExpiresAt: exp}, 10); err != nil {
			t.Fatal(err)
		}
	}

	old := time.Now().Add(-6 * time.Minute).UTC()
	b.SetCICredentialLastUsed("k-never", &old)
	if err := b.TouchCICredential(ctx, "k-never"); err != nil {
		t.Fatal(err)
	}
	if c, _ := b.GetCICredential(ctx, "k-never"); c.LastUsedAt == nil || !c.LastUsedAt.After(old) {
		t.Errorf("a use six minutes old was not refreshed: %v", c.LastUsedAt)
	}

	res, err := b.PurgeExpiredData(ctx, 0)
	if err != nil || res.CICredentials != 1 {
		t.Fatalf("PurgeExpiredData = %+v, %v; want 1 CI credential removed", res, err)
	}
	if _, err := b.GetCICredential(ctx, "k-long"); err == nil {
		t.Error("the credential expired 31 days ago survived the sweep")
	}
	for _, id := range []string{"k-recent", "k-never"} {
		if _, err := b.GetCICredential(ctx, id); err != nil {
			t.Errorf("%s went with the sweep: %v", id, err)
		}
	}
}
