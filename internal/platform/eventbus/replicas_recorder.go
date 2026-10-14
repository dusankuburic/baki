package eventbus

import (
	"context"
	"slices"
	"sync"
)

// ReplicasRecorder is a Replicas that records what it is asked to publish, for a test of what a replicated
// subscriber tells the other replicas, without a Redis.
type ReplicasRecorder struct {
	mu  sync.Mutex
	got []string
}

// PublishInvalidation records key and scope as "key{k=v,...}".
func (r *ReplicasRecorder) PublishInvalidation(_ context.Context, key string, scope Scope) {
	r.mu.Lock()
	r.got = append(r.got, key+"{"+scope.String()+"}")
	r.mu.Unlock()
}

// Published returns what was recorded so far, in order.
func (r *ReplicasRecorder) Published() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// Reset forgets what was recorded.
func (r *ReplicasRecorder) Reset() {
	r.mu.Lock()
	r.got = nil
	r.mu.Unlock()
}
