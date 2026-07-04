package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/config"
)

// fakeResumeStore is an in-test backplane standing in for Redis, letting us
// exercise the cross-replica fallback in ResumeStream/OwnerOf without a real
// Redis (the stream "ran on another replica" = present here, absent locally).
type fakeResumeStore struct {
	mu   sync.Mutex
	data map[string]resumeSnapshot
	on   bool
}

func newFakeResumeStore(on bool) *fakeResumeStore {
	return &fakeResumeStore{data: map[string]resumeSnapshot{}, on: on}
}

func (f *fakeResumeStore) enabled() bool { return f.on }

func (f *fakeResumeStore) Save(_ context.Context, id string, snap resumeSnapshot, _ time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[id] = snap
}

func (f *fakeResumeStore) Load(_ context.Context, id string) (resumeSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, ok := f.data[id]
	return snap, ok
}

func TestResumeStream_FallsBackToBackplane(t *testing.T) {
	fake := newFakeResumeStore(true)
	fake.Save(context.Background(), "sid-remote", resumeSnapshot{
		Owner: "user-1", Text: "hello from replica A", Done: true, TokensIn: 3, TokensOut: 5,
	}, time.Minute)

	// Local maps are empty (the stream ran elsewhere) → must resolve via backplane.
	svc := &ChatService{resume: fake}

	res, err := svc.ResumeStream(context.Background(), "sid-remote", 0)
	if err != nil {
		t.Fatalf("ResumeStream fallback failed: %v", err)
	}
	if res.Text != "hello from replica A" || !res.Done || res.TokensOut != 5 {
		t.Fatalf("unexpected resume result: %+v", res)
	}
	if owner := svc.OwnerOf(context.Background(), "sid-remote"); owner != "user-1" {
		t.Fatalf("OwnerOf fallback = %q, want user-1", owner)
	}
}

func TestResumeStream_LocalTakesPrecedenceOverBackplane(t *testing.T) {
	fake := newFakeResumeStore(true)
	fake.Save(context.Background(), "sid", resumeSnapshot{Owner: "user-1", Text: "stale"}, time.Minute)

	svc := &ChatService{resume: fake}
	ctl := &streamCtl{ownerID: "user-1"}
	ctl.buffer.WriteString("live local buffer")
	svc.activeStreams.Store("sid", ctl)

	res, err := svc.ResumeStream(context.Background(), "sid", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "live local buffer" {
		t.Fatalf("expected local buffer to win, got %q", res.Text)
	}
}

func TestResumeStream_UnknownReturnsError(t *testing.T) {
	svc := &ChatService{resume: newFakeResumeStore(true)}
	if _, err := svc.ResumeStream(context.Background(), "nope", 0); err == nil {
		t.Fatal("expected not-found error for unknown stream")
	}
}

// TestResumeStream_DeltaFromOffset verifies C-5: a client that already holds a
// prefix of the buffer sends its length and receives only the tail, avoiding a
// full re-fetch on reconnect. Covers the local-stream path and clamping.
func TestResumeStream_DeltaFromOffset(t *testing.T) {
	svc := &ChatService{}
	ctl := &streamCtl{ownerID: "user-1"}
	ctl.buffer.WriteString("hello world")
	svc.activeStreams.Store("sid", ctl)

	// from=6 → tail only.
	res, err := svc.ResumeStream(context.Background(), "sid", 6)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "world" {
		t.Fatalf("delta resume from 6 = %q, want %q", res.Text, "world")
	}

	// from=0 → full buffer (backward-compatible full replace path).
	res, err = svc.ResumeStream(context.Background(), "sid", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello world" {
		t.Fatalf("full resume = %q, want full buffer", res.Text)
	}

	// from beyond the end → "" (client already has everything).
	res, err = svc.ResumeStream(context.Background(), "sid", 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "" {
		t.Fatalf("oversized from should yield empty text, got %q", res.Text)
	}

	// Negative from is clamped to 0 (full).
	res, err = svc.ResumeStream(context.Background(), "sid", -5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello world" {
		t.Fatalf("negative from should clamp to full, got %q", res.Text)
	}
}

// TestResumeStream_DeltaFromOffset_NonASCII verifies the byte-offset contract
// holds for multibyte UTF-8 (BUG-1 regression): the client now sends a UTF-8
// BYTE length (not JS UTF-16 code units), and the backend slices bytes. A
// pre-fix client sending UTF-16 units would land mid-rune here.
func TestResumeStream_DeltaFromOffset_NonASCII(t *testing.T) {
	svc := &ChatService{}
	ctl := &streamCtl{ownerID: "user-1"}
	ctl.buffer.WriteString("abc😀def") // 10 UTF-8 bytes: a,b,c,(emoji=4 bytes),d,e,f
	svc.activeStreams.Store("sid", ctl)

	// Byte offset 3 → the emoji + "def" (emoji occupies bytes 3..6).
	res, err := svc.ResumeStream(context.Background(), "sid", 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "😀def" {
		t.Errorf("from byte 3 = %q, want %q", res.Text, "😀def")
	}

	// Byte offset 7 → just "def" (past the emoji).
	res, err = svc.ResumeStream(context.Background(), "sid", 7)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "def" {
		t.Errorf("from byte 7 = %q, want %q", res.Text, "def")
	}

	// Full buffer is valid UTF-8 (no mid-rune slice).
	res, err = svc.ResumeStream(context.Background(), "sid", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "abc😀def" {
		t.Errorf("full = %q, want %q", res.Text, "abc😀def")
	}
}

// TestResumeStream_BackplaneDelta verifies the cross-replica backplane path
// also honours the from offset.
func TestResumeStream_BackplaneDelta(t *testing.T) {
	fake := newFakeResumeStore(true)
	fake.Save(context.Background(), "sid-remote", resumeSnapshot{
		Owner: "user-1", Text: "abcdefghij", Done: true,
	}, time.Minute)
	svc := &ChatService{resume: fake}

	res, err := svc.ResumeStream(context.Background(), "sid-remote", 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "efghij" {
		t.Fatalf("backplane delta from 4 = %q, want %q", res.Text, "efghij")
	}
}

func TestNoopResumeStore_IsDefault(t *testing.T) {
	// A ChatService built via a struct literal (nil resume) must degrade to a
	// no-op backplane rather than panic.
	svc := &ChatService{}
	if svc.resumeBackplane().enabled() {
		t.Fatal("nil resume field should behave as disabled no-op")
	}
	if _, ok := svc.resumeBackplane().Load(context.Background(), "x"); ok {
		t.Fatal("no-op Load should always miss")
	}
}

func TestSetResumeBackplane_NilKeepsDefault(t *testing.T) {
	svc := NewChatService(nil, "", nil, nil, nil, nil, nil, nil, config.ModeLocal)
	svc.SetResumeBackplane(nil) // PAD_REDIS_URL unset
	if svc.resumeBackplane().enabled() {
		t.Fatal("nil client must keep the single-replica no-op store")
	}
}
