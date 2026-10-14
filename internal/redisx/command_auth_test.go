package redisx_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/testutil/redisreplica"
)

// Cross-replica commands are authenticated (REVIEW_REDIS M2): write access to Redis alone is not authority to cancel
// a stream or approve a fix. These tests run the bus keyed.

func keyedBuses(t *testing.T, c *redisreplica.Cluster, keys []string, handle func(context.Context, []byte) []byte) []*redisx.CommandBus {
	t.Helper()
	var out []*redisx.CommandBus
	for i, r := range c.Replicas {
		b := redisx.NewCommandBus(r.Client, r.ID, testCmd, testReply, redisx.WithBusKey(redisx.DeriveBusKey(keys[i])))
		b.Start(handle)
		t.Cleanup(b.Stop)
		out = append(out, b)
	}
	for _, b := range out {
		redisreplica.Eventually(t, 3*time.Second, b.Instance()+" subscribed", b.Ready)
	}
	return out
}

func rejected(reason string) float64 {
	return promtest.ToFloat64(redisx.BusRejectedForTest(reason))
}

func cmdFor(t *testing.T, id, op string) []byte {
	return marshal(t, testCommand{V: 1, Kind: "test.cmd", RequestID: id, ReplyTo: "replica-1", Op: op})
}

// captured subscribes to a replica's command channel to take a frame off the wire as an eavesdropper with read
// access would.
func captured(t *testing.T, c *redisreplica.Cluster, target string) func() string {
	t.Helper()
	sub := c.Replicas[0].Client.Subscribe(context.Background(), testCmd.Key(target))
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	return func() string {
		msg, err := sub.ReceiveMessage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return msg.Payload
	}
}

func TestCommandBus_SignedRoundTrip(t *testing.T) {
	c := redisreplica.New(t, 2)
	b := keyedBuses(t, c, []string{"shared-secret", "shared-secret"}, echo)
	reply, err := b[0].Call(context.Background(), "replica-2", "r1", cmdFor(t, "r1", "ping"), time.Second)
	if err != nil {
		t.Fatalf("signed Call: %v", err)
	}
	var got struct{ Status string }
	if err := json.Unmarshal(reply, &got); err != nil || got.Status != "ping" {
		t.Fatalf("reply = %s (%v), want the handler's answer with the original payload", reply, err)
	}
}

func TestCommandBus_UnsignedCommandIsDropped(t *testing.T) {
	c := redisreplica.New(t, 2)
	var handled atomic.Int32
	keyedBuses(t, c, []string{"k", "k"}, func(ctx context.Context, p []byte) []byte { handled.Add(1); return echo(ctx, p) })
	before := rejected("unsigned")
	// What a process with write access but not the key can send: a well-formed plain command.
	if err := c.Replicas[0].Client.Publish(context.Background(), testCmd.Key("replica-2"), cmdFor(t, "x", "decide")).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if handled.Load() != 0 {
		t.Fatal("an unsigned command reached the handler")
	}
	if got := rejected("unsigned") - before; got != 1 {
		t.Errorf("unsigned rejections += %v, want 1", got)
	}
}

func TestCommandBus_WrongKeyIsDropped(t *testing.T) {
	c := redisreplica.New(t, 2)
	var handled atomic.Int32
	b := keyedBuses(t, c, []string{"key-one", "key-two"}, func(ctx context.Context, p []byte) []byte { handled.Add(1); return echo(ctx, p) })
	before := rejected("bad_mac")
	_, err := b[0].Call(context.Background(), "replica-2", "r1", cmdFor(t, "r1", "ping"), 150*time.Millisecond)
	if !errors.Is(err, redisx.ErrNoReply) {
		t.Fatalf("Call signed with another key = %v, want no reply (dropped)", err)
	}
	if handled.Load() != 0 {
		t.Fatal("a command signed with another key reached the handler")
	}
	if got := rejected("bad_mac") - before; got != 1 {
		t.Errorf("bad_mac rejections += %v, want 1", got)
	}
}

func TestCommandBus_TamperedBodyIsDropped(t *testing.T) {
	c := redisreplica.New(t, 2)
	var handled atomic.Int32
	b := keyedBuses(t, c, []string{"k", "k"}, func(ctx context.Context, p []byte) []byte { handled.Add(1); return echo(ctx, p) })
	take := captured(t, c, "replica-2")
	if _, err := b[0].Call(context.Background(), "replica-2", "r1", cmdFor(t, "r1", "cancel"), time.Second); err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(take()), &frame); err != nil {
		t.Fatalf("a signed command is a JSON object: %v", err)
	}
	body, _ := frame["body"].(string)
	if body == "" {
		t.Fatalf("signed frame %v has no body", frame)
	}
	handled.Store(0)
	before := rejected("bad_mac")
	frame["body"] = body[:len(body)-1] + ` ,"op":"decide"}`
	forged, _ := json.Marshal(frame)
	if err := c.Replicas[0].Client.Publish(context.Background(), testCmd.Key("replica-2"), forged).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if handled.Load() != 0 {
		t.Fatal("a command with an altered body reached the handler")
	}
	if got := rejected("bad_mac") - before; got != 1 {
		t.Errorf("bad_mac rejections += %v, want 1", got)
	}
}

