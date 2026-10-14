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

var (
	testCmd = redisx.Declare(redisx.Spec{
		Name: "test.cmd", Kind: redisx.KindPubSub, Pattern: "baki:test:cmd:{instance}", Version: 1,
		OnDown: redisx.OnDownClosed, Owner: "internal/redisx",
	})
	testReply = redisx.Declare(redisx.Spec{
		Name: "test.reply", Kind: redisx.KindPubSub, Pattern: "baki:test:reply:{instance}", Version: 1,
		OnDown: redisx.OnDownClosed, Owner: "internal/redisx",
	})
)

type testCommand struct {
	V         int    `json:"v"`
	Kind      string `json:"kind"`
	RequestID string `json:"requestId"`
	ReplyTo   string `json:"replyTo"`
	Op        string `json:"op"`
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// echo answers every command with its op.
func echo(_ context.Context, payload []byte) []byte {
	var c testCommand
	_ = json.Unmarshal(payload, &c)
	out, _ := json.Marshal(map[string]any{"v": 1, "kind": "test.reply", "requestId": c.RequestID, "status": c.Op})
	return out
}

func buses(t *testing.T, c *redisreplica.Cluster, handle func(context.Context, []byte) []byte) []*redisx.CommandBus {
	t.Helper()
	var out []*redisx.CommandBus
	for _, r := range c.Replicas {
		b := redisx.NewCommandBus(r.Client, r.ID, testCmd, testReply)
		b.Start(handle)
		t.Cleanup(b.Stop)
		out = append(out, b)
	}
	for _, b := range out {
		redisreplica.Eventually(t, 3*time.Second, b.Instance()+" subscribed", b.Ready)
	}
	return out
}

func TestCommandBus_RequestReply(t *testing.T) {
	c := redisreplica.New(t, 2)
	b := buses(t, c, echo)
	cmd := marshal(t, testCommand{V: 1, Kind: "test.cmd", RequestID: "r1", ReplyTo: "replica-1", Op: "ping"})
	reply, err := b[0].Call(context.Background(), "replica-2", "r1", cmd, time.Second)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got struct{ Status string }
	if err := json.Unmarshal(reply, &got); err != nil || got.Status != "ping" {
		t.Fatalf("reply = %s (%v)", reply, err)
	}
}

func TestCommandBus_NoReceiverFailsAtOnce(t *testing.T) {
	c := redisreplica.New(t, 2)
	b := buses(t, c, echo)
	started := time.Now()
	_, err := b[0].Call(context.Background(), "replica-9", "r1", marshal(t, testCommand{RequestID: "r1", ReplyTo: "replica-1"}), 2*time.Second)
	if !errors.Is(err, redisx.ErrNoReceiver) {
		t.Fatalf("Call to a replica nobody serves = %v, want ErrNoReceiver", err)
	}
	if took := time.Since(started); took > time.Second {
		t.Errorf("took %v, want an immediate refusal", took)
	}
}

func TestCommandBus_NoReplyTimesOut(t *testing.T) {
	c := redisreplica.New(t, 2)
	b := buses(t, c, func(context.Context, []byte) []byte { return nil })
	_, err := b[0].Call(context.Background(), "replica-2", "r1", marshal(t, testCommand{RequestID: "r1", ReplyTo: "replica-1"}), 100*time.Millisecond)
	if !errors.Is(err, redisx.ErrNoReply) {
		t.Fatalf("Call without a reply = %v, want ErrNoReply", err)
	}
}

func TestCommandBus_CallBeforeSubscribedIsRefused(t *testing.T) {
	c := redisreplica.New(t, 1)
	b := redisx.NewCommandBus(c.Replicas[0].Client, "replica-1", testCmd, testReply)
	if _, err := b.Call(context.Background(), "replica-2", "r1", nil, time.Second); !errors.Is(err, redisx.ErrNotReady) {
		t.Fatalf("Call on a bus that was never started = %v, want ErrNotReady", err)
	}
}

func TestCommandBus_RedisDownThenBack(t *testing.T) {
	c := redisreplica.New(t, 2)
	b := buses(t, c, echo)
	c.Kill()
	_, err := b[0].Call(context.Background(), "replica-2", "r1", marshal(t, testCommand{RequestID: "r1", ReplyTo: "replica-1"}), time.Second)
	if err == nil || errors.Is(err, redisx.ErrNoReply) {
		t.Fatalf("Call with Redis down = %v, want the transport error", err)
	}
	c.Restore(t)
	redisreplica.Eventually(t, 10*time.Second, "subscriptions re-established", func() bool { return b[0].Ready() && b[1].Ready() })
	redisreplica.Eventually(t, 5*time.Second, "a call after Redis returned", func() bool {
		_, err := b[0].Call(context.Background(), "replica-2", "r2", marshal(t, testCommand{RequestID: "r2", ReplyTo: "replica-1", Op: "ping"}), 500*time.Millisecond)
		return err == nil
	})
}

// A payload of a version or kind this release does not know is dropped and counted, never handed to the handler;
// junk is dropped without a reply; neither stops the bus.
func TestCommandBus_IgnoresWhatItCannotRead(t *testing.T) {
	c := redisreplica.New(t, 2)
	var handled atomic.Int32
	b := buses(t, c, func(ctx context.Context, p []byte) []byte { handled.Add(1); return echo(ctx, p) })
	unknownBefore := promtest.ToFloat64(redisx.MsgsCounterForTest("test.cmd", "unknown"))
	ctx := context.Background()
	pub := func(payload string) {
		if err := c.Replicas[0].Client.Publish(ctx, testCmd.Key("replica-2"), payload).Err(); err != nil {
			t.Fatal(err)
		}
	}
	pub(`{"v":9,"kind":"test.cmd","requestId":"x","replyTo":"replica-1"}`)
	pub(`{"v":1,"kind":"other.kind","requestId":"x","replyTo":"replica-1"}`)
	pub(`not json`)
	pub(`{"v":1,"kind":"test.cmd"}`) // no requestId / replyTo
	time.Sleep(100 * time.Millisecond)
	if n := handled.Load(); n != 0 {
		t.Fatalf("handler ran %d times for payloads it must not see", n)
	}
	if got := promtest.ToFloat64(redisx.MsgsCounterForTest("test.cmd", "unknown")) - unknownBefore; got != 2 {
		t.Errorf("unknown payloads counted = %v, want 2", got)
	}
	if _, err := b[0].Call(ctx, "replica-2", "r1", marshal(t, testCommand{V: 1, Kind: "test.cmd", RequestID: "r1", ReplyTo: "replica-1", Op: "ping"}), time.Second); err != nil {
		t.Fatalf("the bus stopped serving after junk: %v", err)
	}
}

// A burst past the in-flight bound is dropped, not queued without limit.
func TestCommandBus_BoundsInFlightHandlers(t *testing.T) {
	c := redisreplica.New(t, 2)
	release := make(chan struct{})
	var running atomic.Int32
	peak := atomic.Int32{}
	buses(t, c, func(context.Context, []byte) []byte {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return nil
	})
	for i := 0; i < 300; i++ {
		_ = c.Replicas[0].Client.Publish(context.Background(), testCmd.Key("replica-2"),
			marshal(t, testCommand{V: 1, Kind: "test.cmd", RequestID: "r", ReplyTo: "replica-1"})).Err()
	}
	time.Sleep(300 * time.Millisecond)
	close(release)
	if p := peak.Load(); p > 64 {
		t.Errorf("%d handlers ran at once, want at most the bound of 64", p)
	}
}
