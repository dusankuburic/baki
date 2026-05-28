package middleware

import (
	"encoding/json"
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
	rate           float64 // tokens added per second
	capacity       float64 // max tokens
	trustedProxies []string
	mu             sync.Mutex
	buckets        map[string]*bucket
}

// NewRateLimiter creates a RateLimiter that allows rps requests per second with
// a burst capacity of burst.
func NewRateLimiter(rps, burst float64, trustedProxies []string) *RateLimiter {
	rl := &RateLimiter{
		rate:           rps,
		capacity:       burst,
		trustedProxies: trustedProxies,
		buckets:        make(map[string]*bucket),
	}
	go rl.cleanup()
	return rl
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
// request comes from a trusted proxy.
func (rl *RateLimiter) getIP(r *http.Request) string {
	remoteIP := r.RemoteAddr
	if idx := strings.LastIndex(remoteIP, ":"); idx != -1 {
		remoteIP = remoteIP[:idx]
	}

	// Only trust XFF if it comes from a trusted proxy
	if len(rl.trustedProxies) > 0 {
		isTrusted := false
		for _, proxy := range rl.trustedProxies {
			if remoteIP == proxy {
				isTrusted = true
				break
			}
		}

		if isTrusted {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				if idx := strings.IndexByte(xff, ','); idx >= 0 {
					return strings.TrimSpace(xff[:idx])
				}
				return strings.TrimSpace(xff)
			}
		}
	}

	return remoteIP
}

// Limit returns middleware that enforces the rate limit, keyed by remote IP.
func (rl *RateLimiter) Limit(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.getIP(r)
		if !rl.allow(ip) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
			return
		}
		h.ServeHTTP(w, r)
	})
}
