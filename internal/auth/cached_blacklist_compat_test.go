package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/testutil/redisreplica"
)

// A revocation message published by release 0043 (captured from that tree before the Redis envelope existed) is
// applied by this release.
func TestPrevRelease_RevocationMessageIsApplied(t *testing.T) {
	bl := newTestCache(t, newFakeSource(), nil)
	bl.apply(redisreplica.PrevFixture(t, "0043", "revocation_message.json"))
	if !bl.lookup("jti-0043-fixture") {
		t.Fatal("a release-0043 revocation message was not applied")
	}
}

type capturingPublisher struct{ payload []byte }

func (p *capturingPublisher) Publish(_ context.Context, _ string, payload []byte) error {
	p.payload = payload
	return nil
}

// The revocation message is still the release-0043 shape ({"jti","expires_at"}, no envelope): a security message
// reaches a peer of either release, and the 30 s reload is the backstop for one a peer cannot read.
func TestRevocationMessage_StaysTheV0Shape(t *testing.T) {
	pub := &capturingPublisher{}
	bl := NewCachedBlacklist(newFakeSource(), CachedBlacklistOptions{Publisher: pub, ReloadInterval: time.Hour})
	t.Cleanup(bl.Stop)
	bl.Add("jti-1", time.Hour)

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(pub.payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["jti"] == nil || fields["expires_at"] == nil {
		t.Errorf("published fields = %v, want exactly jti and expires_at", fields)
	}
}
