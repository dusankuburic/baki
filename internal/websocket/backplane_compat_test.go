package websocket

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The backplane payloads cross a rolling deploy in both directions: an old replica's records must still read here
// (previous-release fixtures, captured from release 0043 before the envelope existed), and an old replica must
// still read what this release writes (the old shapes, copied below).

// prevPubMessage and prevPresenceRecord are the shapes release 0043 decodes with: no v, no kind.
type prevPubMessage struct {
	Origin string          `json:"o"`
	Env    json.RawMessage `json:"e"`
}

type prevPresenceRecord struct {
	UserID          string `json:"userId"`
	DisplayName     string `json:"displayName,omitempty"`
	SelectedBlockID string `json:"selectedBlockId,omitempty"`
	SelectedAt      int64  `json:"selAt,omitempty"`
	ConnID          string `json:"connId,omitempty"`
	Exp             int64  `json:"exp"`
}

// redisMsgs reads pad_redis_msgs_total{kind,result} from the registry /metrics serves.
func redisMsgs(t *testing.T, kind, result string) float64 {
	t.Helper()
	families, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != "pad_redis_msgs_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["kind"] == kind && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// Release 0043's room message is delivered to the room's local clients as it always was.
func TestPrevRelease_RoomMessageIsDelivered(t *testing.T) {
	c := redisreplica.New(t, 1)
	delivered := make(chan []byte, 2)
	b := newRedisBackplane(c.Replicas[0].Client, "replica-new", func(flowID string, envJSON []byte) {
		if flowID != "flow-1" {
			t.Errorf("flowID = %q", flowID)
		}
		delivered <- envJSON
	})
	t.Cleanup(b.close)
	redisreplica.Eventually(t, 2*time.Second, "the pattern subscription", func() bool {
		return c.Redis.PubSubNumPat() > 0
	})

	fixture := redisreplica.PrevFixture(t, "0043", "ws_room_message.json")
	if err := c.Replicas[0].Client.Publish(context.Background(), roomChannel.Key("flow-1"), fixture).Err(); err != nil {
		t.Fatal(err)
	}
	var want prevPubMessage
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-delivered:
		if string(got) != string(want.Env) {
			t.Errorf("delivered envelope = %s, want %s", got, want.Env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a release-0043 room message was not delivered")
	}
}

// Release 0043's presence record is read as before, with its fields intact.
func TestPrevRelease_PresenceRecordIsRead(t *testing.T) {
	c := redisreplica.New(t, 1)
	b := newRedisBackplane(c.Replicas[0].Client, "replica-new", func(string, []byte) {})
	t.Cleanup(b.close)

	fixture := redisreplica.PrevFixture(t, "0043", "presence_record.json")
	c.Redis.HSet(presenceKey.Key("flow-1"), "conn-1", string(fixture))

	got := b.listPresence("flow-1")
	if len(got) != 1 {
		t.Fatalf("listPresence = %+v, want the one record", got)
	}
	want := presenceRecord{UserID: "user-1", DisplayName: "Alice", SelectedBlockID: "block-7", SelectedAt: 1790000000123456789, ConnID: "conn-1", Exp: 4102444800000000000}
	if got[0] != want {
		t.Errorf("record = %+v, want %+v", got[0], want)
	}
}

// What this release writes carries the envelope, and release 0043 still decodes it field for field.
func TestNewRelease_WritesTheEnvelopeAndOldReadersStillDecode(t *testing.T) {
	c := redisreplica.New(t, 1)
	client := c.Replicas[0].Client
	sub := client.PSubscribe(context.Background(), roomChannel.Glob())
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := newRedisBackplane(client, "replica-new", func(string, []byte) {})
	t.Cleanup(b.close)

	env, _ := json.Marshal(Envelope{Type: EventBlockUpdate, FlowID: "flow-1", UserID: "user-1", Timestamp: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)})
	b.publish("flow-1", env)
	msg, err := sub.ReceiveMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		V    int    `json:"v"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(msg.Payload), &head); err != nil || head.V != 1 || head.Kind != "ws.room" {
		t.Errorf("room message head = %+v, %v; want v 1, kind ws.room (%s)", head, err, msg.Payload)
	}
	var old prevPubMessage
	if err := json.Unmarshal([]byte(msg.Payload), &old); err != nil || old.Origin != "replica-new" || string(old.Env) != string(env) {
		t.Errorf("release 0043 decodes %+v, %v; want the origin and the envelope intact", old, err)
	}

	cl := &Client{UserID: "user-1", DisplayName: "Alice", connID: "conn-1", send: make(chan Envelope, 1)}
	cl.selectedBlockID, cl.selectedAt = "block-7", 42
	rec, _ := json.Marshal(cl.presenceRecord())
	if err := json.Unmarshal(rec, &head); err != nil || head.V != 1 || head.Kind != "ws.presence" {
		t.Errorf("presence record head = %+v, %v; want v 1, kind ws.presence (%s)", head, err, rec)
	}
	var oldRec prevPresenceRecord
	if err := json.Unmarshal(rec, &oldRec); err != nil || oldRec.UserID != "user-1" || oldRec.ConnID != "conn-1" || oldRec.SelectedBlockID != "block-7" || oldRec.SelectedAt != 42 {
		t.Errorf("release 0043 decodes %+v, %v; want the fields intact", oldRec, err)
	}
}

// A room message of a version or kind this release does not know is counted and ignored; the next one still flows.
func TestRoomMessage_UnknownVersionAndKindAreIgnoredAndCounted(t *testing.T) {
	c := redisreplica.New(t, 1)
	client := c.Replicas[0].Client
	delivered := make(chan string, 4)
	b := newRedisBackplane(client, "replica-new", func(_ string, envJSON []byte) { delivered <- string(envJSON) })
	t.Cleanup(b.close)
	redisreplica.Eventually(t, 2*time.Second, "the pattern subscription", func() bool { return c.Redis.PubSubNumPat() > 0 })

	unknown, bad := redisMsgs(t, "ws.room", "unknown"), redisMsgs(t, "ws.room", "bad")
	for _, payload := range []string{
		`{"v":2,"kind":"ws.room","o":"peer","e":{"type":"future"}}`,
		`{"v":1,"kind":"ws.other","o":"peer","e":{"type":"other"}}`,
		`not json`,
		`{"v":1,"kind":"ws.room","o":"peer","e":{"type":"block.update"}}`,
	} {
		if err := client.Publish(context.Background(), roomChannel.Key("flow-1"), payload).Err(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-delivered:
		if got != `{"type":"block.update"}` {
			t.Fatalf("delivered %s, want only the v1 message", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the v1 message after the unknown ones was not delivered")
	}
	select {
	case extra := <-delivered:
		t.Fatalf("an unknown message was delivered: %s", extra)
	case <-time.After(100 * time.Millisecond):
	}
	if d := redisMsgs(t, "ws.room", "unknown") - unknown; d != 2 {
		t.Errorf("unknown grew by %v, want 2", d)
	}
	if d := redisMsgs(t, "ws.room", "bad") - bad; d != 1 {
		t.Errorf("bad grew by %v, want 1", d)
	}
}

// A presence record this release cannot decode is skipped and left in the hash: another release may have written it
// and still depend on it. Only a record's own expiry removes it.
func TestDecodePresence_SkipsWhatItCannotDecodeAndNeverDeletesIt(t *testing.T) {
	c := redisreplica.New(t, 1)
	client := c.Replicas[0].Client
	b := newRedisBackplane(client, "replica-new", func(string, []byte) {})
	t.Cleanup(b.close)

	key := presenceKey.Key("flow-1")
	live, _ := json.Marshal(presenceRecord{UserID: "live", Exp: time.Now().Add(time.Minute).UnixNano()})
	expired, _ := json.Marshal(presenceRecord{UserID: "expired", Exp: time.Now().Add(-time.Minute).UnixNano()})
	c.Redis.HSet(key,
		"c-live", string(live),
		"c-expired", string(expired),
		"c-garbage", `not json`,
		"c-future", `{"v":2,"kind":"ws.presence","userId":"from-the-future","exp":4102444800000000000}`,
		"c-otherkind", `{"v":1,"kind":"ws.other","userId":"other","exp":4102444800000000000}`,
	)

	got := b.listPresence("flow-1")
	if len(got) != 1 || got[0].UserID != "live" {
		t.Fatalf("listPresence = %+v, want only the live record", got)
	}
	remaining, _ := c.Redis.HKeys(key)
	if len(remaining) != 4 {
		t.Errorf("hash fields after the read = %v; want the expired one pruned and the undecodable three left in place", remaining)
	}
	for _, f := range []string{"c-garbage", "c-future", "c-otherkind", "c-live"} {
		if c.Redis.HGet(key, f) == "" {
			t.Errorf("field %s was deleted by the reader", f)
		}
	}
	if c.Redis.HGet(key, "c-expired") != "" {
		t.Error("an expired record was not pruned")
	}
}
