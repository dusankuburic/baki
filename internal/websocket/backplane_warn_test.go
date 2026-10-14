package websocket

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
)

// syncBuffer is a log sink the test reads while the code under test writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// With Redis down an unthrottled warning per broadcast and per heartbeat, each with the server's address, is a flood
// that says nothing a single line does not (REVIEW_COMPAT M3, REVIEW_REDIS L8). The line carries the error class only,
// and a repeat within the interval is counted, not written.
func TestBackplane_RedisFailuresAreLoggedOncePerIntervalWithoutTheAddress(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	addr := mr.Addr()
	client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	b := &redisBackplane{c: client, replicaID: "r1", ctx: context.Background(), cancel: func() {}}
	mr.Close()

	redisx.ResetLimitedWarns()
	var out syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec, _ := jsonPresence("u1", "c1")
	for range 6 {
		b.publish("flow-1", []byte(`{"type":"x"}`))
		b.writePresence("flow-1", map[string][]byte{"c1": rec})
		b.removePresence("flow-1", "c1")
	}
	text := out.String()
	if strings.Contains(text, addr) || strings.Contains(text, "127.0.0.1") {
		t.Errorf("a log line carries the Redis address:\n%s", text)
	}
	for _, msg := range []string{"websocket backplane publish failed", "websocket backplane presence write failed", "websocket backplane presence delete failed"} {
		if n := strings.Count(text, msg); n != 1 {
			t.Errorf("%q logged %d times for 18 failures, want once", msg, n)
		}
	}
	if !strings.Contains(text, "suppressed") && strings.Count(text, "WARN") > 3 {
		t.Errorf("repeats were neither written nor counted:\n%s", text)
	}
}

func jsonPresence(user, conn string) ([]byte, error) {
	return []byte(`{"userId":"` + user + `","connId":"` + conn + `","exp":1}`), nil
}
