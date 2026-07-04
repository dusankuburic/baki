package middleware

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// bucketStore is the per-IP token-bucket decision a RateLimiter delegates to.
// It has two implementations: an in-process map (correct for single-replica)
// and a Redis-backed store (atomic across replicas via a Lua script). The store
// abstraction is what lets the backplane be optional (#1): the wiring picks the
// implementation from config without the HTTP middleware caring.
type bucketStore interface {
	// Allow returns true if a request from `key` should be admitted, consuming
	// one token. rate/capacity configure the bucket (refill per second / max).
	// Implementations must be safe for concurrent use.
	Allow(ctx context.Context, key string, rate, capacity float64) bool
	// Stop releases store-internal resources (e.g. the in-memory cleanup
	// goroutine). The Redis store is a no-op; its client is closed by the fx
	// lifecycle that owns it.
	Stop()
}

// RateLimiter is a per-IP token-bucket rate limiter. The bucket state lives in
// the embedded store, so the same HTTP-facing type serves both the in-memory
// (single-replica) and Redis-backed (multi-replica) deployments.
type RateLimiter struct {
	rate         float64 // tokens added per second
	capacity     float64 // max tokens
	trustedIPs   []net.IP
	trustedCIDRs []*net.IPNet
	store        bucketStore
	group        string
}

// SetGroup sets the metric label used when refusing requests; defaults to
// "general" if unset. Call once after NewRateLimiter to differentiate
// instances ("auth", "general", etc.). The group also namespaces the shared
// Redis bucket key so two limiters (e.g. "auth" and "chat") never collide.
func (rl *RateLimiter) SetGroup(g string) *RateLimiter {
	rl.group = g
	return rl
}

// NewRateLimiter creates an in-process RateLimiter that allows rps requests per
// second with a burst capacity of burst. Correct for single-replica; for
// multi-replica use NewRateLimiterRedis so the limit is shared across pods.
//
// Trusted-proxy entries are parsed here as either plain IP addresses or CIDR
// blocks; malformed entries are silently dropped (config-load validation already
// rejects them, this is defense-in-depth so the middleware never panics on a bad
// input it shouldn't have received).
func NewRateLimiter(rps, burst float64, trustedProxies []string) *RateLimiter {
	return newRateLimiter(rps, burst, trustedProxies, newInMemoryStore())
}

// NewRateLimiterRedis creates a RateLimiter backed by the shared Redis client so
// the per-IP token bucket is enforced across every replica that shares the
// client. A nil client falls back to the in-memory store (used by callers that
// build one limiter constructor and switch on backplane availability).
func NewRateLimiterRedis(client *redis.Client, rps, burst float64, trustedProxies []string) *RateLimiter {
	if client == nil {
		return NewRateLimiter(rps, burst, trustedProxies)
	}
	return newRateLimiter(rps, burst, trustedProxies, newRedisStore(client))
}

func newRateLimiter(rps, burst float64, trustedProxies []string, store bucketStore) *RateLimiter {
	rl := &RateLimiter{
		rate:     rps,
		capacity: burst,
		store:    store,
		group:    "general",
	}
	for _, p := range trustedProxies {
		entry := strings.TrimSpace(p)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil {
				rl.trustedCIDRs = append(rl.trustedCIDRs, cidr)
			}
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			rl.trustedIPs = append(rl.trustedIPs, ip)
		}
	}
	return rl
}

func (rl *RateLimiter) Stop() {
	rl.store.Stop()
}

// isTrustedProxy reports whether the request's immediate peer is in the
// configured trusted-proxy list (matching either an exact IP or any CIDR).
func (rl *RateLimiter) isTrustedProxy(remoteIP string) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	for _, t := range rl.trustedIPs {
		if t.Equal(ip) {
			return true
		}
	}
	for _, c := range rl.trustedCIDRs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// Limit returns middleware that enforces the rate limit, keyed by remote IP.
