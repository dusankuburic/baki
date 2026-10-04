package redisx

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// PubSub is the Redis fan-out for auth.CachedBlacklist: it satisfies
// auth.RevocationPublisher and auth.RevocationSubscriber over the shared
// client, so a logout on one replica revokes on every replica within one
// message rather than one reload interval. It lives here, not in auth, which
// must stay free of the Redis dependency.
type PubSub struct {
	c *redis.Client
}

// NewPubSub wraps the backplane client (non-nil: the caller picks the
// single-replica configuration when there is none).
func NewPubSub(c *redis.Client) *PubSub { return &PubSub{c: c} }

// Publish sends payload on channel.
func (p *PubSub) Publish(ctx context.Context, channel string, payload []byte) error {
	return p.c.Publish(ctx, channel, payload).Err()
}

// Subscribe follows channel until ctx ends (nil) or the subscription breaks
// (the error). onReady runs once the server has confirmed the subscription —
// before that, a message published would be lost, which is exactly the window
// the caller treats as "not subscribed".
//
// go-redis reads honour a context DEADLINE but not cancellation, so the
// subscription is closed from a watcher when ctx ends; that unblocks the
// pending read with an error, which is reported as a clean nil return.
func (p *PubSub) Subscribe(ctx context.Context, channel string, onReady func(), onMessage func(payload []byte)) error {
	ps := p.c.Subscribe(ctx, channel)
	defer func() { _ = ps.Close() }()
	stopWatcher := context.AfterFunc(ctx, func() { _ = ps.Close() })
	defer stopWatcher()

	// The first reply is the server's subscription confirmation.
	if _, err := ps.Receive(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	onReady()
	for {
		msg, err := ps.ReceiveMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		onMessage([]byte(msg.Payload))
	}
}
