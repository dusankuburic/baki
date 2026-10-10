package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/api/render"
	"pad-core/logger"
)

// bucketStore is the token-bucket decision a RateLimiter delegates to: an in-process map (single-replica) or a Redis-backed store
// (atomic across replicas via a Lua script). The wiring picks the implementation; the middleware doesn't care.
type bucketStore interface {
	// Allow returns true if a request from `key` should be admitted,
	// consuming one token (rate = refill/second, capacity = max). Concurrency-safe.
	Allow(ctx context.Context, key string, rate, capacity float64) bool
	// Stop releases store-internal resources. The Redis store is a no-op;
	// its client is closed by the fx lifecycle that owns it.
	Stop()
}

// RateLimiter is a per-IP token-bucket rate limiter; the bucket state lives in the embedded store,
// so one HTTP-facing type serves both the in-memory and Redis-backed deployments.
type RateLimiter struct {
	rate     float64 // tokens added per second
	capacity float64 // max tokens
	proxies  proxySet
	store    bucketStore
	group    string
}

// SetGroup sets the refusal metric label (default "general") and namespaces the shared Redis
// bucket key so two limiters (e.g. "auth" and "chat") never collide.
func (rl *RateLimiter) SetGroup(g string) *RateLimiter {
	rl.group = g
	return rl
}

// NewRateLimiter creates an in-process RateLimiter (rps/second, burst capacity): correct for single-replica; use NewRateLimiterRedis to share the limit across
// pods. Malformed trusted-proxy entries are silently dropped so the middleware never panics on bad config.
func NewRateLimiter(rps, burst float64, trustedProxies []string) *RateLimiter {
	return newRateLimiter(rps, burst, trustedProxies, newInMemoryStore())
}

// NewRateLimiterRedis creates a RateLimiter backed by the shared Redis client so the bucket is
// enforced across every replica; a nil client falls back to the in-memory store.
func NewRateLimiterRedis(client *redis.Client, rps, burst float64, trustedProxies []string) *RateLimiter {
	if client == nil {
		return NewRateLimiter(rps, burst, trustedProxies)
	}
	return newRateLimiter(rps, burst, trustedProxies, newRedisStore(client))
}

func newRateLimiter(rps, burst float64, trustedProxies []string, store bucketStore) *RateLimiter {
	return &RateLimiter{
		rate:     rps,
		capacity: burst,
		proxies:  parseProxySet(trustedProxies),
		store:    store,
		group:    "general",
	}
}

func (rl *RateLimiter) Stop() {
	rl.store.Stop()
}

