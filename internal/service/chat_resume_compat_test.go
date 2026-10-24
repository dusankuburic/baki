package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/testutil/redisreplica"
)

// A resume snapshot a release-0043 replica mirrored (captured from that tree before the Redis envelope existed) is
// read by this release, field for field.
func TestPrevRelease_ResumeSnapshotIsRead(t *testing.T) {
	c := redisreplica.New(t, 1)
	c.Redis.Set(resumeKey.Key("stream-1"), string(redisreplica.PrevFixture(t, "0043", "chat_resume_snapshot.json")))

	got, ok := redisResumeStore{c: c.Replicas[0].Client}.Load(context.Background(), "stream-1")
	if !ok {
		t.Fatal("a release-0043 snapshot was not loaded")
	}
	if got.Owner != "user-42" || got.Text != "Partial answer ✅" || got.Done || got.TokensIn != 11 || got.TokensOut != 22 ||
		got.ServedModel != "gpt-test" {
		t.Errorf("snapshot = %+v", got)
	}
	if len(got.Events) != 1 || got.Events[0].Type != "tool" {
		t.Errorf("journal = %+v, want the one tool event", got.Events)
	}
}

// A snapshot of a version this release does not know is a miss and stays in place: the replica that wrote it may be
// the one the client resumes on.
func TestResumeSnapshot_UnknownVersionIsAMissAndNotDeleted(t *testing.T) {
	c := redisreplica.New(t, 1)
	key := resumeKey.Key("stream-2")
	c.Redis.Set(key, `{"v":7,"owner":"user-42","text":"from the future"}`)
	c.Redis.SetTTL(key, time.Minute)

	if _, ok := (redisResumeStore{c: c.Replicas[0].Client}).Load(context.Background(), "stream-2"); ok {
		t.Fatal("a snapshot of an unknown version was loaded")
	}
	if !c.Redis.Exists(key) {
		t.Error("the reader deleted a snapshot it could not read")
	}
}

// Every resume key carries a TTL, which is what lets the server evict under volatile-lru.
func TestResumeStore_SaveSetsTheDeclaredTTL(t *testing.T) {
	c := redisreplica.New(t, 1)
	st := redisResumeStore{c: c.Replicas[0].Client}
	if !st.Save(context.Background(), "stream-3", resumeSnapshot{Owner: "u"}, maxChatStreamDuration+resumeRetention) {
		t.Fatal("Save failed")
	}
	if ttl := c.Redis.TTL(resumeKey.Key("stream-3")); ttl <= 0 || ttl > resumeKey.Spec().TTL {
		t.Errorf("TTL = %v, want within the declared %v", ttl, resumeKey.Spec().TTL)
	}
}
