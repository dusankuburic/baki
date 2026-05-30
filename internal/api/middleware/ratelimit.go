package middleware

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type bucket struct {
	tokens   float64
	lastFill time.Time
}

// RateLimiter is a per-IP token-bucket rate limiter.
type RateLimiter struct {
	rate         float64 // tokens added per second
	capacity     float64 // max tokens
	trustedIPs   []net.IP
	trustedCIDRs []*net.IPNet
	mu           sync.Mutex
	buckets      map[string]*bucket
	// group labels rate_limit_exceeded_total emissions, so the metrics
	// scraper can distinguish "general" refusals from "auth" refusals.
	group string
}

// SetGroup sets the metric label used when refusing requests; defaults to
// "general" if unset. Call once after NewRateLimiter to differentiate
// instances ("auth", "general", etc.).
func (rl *RateLimiter) SetGroup(g string) *RateLimiter {
	rl.group = g
	return rl
}

// NewRateLimiter creates a RateLimiter that allows rps requests per second with
// a burst capacity of burst. Trusted-proxy entries are parsed here as either
// plain IP addresses or CIDR blocks; malformed entries are silently dropped
// (config-load validation already rejects them, this is defense-in-depth so
// the middleware never panics on a bad input it shouldn't have received).
func NewRateLimiter(rps, burst float64, trustedProxies []string) *RateLimiter {
	rl := &RateLimiter{
		rate:     rps,
		capacity: burst,
		buckets:  make(map[string]*bucket),
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
	go rl.cleanup()
	return rl
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

func (rl *RateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[ip]
	if !ok {
		b = &bucket{tokens: rl.capacity, lastFill: time.Now()}
		rl.buckets[ip] = b
	}

	now := time.Now()
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.capacity {
		b.tokens = rl.capacity
	}
	b.lastFill = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// cleanup removes stale buckets every minute to prevent unbounded memory growth.
func (rl *RateLimiter) cleanup() {
	t := time.NewTicker(time.Minute)
	for range t.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-5 * time.Minute)
		for ip, b := range rl.buckets {
			if b.lastFill.Before(cutoff) {
				delete(rl.buckets, ip)
			}
		}
		rl.mu.Unlock()
	}
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
	if idx := strings.IndexByte(xff, ','); idx >= 0 {
		return strings.TrimSpace(xff[:idx])
	}
	return strings.TrimSpace(xff)
}

// Limit returns middleware that enforces the rate limit, keyed by remote IP.
func (rl *RateLimiter) Limit(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.getIP(r)
		if !rl.allow(ip) {
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