// Limit returns middleware that enforces the rate limit, keyed by remote IP.
func (rl *RateLimiter) Limit(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.getIP(r)
		if !rl.store.Allow(r.Context(), rl.key(ip), rl.rate, rl.capacity) {
			rl.refuse(w)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// refuse answers a request over the limit: 429 with Retry-After, the whole seconds one token takes to refill (at
// least 1), so a client that retries (bakicli report) knows when.
func (rl *RateLimiter) refuse(w http.ResponseWriter) {
	RecordRateLimitExceeded(rl.group)
	secs := 1
	if rl.rate > 0 {
		secs = max(1, int(math.Ceil(1/rl.rate)))
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	render.Error(w, fmt.Errorf("rate limit exceeded"), http.StatusTooManyRequests)
}

// LimitByKey enforces the limit keyed by an arbitrary caller-derived string (e.g. a per-user bucket). A "" from keyFn passes unchecked — callers use that to
// skip routes this limiter doesn't cover (e.g. reads for a per-user write-only limiter). The key is used verbatim.
func (rl *RateLimiter) LimitByKey(h http.Handler, keyFn func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyFn(r)
		if key == "" {
			h.ServeHTTP(w, r)
			return
		}
		if !rl.store.Allow(r.Context(), key, rl.rate, rl.capacity) {
			rl.refuse(w)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// key namespaces the bucket by group so limiters sharing one Redis instance
// never collide; for the in-memory store the prefix is harmless.
func (rl *RateLimiter) key(ip string) string {
	return "ratelimit:" + rl.group + ":" + RateLimitAddr(ip)
}

// RateLimitAddr is the part of a client address a per-IP rate-limit bucket is keyed on: an IPv6 client by its /64 (one host is routinely handed a whole /64, so per-address buckets let it rotate for a fresh burst every time).
// IPv4, IPv4-mapped IPv6 and translator-embedded IPv4 stay per-address (the /64 of ::ffff:a.b.c.d is the entire IPv4 internet), as does the address of an ip:port; any other non-IP value is used verbatim. Only rate-limit keys use this — ClientIP, which feeds the access log and audit trail, keeps the full address.
func RateLimitAddr(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		ap, perr := netip.ParseAddrPort(ip)
		if perr != nil {
			return ip
		}
		addr = ap.Addr()
	}
	if addr.Is4() || addr.Is4In6() {
		return addr.Unmap().String()
	}
	addr = addr.WithZone("")
	for _, p := range ipv4EmbeddingPrefixes {
		if p.Contains(addr) {
			b := addr.As16()
			return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}).String()
		}
	}
	if localNAT64Prefix.Contains(addr) {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

// ipv4EmbeddingPrefixes are the IPv6 prefixes whose low 32 bits are an IPv4 client's address as a translator presents it (RFC 6052 NAT64, RFC 6145
// IPv4-translated); keyed by their /64, every IPv4 client behind the translator would share one bucket — for the auth limiter, a collective lockout.
var ipv4EmbeddingPrefixes = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("::ffff:0:0:0/96"),
}

// localNAT64Prefix is RFC 8215's local-use NAT64 range: the operator picks the translation prefix's
// length inside it, so where the IPv4 address sits is unknown — its addresses are keyed whole.
var localNAT64Prefix = netip.MustParsePrefix("64:ff9b:1::/48")

// getIP extracts the real client IP, only trusting X-Forwarded-For if the
// request comes from a configured trusted proxy.
func (rl *RateLimiter) getIP(r *http.Request) string {
	return rl.proxies.clientIP(r)
}

// ---- in-memory store (single-replica default) ----

type bucket struct {
	tokens   float64
	lastFill time.Time
}

// maxInMemoryBuckets caps the in-memory bucket map: buckets are otherwise removed only after 5 minutes idle, and a key-minting client would grow the
// map without bound. 100 000 is ~12 MB, far above one replica's distinct clients in any 5-minute window.
const maxInMemoryBuckets = 100_000

// bucketIdleTTL is how long an unused bucket is kept by the periodic cleanup.
const bucketIdleTTL = 5 * time.Minute

// fullSweepInterval throttles the sweep a full map triggers: unthrottled, each new key at a map
// that stays full (the attack the cap exists for) would pay for a full locked walk.
const fullSweepInterval = time.Second

type inMemoryStore struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	// maxBuckets is maxInMemoryBuckets; a field so tests can use a small cap.
	maxBuckets int
	lastSweep  time.Time
	// warnedFull: the full-map warning is logged once per store, not once
	// per new key at a full map — that is every request of the attack.
	warnedFull bool
	stop       chan struct{}
	stopOnce   sync.Once
}

func newInMemoryStore() *inMemoryStore {
	s := &inMemoryStore{
		buckets:    make(map[string]*bucket),
		maxBuckets: maxInMemoryBuckets,
		stop:       make(chan struct{}),
	}
	go s.cleanup()
	return s
}

func (s *inMemoryStore) Allow(_ context.Context, key string, rate, capacity float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	b, ok := s.buckets[key]
	if !ok {
		b = s.newBucketLocked(key, now, rate, capacity)
	}

	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * rate
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.lastFill = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// newBucketLocked stores and returns a fresh full bucket for a key the map does not hold yet; at the cap it first sweeps (at most once per fullSweepInterval), then evicts one arbitrary bucket.
// A shared overflow bucket would not do: while a key-minting client holds the map full, practically every legitimate client would land in it — for the auth limiter a global login lockout — while an arbitrary victim is far likelier to be one of the attacker's many slots. Caller holds s.mu.
func (s *inMemoryStore) newBucketLocked(key string, now time.Time, rate, capacity float64) *bucket {
	if len(s.buckets) >= s.maxBuckets && now.Sub(s.lastSweep) >= fullSweepInterval {
		s.lastSweep = now
		s.sweepLocked(now, rate, capacity)
	}
	if len(s.buckets) >= s.maxBuckets {
		if !s.warnedFull {
			s.warnedFull = true
			logger.Warn("rate limiter: in-memory bucket map is full; each new client now evicts an arbitrary bucket",
				"max_buckets", s.maxBuckets)
		}
		// Every map iteration starts at a random position, so the first key
		// is one no client can choose.
		for k := range s.buckets {
			delete(s.buckets, k)
			break
		}
	}
	b := &bucket{tokens: capacity, lastFill: now}
	s.buckets[key] = b
	return b
}

// sweepLocked drops every bucket idle past bucketIdleTTL or refilled to capacity — the latter is
// lossless (a refilled bucket admits exactly what a fresh one would) and makes room quickly.
func (s *inMemoryStore) sweepLocked(now time.Time, rate, capacity float64) {
	cutoff := now.Add(-bucketIdleTTL)
	for k, b := range s.buckets {
		if b.lastFill.Before(cutoff) || b.tokens+now.Sub(b.lastFill).Seconds()*rate >= capacity {
			delete(s.buckets, k)
		}
	}
}

// cleanup removes stale buckets every minute to prevent unbounded memory growth.
func (s *inMemoryStore) cleanup() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.mu.Lock()
			cutoff := time.Now().Add(-bucketIdleTTL)
			for ip, b := range s.buckets {
				if b.lastFill.Before(cutoff) {
					delete(s.buckets, ip)
				}
			}
			s.mu.Unlock()
		case <-s.stop:
			return
		}
	}
}

