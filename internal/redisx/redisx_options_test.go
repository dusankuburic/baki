package redisx

import (
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
)

// The shared client must enable go-redis's in-band context deadlines: without
// ContextTimeoutEnabled the rate limiter wraps every command in a goroutine to
// apply the deadline (internal/api/middleware/ratelimit.go, redisStore.run) —
// one goroutine and buffered channel per request.
func TestOptionsFor_ContextTimeoutEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Redis.URL = secret.New("redis://localhost:6379/0")
	cfg.Redis.PoolSize = 7
	cfg.Redis.MinIdleConns = 2

	opts, err := optionsFor(cfg)
	if err != nil {
		t.Fatalf("optionsFor: %v", err)
	}
	if !opts.ContextTimeoutEnabled {
		t.Error("ContextTimeoutEnabled is false: the rate limiter will spawn a goroutine per request")
	}
	if opts.PoolSize != 7 {
		t.Errorf("PoolSize = %d, want the configured 7", opts.PoolSize)
	}
	if opts.MinIdleConns != 2 {
		t.Errorf("MinIdleConns = %d, want the configured 2", opts.MinIdleConns)
	}
}

func TestOptionsFor_ParseError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Redis.URL = secret.New("not-a-redis-url")
	if _, err := optionsFor(cfg); err == nil || !strings.Contains(err.Error(), "PAD_REDIS_URL") {
		t.Errorf("err = %v, want a PAD_REDIS_URL parse error", err)
	}
}

// TestOptionsFor_ParseErrorNeverQuotesTheURL (R9): url.Parse quotes the whole URL in its error, and go-redis quotes
// the parts it rejects (a port, a path, an option). A password with an unescaped reserved character lands in one
// of those, so a malformed PAD_REDIS_URL printed its password in the boot error.
func TestOptionsFor_ParseErrorNeverQuotesTheURL(t *testing.T) {
	const pw = "Sentinel7pw"
	for _, raw := range []string{
		"redis://:" + pw + "%zz@cache:6379",         // bad %-escape in the password: url.Error with the whole URL
		"redis://user:" + pw + "/x@cache:6379",      // '/' ends the authority: the password becomes a port
		"redis://user:6379/" + pw + "@cache:6379/0", // ... or a path go-redis rejects
		"redis://user:6379?" + pw + "@cache:6379",   // '?' ends it: the password becomes an unknown option
		"redis://user:6379/0?dial_timeout=" + pw,    // a bad option value
		"redis://user:6379/0?pool_size=" + pw,
		"redis://user:6379/0?pool_fifo=" + pw,
		"redis://user:6379/" + pw,                  // db number
		"http://user:" + pw + "@cache:6379",        // wrong scheme
		"unix://user:" + pw + "@",                  // empty socket path
		"redis://user:" + pw + "@[::1",             // unterminated IPv6 host
		"redis://user:" + pw + "@cache:6379/0\x7f", // control character
	} {
		cfg := &config.Config{}
		cfg.Redis.URL = secret.New(raw)
		_, err := optionsFor(cfg)
		if err == nil {
			t.Errorf("%q: no error", raw)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, pw) || strings.Contains(msg, "Sentinel") || !strings.HasPrefix(msg, "redis: parse PAD_REDIS_URL: ") {
			t.Errorf("%q: error %q", raw, msg)
		}
		if errors.Unwrap(err) != nil {
			t.Errorf("%q: the error wraps the original, which quotes the URL: %v", raw, errors.Unwrap(err))
		}
	}
}