func (rl *RateLimiter) Limit(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.getIP(r)
		if !rl.store.Allow(r.Context(), rl.key(ip), rl.rate, rl.capacity) {
			RecordRateLimitExceeded(rl.group)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			// Best-effort: the client likely doesn't care about the body, and
			// since we've already written the status, we have nothing to
			// recover. Swallow silently if it fails.
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// key namespaces the bucket by group so different limiters sharing one Redis
// instance (the multi-replica case) never collide. For the per-instance
// in-memory store the prefix is harmless.
func (rl *RateLimiter) key(ip string) string {
	return "ratelimit:" + rl.group + ":" + ip
}

// getIP extracts the real client IP, only trusting X-Forwarded-For if the
// request comes from a configured trusted proxy.
func (rl *RateLimiter) getIP(r *http.Request) string {
	remoteIP := r.RemoteAddr
	// SplitHostPort handles both "1.2.3.4:5678" and "[::1]:5678"; fall back to
	// the raw value for malformed inputs.
	if host, _, err := net.SplitHostPort(remoteIP); err == nil {
		remoteIP = host
	}

	if len(rl.trustedIPs) == 0 && len(rl.trustedCIDRs) == 0 {
		return remoteIP
	}
	if !rl.isTrustedProxy(remoteIP) {
		return remoteIP
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remoteIP
	}
	// Rightmost = the address the trusted proxy appended; the leftmost is
	// client-forgeable and would let a client rotate it to dodge the limiter.
	return rightmostForwardedFor(xff)
}

// ---- in-memory store (single-replica default) ----

type bucket struct {
	tokens   float64
	lastFill time.Time
}

type inMemoryStore struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	stop     chan struct{}
	stopOnce sync.Once
}

func newInMemoryStore() *inMemoryStore {
	s := &inMemoryStore{
		buckets: make(map[string]*bucket),
		stop:    make(chan struct{}),
	}
	go s.cleanup()
	return s
}

func (s *inMemoryStore) Allow(_ context.Context, key string, rate, capacity float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, lastFill: time.Now()}
		s.buckets[key] = b
	}

	now := time.Now()
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

// cleanup removes stale buckets every minute to prevent unbounded memory growth.
func (s *inMemoryStore) cleanup() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.mu.Lock()
			cutoff := time.Now().Add(-5 * time.Minute)
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

// redisBucketTTL is how long an idle bucket survives in Redis before expiring.
// It mirrors the in-memory cleanup window (5 min): an expired bucket is rebuilt
// at full capacity on the next request, so the limiter self-heals after idle.
const redisBucketTTL = 5 * time.Minute

// tokenBucketScript atomically refills and consumes a token from the named
// bucket. Running it via EVAL makes the check-and-decrement atomic across every
// replica sharing the Redis instance, so the effective rate limit does not
// multiply by replica count (the core #1 defect).
//
// KEYS[1]   = bucket key
// ARGV[1..] = rate, capacity, now(ms), ttl(ms)
// returns   = 1 if admitted (token consumed), 0 if limited
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

type redisStore struct {
	client *redis.Client
}

func newRedisStore(client *redis.Client) *redisStore {
	return &redisStore{client: client}
}

func (s *redisStore) Allow(ctx context.Context, key string, rate, capacity float64) bool {
	now := time.Now().UnixMilli()
	res, err := tokenBucketScript.Run(ctx, s.client, []string{key},
		rate, capacity, now, redisBucketTTL.Milliseconds()).Int()
	// Fail open on a Redis error: a backplane blip must not take the whole API
	// offline (every request would 429-on-no-quorum). The in-memory fallback
	// covers planned single-replica; an unreachable Redis during a transient
	// outage degrading the limit is preferable to a hard outage. Operators alert
	// on the backplane being down via the redisx boot ping + ACA readiness.
	if err != nil {
		return true
	}
	return res == 1
}

func (s *redisStore) Stop() {} // client closed by the redisx fx lifecycle
