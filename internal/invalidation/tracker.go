package invalidation

import "time"

// quietAfter is how long an origin may stay silent before the receiver forgets its epoch. Heartbeats arrive every
// ten seconds, so a replica gone for ten minutes was scaled in; a new one under the same name starts again.
const quietAfter = 10 * time.Minute

type tracked struct {
	epoch uint64
	at    time.Time
}

// tracker follows each origin's message numbering to notice a message that never arrived. Every message of an
// origin carries its next epoch (1, 2, 3, ...), including the ones it could not send, and its heartbeat the last one
// used. It is used by the subscription's goroutine only.
type tracker struct {
	last map[string]tracked
	now  func() time.Time
}

func newTracker() *tracker { return &tracker{last: map[string]tracked{}, now: time.Now} }

// observe records a message of kind from origin at epoch and reports whether it shows a gap. The first sight of an
// origin sets the baseline: what it sent before was either covered by the resync that follows every subscription
// or is the receiver's own start. An epoch that goes back is an origin that restarted and counts again from one.
func (t *tracker) observe(origin, kind string, epoch uint64) (gap bool) {
	prev, seen := t.last[origin]
	t.last[origin] = tracked{epoch: epoch, at: t.now()}
	if !seen || epoch < prev.epoch {
		return false
	}
	if kind == kindHeartbeat {
		return epoch > prev.epoch // a heartbeat names the last epoch sent; the message before it was lost
	}
	return epoch > prev.epoch+1
}

// prune forgets the origins that went quiet.
func (t *tracker) prune() {
	cutoff := t.now().Add(-quietAfter)
	for origin, tr := range t.last {
		if tr.at.Before(cutoff) {
			delete(t.last, origin)
		}
	}
}
