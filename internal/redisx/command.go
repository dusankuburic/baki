package redisx

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"pad-core/logger"
)

// CommandBus is the request/reply transport between replicas over two declared per-instance channels: a command is
// published to the channel of the replica that owns the thing it acts on, which answers on the channel of the
// replica that asked. It carries bytes: the payloads, their fields beyond the two below and their meaning belong to
// the user (the chat stream commands, internal/service/chat_commands.go).
//
// Two fields are the transport's own and are read from every payload: a command carries "requestId" (the
// correlation key) and "replyTo" (the instance to answer); a reply carries "requestId". Everything else is opaque.
//
// Redis pub/sub is fire-and-forget, so the guarantees are narrow and the callers are written around them:
//   - Call fails at once when nobody listens on the target's channel (PUBLISH reports zero receivers): the owner is
//     down, restarting or not subscribed yet. That is the same answer as a timeout, only quicker.
//   - Call fails when Redis cannot be reached, or when the reply does not arrive in time. The command may still
//     have been delivered and acted on, so a command must be safe to send twice.
//   - Nothing is queued: a command published while the owner's subscription is re-establishing is lost, and
//     counted as a failed Call by its sender.
type CommandBus struct {
	c        *redis.Client
	instance string
	cmd      *Key
	reply    *Key
	ps       *PubSub

	cmdReady   atomic.Bool
	replyReady atomic.Bool

	mu      sync.Mutex
	waiters map[string]chan []byte
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	slots   chan struct{}

	// key signs and verifies the frames (WithBusKey); seen is the nonce window that goes with it; now is the clock
	// (tests move it).
	key  []byte
	seen *nonceSet
	now  func() time.Time
}

// Errors of Call. All of them mean "the command did not get a confirmed answer"; the caller reports the owner as
// unreachable and the user retries.
var (
	// ErrNoReceiver: no replica listens on the target's channel.
	ErrNoReceiver = errors.New("redisx: no replica is listening for commands on the target channel")
	// ErrNotReady: this replica's own reply subscription is not established, so an answer could not arrive.
	ErrNotReady = errors.New("redisx: the command bus is not subscribed yet")
	// ErrNoReply: the command was published but no answer came in time.
	ErrNoReply = errors.New("redisx: no reply to the command in time")
)

// maxInFlightCommands bounds the handler goroutines of one replica: a burst beyond it is dropped (its sender times
// out), which keeps a misbehaving peer from growing this replica's goroutines without limit.
const maxInFlightCommands = 64

// maxCommandBytes bounds what the bus parses: the commands are a few hundred bytes.
const maxCommandBytes = 16 << 10

