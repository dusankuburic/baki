package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"pad-analyzer/internal/testutil/redisreplica"
)

// prev0043Snapshot is release 0043's resumeSnapshot, field for field: what an older replica decodes a snapshot into.
type prev0043Snapshot struct {
	Owner          string         `json:"owner"`
	Text           string         `json:"text"`
	Done           bool           `json:"done"`
	Error          string         `json:"error"`
	ErrorCode      string         `json:"errorCode,omitempty"`
	ErrorShared    bool           `json:"errorShared,omitempty"`
	TokensIn       int            `json:"tokensIn"`
	TokensOut      int            `json:"tokensOut"`
	ServedModel    string         `json:"servedModel,omitempty"`
	InputCostPerM  float64        `json:"inputCostPerM,omitempty"`
	OutputCostPerM float64        `json:"outputCostPerM,omitempty"`
	Events         []journalEvent `json:"events,omitempty"`
}

// A snapshot this release writes carries v, kind and replica beside the fields release 0043 reads, which decodes
// it unchanged (additive only: a rolling deploy runs both).
func TestNewRelease_SnapshotIsStillReadByRelease0043(t *testing.T) {
	c := redisreplica.New(t, 1)
	st := redisResumeStore{c: c.Replicas[0].Client, instance: "replica-7"}
	want := resumeSnapshot{Owner: "u1", Text: "héllo", TokensIn: 3, TokensOut: 4, ServedModel: "m", InputCostPerM: 1.5}
	if !st.Save(context.Background(), "s1", want, resumeRetention) {
		t.Fatal("Save failed")
	}
	raw, err := c.Redis.Get(resumeKey.Key("s1"))
	if err != nil {
		t.Fatal(err)
	}
	var head map[string]any
	if err := json.Unmarshal([]byte(raw), &head); err != nil {
		t.Fatal(err)
	}
	if head["v"] != float64(1) || head["kind"] != "chat.resume" || head["replica"] != "replica-7" {
		t.Errorf("snapshot head = %v, want v 1, kind chat.resume, replica replica-7", head)
	}
	var old prev0043Snapshot
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		t.Fatalf("release 0043 cannot decode the new snapshot: %v", err)
	}
	if old.Owner != "u1" || old.Text != "héllo" || old.TokensIn != 3 || old.TokensOut != 4 || old.ServedModel != "m" || old.InputCostPerM != 1.5 {
		t.Errorf("release 0043 read %+v, want the fields intact", old)
	}
}

// A snapshot a release-0043 replica wrote has no owner replica: this release reads it and knows whose it is, but
// the stream cannot be commanded (that release never answers one). The answer is the one that release gave a stream
// the replica did not run: not the caller's here, STREAM_NOT_FOUND, which the client takes as "gone" without a retry
// loop or a toast; never REPLICA_UNREACHABLE, which promises a retry that can only fail for as long as the stream
// lives (REVIEW_COMPAT M6).
func TestPrevRelease_SnapshotWithoutReplicaCannotBeCommanded(t *testing.T) {
	c := redisreplica.New(t, 1)
	svc := &ChatService{resume: redisResumeStore{c: c.Replicas[0].Client, instance: "replica-1"}, cluster: newChatCluster(c.Replicas[0].Client, "replica-1")}
	c.Redis.Set(resumeKey.Key("old-live"), string(redisreplica.PrevFixture(t, "0043", "chat_resume_snapshot.json")))

	if owner, err := svc.OwnerOfChecked(context.Background(), "old-live"); err != nil || owner != "user-42" {
		t.Errorf("OwnerOfChecked = (%q, %v), want user-42", owner, err)
	}
	if err := svc.CancelStreamAs(context.Background(), "user-42", "old-live"); !errors.Is(err, ErrStreamNotOwned) || errors.Is(err, ErrReplicaUnreachable) {
		t.Errorf("CancelStreamAs of a live stream of a release before the commands = %v, want ErrStreamNotOwned (STREAM_NOT_FOUND), never a retryable 503", err)
	}
	c.Redis.Set(resumeKey.Key("old-done"), `{"owner":"user-42","text":"x","done":true}`)
	if err := svc.CancelStreamAs(context.Background(), "user-42", "old-done"); err != nil {
		t.Errorf("CancelStreamAs of a finished stream = %v, want nil", err)
	}
}

// A command from a newer release with an op this one does not know is answered "unsupported", which the asking
// replica reports as unreachable; a command whose version it does not know never reaches the handler.
func TestCommands_UnknownOpIsUnsupported(t *testing.T) {
	svc := &ChatService{}
	out := svc.handleChatCommand(context.Background(), []byte(`{"v":1,"kind":"chat.cmd","op":"pause","requestId":"r1","replyTo":"x"}`))
	var rep chatReply
	if err := json.Unmarshal(out, &rep); err != nil || rep.Status != replyUnsupported || rep.RequestID != "r1" {
		t.Errorf("reply = %s (%v), want unsupported for r1", out, err)
	}
	if out := svc.handleChatCommand(context.Background(), []byte(`not json`)); out != nil {
		t.Errorf("reply to junk = %s, want none", out)
	}
}