func (s *inMemoryStore) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

// ---- Redis store (multi-replica shared backplane) ----

// redisBucketTTL is how long an idle bucket survives in Redis: it mirrors the in-memory cleanup
// window, and an expired bucket is rebuilt at full capacity on the next request (self-healing).
const redisBucketTTL = 5 * time.Minute

// tokenBucketScript atomically refills and consumes a token from the named bucket; EVAL makes the check-and-decrement atomic across every replica, so the
// effective limit does not multiply by replica count. KEYS[1]=bucket key; ARGV[1..]=rate, capacity, now(ms), ttl(ms); returns 1|0.
var tokenBucketScript = redis.NewScript(`
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl_ms = tonumber(ARGV[4])
local t = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(t[1])
local ts = tonumber(t[2])
if tokens == nil then
  tokens = capacity
  ts = now
end
local elapsed = math.max(0, now - ts) / 1000.0
tokens = math.min(capacity, tokens + elapsed * rate)
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call('HMSET', key, 'tokens', tostring(tokens), 'ts', tostring(now))
redis.call('PEXPIRE', key, ttl_ms)
return allowed
`)

// redisCallTimeout bounds one limiter round trip: without it, a Redis brownout
// blocks every API request for go-redis's default seconds before the fallback.
const redisCallTimeout = 75 * time.Millisecond

// redisBreakerCooldown is how long the store stays on its in-memory fallback after a failed Redis
// call; without it every request of a brownout pays redisCallTimeout first.
const redisBreakerCooldown = 5 * time.Second

type redisStore struct {
	client   *redis.Client
	fallback *inMemoryStore
	// openUntil is the unix-nano time before which Allow skips Redis (the
	// breaker is open); 0 when closed.
	openUntil atomic.Int64
	// now is time.Now; a field so tests can move the clock past the cooldown.
	now func() time.Time
}

func newRedisStore(client *redis.Client) *redisStore {
	return &redisStore{client: client, fallback: newInMemoryStore(), now: time.Now}
}

func (s *redisStore) Allow(ctx context.Context, key string, rate, capacity float64) bool {
	now := s.now()
	if now.UnixNano() < s.openUntil.Load() {
		return s.fallback.Allow(ctx, key, rate, capacity)
	}
	allowed, err := s.run(ctx, key, rate, capacity, now)
	if err != nil {
		// A request whose own context ended says nothing about Redis, so only
		// a failure under a live context opens the breaker.
		if ctx.Err() == nil {
			s.openUntil.Store(now.Add(redisBreakerCooldown).UnixNano())
		}
		// Fall back to a per-replica in-memory bucket instead of uncapping ("return true" let an attacker who DoS'd Redis disable ALL rate limiting):
		// the same rate/capacity per replica — an outage degrades but does not eliminate the limit.
		return s.fallback.Allow(ctx, key, rate, capacity)
	}
	return allowed
}

// run executes the token-bucket script, giving up after redisCallTimeout. The context deadline alone does not bound the call — go-redis applies it to socket reads/writes only on a ContextTimeoutEnabled client, and the shared backplane client (internal/redisx) is not.
// So the call runs on its own goroutine and run stops waiting at the deadline; the breaker keeps new calls off Redis meanwhile. An abandoned call ends at the client's ReadTimeout.
func (s *redisStore) run(ctx context.Context, key string, rate, capacity float64, now time.Time) (bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, redisCallTimeout)
	defer cancel()
	call := func() (bool, error) {
		res, err := tokenBucketScript.Run(callCtx, s.client, []string{key},
			rate, capacity, now.UnixMilli(), redisBucketTTL.Milliseconds()).Int()
		return res == 1, err
	}
	if s.client.Options().ContextTimeoutEnabled {
		return call()
	}
	type result struct {
		allowed bool
		err     error
	}
	done := make(chan result, 1) // buffered: an abandoned call must not block
	go func() {
		allowed, err := call()
		done <- result{allowed, err}
	}()
	select {
	case r := <-done:
		return r.allowed, r.err
	case <-callCtx.Done():
		return false, callCtx.Err()
	}
}

func (s *redisStore) Stop() {
	s.fallback.Stop()
}
