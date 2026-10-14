package redisx

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// Authenticated commands (REVIEW_REDIS M2). The command bus is a pub/sub transport any process with access to the
// Redis server can publish on, and the commands it carries act as the user: they stop a stream and approve an AI
// proposal that edits a flow. So when the replicas share a secret, every command and reply travels in a signed
// frame:
//
//	{"v":1,"kind":"<channel name>","dir":"cmd"|"reply","to":"<receiving replica>","ts":<unix ms>,"n":"<nonce>",
//	 "body":"<the payload, as text>","mac":"<hex HMAC-SHA256>"}
//
// The MAC covers the version string, the direction, the receiver, the timestamp, the nonce and the body, with a key
// derived from the deployment's secret (DeriveBusKey). A frame is accepted only when the MAC verifies, it is
// addressed to this replica on the right channel, its timestamp is within busClockWindow of the receiver's clock,
// and its nonce has not been seen inside that window: a captured frame cannot be sent again, nor to another
// replica, nor as a reply. The body is carried as text so the bytes that were signed are the bytes that are
// checked. The envelope's kind is the channel's own name, which is what the subscription layer counts and admits.
//
// Commands are new in Phase 5b, so no earlier release publishes or reads this channel and the signed form needs no
// compatibility with an unsigned sender. Without a key the bus carries the payload as it is (a deployment that has
// no shared secret: auth off, or only a per-process one); the caller says so at boot.

const (
	// busClockWindow is how far a frame's timestamp may differ from the receiver's clock. A command is answered
	// within seconds, so this only has to cover the skew between two replicas' clocks.
	busClockWindow = 30 * time.Second
	// busNonceCap bounds the nonces remembered. A nonce lives busClockWindow*2 (a frame is acceptable for that long
	// either side of now); only a holder of the key can fill this, and a full cache refuses rather than forgets.
	busNonceCap = 8192

	busFrameVersion = "baki-bus/1"
	dirCommand      = "cmd"
	dirReply        = "reply"
)

var busRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pad_redis_bus_rejected_total",
	Help: "Command-bus frames dropped before they reached a handler, by reason: unsigned (a plain payload on a keyed bus), malformed (not a frame), oversize, bad_mac (signed with another key, or altered), stale (timestamp outside the clock window), replay (a nonce seen already), target (addressed to another replica or direction), flood (the nonce cache is full). Anything but a trickle of stale is somebody publishing on the command channels who is not a replica, or replicas with different secrets.",
}, []string{"reason"})

func init() { metrics.MustRegister(busRejected) }

// DeriveBusKey derives the bus's 32-byte MAC key from the deployment's shared secret (HKDF-SHA256 with a purpose
// label), so the secret itself never signs anything and the key is of no use anywhere else.
func DeriveBusKey(secret string) []byte {
	k, err := hkdf.Key(sha256.New, []byte(secret), nil, "baki redis command bus v1", 32)
	if err != nil { // cannot happen for a 32-byte output
		return nil
	}
	return k
}

// BusOption configures a CommandBus.
type BusOption func(*CommandBus)

// WithBusKey makes the bus sign what it sends and accept only what verifies under key (DeriveBusKey). Without it
// the bus is plain.
func WithBusKey(key []byte) BusOption {
	return func(b *CommandBus) {
		if len(key) > 0 {
			b.key = key
			b.seen = &nonceSet{m: map[string]int64{}}
		}
	}
}

// Signed reports whether the bus signs and verifies.
func (b *CommandBus) Signed() bool { return len(b.key) > 0 }

type busFrame struct {
	Envelope
	Dir   string `json:"dir"`
	To    string `json:"to"`
	TS    int64  `json:"ts"`
	Nonce string `json:"n"`
	Body  string `json:"body"`
	MAC   string `json:"mac"`
}

func (b *CommandBus) mac(dir, to string, ts int64, nonce, body string) string {
	h := hmac.New(sha256.New, b.key)
	for _, part := range []string{busFrameVersion, dir, to, strconv.FormatInt(ts, 10), nonce, body} {
		// Length-prefixed, so no choice of fields can make two different tuples hash the same bytes.
		_, _ = h.Write([]byte(strconv.Itoa(len(part))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// wrap turns payload into what is published: the payload itself on a plain bus, a signed frame addressed to `to` on
// a keyed one.
func (b *CommandBus) wrap(channelName, dir, to string, payload []byte) []byte {
	if !b.Signed() {
		return payload
	}
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	f := busFrame{
		Envelope: Envelope{V: 1, Kind: channelName},
		Dir:      dir, To: to, TS: b.now().UnixMilli(), Nonce: hex.EncodeToString(nonce[:]), Body: string(payload),
	}
	f.MAC = b.mac(f.Dir, f.To, f.TS, f.Nonce, f.Body)
	out, err := json.Marshal(f)
	if err != nil { // strings and numbers
		return nil
	}
	return out
}

// unwrap returns the payload a received message carries, or nil after counting why it was dropped. dir and to are
// what the receiver expects: the direction of the channel it listens on and its own name.
func (b *CommandBus) unwrap(dir, to string, raw []byte) []byte {
	if len(raw) > maxCommandBytes*2 { // the frame is the payload plus ~250 bytes of framing, with escaping
		busRejected.WithLabelValues("oversize").Inc()
		return nil
	}
	if !b.Signed() {
		return raw
	}
	var f busFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		busRejected.WithLabelValues("malformed").Inc()
		return nil
	}
	if f.MAC == "" {
		busRejected.WithLabelValues("unsigned").Inc()
		return nil
	}
	if f.Dir == "" || f.Body == "" {
		busRejected.WithLabelValues("malformed").Inc()
		return nil
	}
	want, err := hex.DecodeString(f.MAC)
	got, _ := hex.DecodeString(b.mac(f.Dir, f.To, f.TS, f.Nonce, f.Body))
	if err != nil || !hmac.Equal(want, got) {
		busRejected.WithLabelValues("bad_mac").Inc()
		return nil
	}
	if f.Dir != dir || f.To != to {
		busRejected.WithLabelValues("target").Inc()
		return nil
	}
	now := b.now()
	if d := now.Sub(time.UnixMilli(f.TS)); d > busClockWindow || d < -busClockWindow {
		busRejected.WithLabelValues("stale").Inc()
		return nil
	}
	switch b.seen.add(f.Nonce, now, 2*busClockWindow) {
	case nonceReplay:
		busRejected.WithLabelValues("replay").Inc()
		return nil
	case nonceFull:
		busRejected.WithLabelValues("flood").Inc()
		return nil
	}
	return []byte(f.Body)
}

type nonceResult int

const (
	nonceNew nonceResult = iota
	nonceReplay
	nonceFull
)

// nonceSet remembers the nonces of accepted frames until they can no longer be replayed.
type nonceSet struct {
	mu sync.Mutex
	m  map[string]int64 // nonce → unix ms it may be forgotten
}

func (s *nonceSet) add(nonce string, now time.Time, ttl time.Duration) nonceResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := now.UnixMilli()
	if exp, ok := s.m[nonce]; ok && exp > ms {
		return nonceReplay
	}
	if len(s.m) >= busNonceCap {
		for n, exp := range s.m {
			if exp <= ms {
				delete(s.m, n)
			}
		}
		if len(s.m) >= busNonceCap {
			return nonceFull
		}
	}
	s.m[nonce] = now.Add(ttl).UnixMilli()
	return nonceNew
}