// A captured frame sent again within the window is the same command twice: dropped.
func TestCommandBus_ReplayedFrameIsDropped(t *testing.T) {
	c := redisreplica.New(t, 2)
	var handled atomic.Int32
	b := keyedBuses(t, c, []string{"k", "k"}, func(ctx context.Context, p []byte) []byte { handled.Add(1); return echo(ctx, p) })
	take := captured(t, c, "replica-2")
	if _, err := b[0].Call(context.Background(), "replica-2", "r1", cmdFor(t, "r1", "cancel"), time.Second); err != nil {
		t.Fatal(err)
	}
	payload := take()
	if handled.Load() != 1 {
		t.Fatalf("handler ran %d times for the first send, want 1", handled.Load())
	}
	before := rejected("replay")
	if err := c.Replicas[0].Client.Publish(context.Background(), testCmd.Key("replica-2"), payload).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if handled.Load() != 1 {
		t.Fatalf("the replayed frame reached the handler (%d runs)", handled.Load())
	}
	if got := rejected("replay") - before; got != 1 {
		t.Errorf("replay rejections += %v, want 1", got)
	}
}

// Outside the clock window a correctly signed frame is stale.
func TestCommandBus_StaleFrameIsDropped(t *testing.T) {
	c := redisreplica.New(t, 2)
	var handled atomic.Int32
	b := keyedBuses(t, c, []string{"k", "k"}, func(ctx context.Context, p []byte) []byte { handled.Add(1); return echo(ctx, p) })
	b[0].SetNowForTest(func() time.Time { return time.Now().Add(-10 * time.Minute) })
	before := rejected("stale")
	if _, err := b[0].Call(context.Background(), "replica-2", "r1", cmdFor(t, "r1", "ping"), 150*time.Millisecond); !errors.Is(err, redisx.ErrNoReply) {
		t.Fatalf("Call with a ten-minute-old timestamp = %v, want no reply (dropped)", err)
	}
	if handled.Load() != 0 {
		t.Fatal("a stale frame reached the handler")
	}
	if got := rejected("stale") - before; got != 1 {
		t.Errorf("stale rejections += %v, want 1", got)
	}
}

// A frame signed for one replica is not accepted by another (a command copied to a different channel).
func TestCommandBus_FrameForAnotherReplicaIsDropped(t *testing.T) {
	c := redisreplica.New(t, 3)
	var handled atomic.Int32
	b := keyedBuses(t, c, []string{"k", "k", "k"}, func(ctx context.Context, p []byte) []byte { handled.Add(1); return echo(ctx, p) })
	take := captured(t, c, "replica-2")
	if _, err := b[0].Call(context.Background(), "replica-2", "r1", cmdFor(t, "r1", "cancel"), time.Second); err != nil {
		t.Fatal(err)
	}
	payload := take()
	handled.Store(0)
	before := rejected("target")
	if err := c.Replicas[0].Client.Publish(context.Background(), testCmd.Key("replica-3"), payload).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if handled.Load() != 0 {
		t.Fatal("a frame addressed to replica-2 was served by replica-3")
	}
	if got := rejected("target") - before; got != 1 {
		t.Errorf("target rejections += %v, want 1", got)
	}
}

// A reply nobody signed does not complete a Call: a process that can publish cannot answer for the owner.
func TestCommandBus_ForgedReplyDoesNotCompleteACall(t *testing.T) {
	c := redisreplica.New(t, 2)
	// The owner never answers; an intruder answers on its behalf.
	b := keyedBuses(t, c, []string{"k", "k"}, func(context.Context, []byte) []byte { return nil })
	done := make(chan error, 1)
	go func() {
		_, err := b[0].Call(context.Background(), "replica-2", "r1", cmdFor(t, "r1", "cancel"), 300*time.Millisecond)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	forged := marshal(t, map[string]any{"v": 1, "kind": "test.reply", "requestId": "r1", "status": "ok"})
	if err := c.Replicas[1].Client.Publish(context.Background(), testReply.Key("replica-1"), forged).Err(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, redisx.ErrNoReply) {
		t.Fatalf("Call answered by an unsigned reply = %v, want ErrNoReply", err)
	}
}

func TestDeriveBusKey_IsStableAndSeparatedBySecret(t *testing.T) {
	a, b := redisx.DeriveBusKey("secret"), redisx.DeriveBusKey("secret")
	if string(a) != string(b) || len(a) != 32 {
		t.Fatalf("DeriveBusKey is not a stable 32-byte key: %x vs %x", a, b)
	}
	if string(redisx.DeriveBusKey("secret")) == string(redisx.DeriveBusKey("other")) {
		t.Fatal("different secrets derived the same key")
	}
	if string(a) == "secret" {
		t.Fatal("the derived key is the secret itself")
	}
}