// NewCommandBus builds the bus for instance. cmd and reply are the declared channel families whose last part is the
// instance. The bus is idle until Start.
func NewCommandBus(c *redis.Client, instance string, cmd, reply *Key, opts ...BusOption) *CommandBus {
	b := &CommandBus{
		c: c, instance: instance, cmd: cmd, reply: reply, ps: NewPubSub(c),
		waiters: map[string]chan []byte{},
		slots:   make(chan struct{}, maxInFlightCommands),
		now:     time.Now,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Instance is the name commands to this replica are addressed with, and replies come back to.
func (b *CommandBus) Instance() string { return b.instance }

// Ready reports whether both subscriptions are established.
func (b *CommandBus) Ready() bool { return b.cmdReady.Load() && b.replyReady.Load() }

// Start subscribes to this replica's two channels and serves commands with handle until Stop. handle returns the
// reply payload (nil sends none); it runs on its own goroutine, so a slow one delays nothing else. Both
// subscriptions are re-established after any break, with a backoff, for as long as the bus runs.
func (b *CommandBus) Start(handle func(ctx context.Context, payload []byte) []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.wg.Add(2)
	go func() {
		defer b.wg.Done()
		b.follow(ctx, b.cmd.Key(b.instance), &b.cmdReady, func(payload []byte) { b.dispatch(ctx, payload, handle) })
	}()
	go func() {
		defer b.wg.Done()
		b.follow(ctx, b.reply.Key(b.instance), &b.replyReady, b.deliverReply)
	}()
}

// Stop ends both subscriptions and waits for them. Calls in flight fail with their own deadline.
func (b *CommandBus) Stop() {
	b.mu.Lock()
	cancel := b.cancel
	b.cancel = nil
	b.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	b.wg.Wait()
	b.cmdReady.Store(false)
	b.replyReady.Store(false)
}

// follow keeps one channel subscribed until ctx ends.
func (b *CommandBus) follow(ctx context.Context, channel string, ready *atomic.Bool, on func([]byte)) {
	const minBackoff, maxBackoff = 250 * time.Millisecond, 5 * time.Second
	backoff := minBackoff
	warned := false
	for ctx.Err() == nil {
		err := b.ps.Subscribe(ctx, channel, func() { ready.Store(true); backoff = minBackoff; warned = false }, on)
		ready.Store(false)
		if ctx.Err() != nil {
			return
		}
		// One line per outage, not one per retry; the cause is an error class, never the error text (which can
		// carry the server's address).
		if !warned {
			logger.Warn("redis command channel lost; resubscribing", "mechanism", MechanismOf(channel), "cause", errorClass(err))
			warned = true
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// dispatch hands one command to handle on a bounded goroutine and publishes the reply it returns.
func (b *CommandBus) dispatch(ctx context.Context, raw []byte, handle func(context.Context, []byte) []byte) {
	payload := b.unwrap(dirCommand, b.instance, raw)
	if payload == nil || len(payload) > maxCommandBytes {
		return
	}
	var head struct {
		RequestID string `json:"requestId"`
		ReplyTo   string `json:"replyTo"`
	}
	if json.Unmarshal(payload, &head) != nil || head.ReplyTo == "" || head.RequestID == "" {
		return
	}
	select {
	case b.slots <- struct{}{}:
	default:
		LimitedWarn("command_overload", "redis command dropped: too many in flight", nil, "mechanism", MechanismOf(b.cmd.Key(b.instance)))
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() { <-b.slots }()
		reply := handle(ctx, payload)
		if reply == nil {
			return
		}
		// The asker is the one waiting: a bounded publish on a context of its own, since the bus may be stopping.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if frame := b.wrap(b.reply.Name(), dirReply, head.ReplyTo, reply); frame != nil {
			_ = b.c.Publish(pctx, b.reply.Key(head.ReplyTo), frame).Err()
		}
	}()
}

// deliverReply hands a reply to the Call waiting for it; one nobody waits for (the Call timed out) is dropped.
func (b *CommandBus) deliverReply(raw []byte) {
	payload := b.unwrap(dirReply, b.instance, raw)
	if payload == nil || len(payload) > maxCommandBytes {
		return
	}
	var head struct {
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(payload, &head) != nil {
		return
	}
	b.mu.Lock()
	ch := b.waiters[head.RequestID]
	b.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- payload:
	default:
	}
}

// Call publishes payload (carrying requestID and replyTo = Instance()) to target's channel and waits up to wait for
// the reply. See the type's comment for what its errors do and do not mean.
func (b *CommandBus) Call(ctx context.Context, target, requestID string, payload []byte, wait time.Duration) ([]byte, error) {
	if !b.replyReady.Load() {
		return nil, ErrNotReady
	}
	ch := make(chan []byte, 1)
	b.mu.Lock()
	b.waiters[requestID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiters, requestID)
		b.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	frame := b.wrap(b.cmd.Name(), dirCommand, target, payload)
	if frame == nil {
		return nil, ErrNotReady
	}
	n, err := b.c.Publish(ctx, b.cmd.Key(target), frame).Result()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNoReceiver
	}
	select {
	case reply := <-ch:
		return reply, nil
	case <-ctx.Done():
		return nil, ErrNoReply
	}
}
