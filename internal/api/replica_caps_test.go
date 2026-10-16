package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The per-user caps of the event streams and the uploads on a deployment that can run more than one replica
// (PAD_MAX_REPLICAS > 1): they count every replica's, so N replicas do not allow N times as many.

// capReplica is an event stream endpoint on one harness replica with the shared stream cap (as internal/di builds it).
func capReplica(t *testing.T, rep *redisreplica.Replica) *httptest.Server {
	t.Helper()
	m := NewEventManager(make(chan struct{}), WithConnCap(rep.Client, rep.Health))
	m.SetClientKeyFunc(func(r *http.Request) string { return "user:" + r.Header.Get("X-Test-User") })
	srv := httptest.NewServer(http.HandlerFunc(m.HandleEvents))
	t.Cleanup(srv.Close)
	return srv
}

// connect opens an event stream as user and returns its status; a 200 stream stays open until the test ends.
func connect(t *testing.T, srv *httptest.Server, user string) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return resp.StatusCode
}

// 10 event streams of one user on A leave none on B; the refusal is the one a replica's own cap answers (503).
func TestSSECap_CountsEveryReplicasStreams(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := capReplica(t, c.Replicas[0]), capReplica(t, c.Replicas[1])
	for i := 0; i < maxSSEConnsPerClient; i++ {
		if code := connect(t, a, "u1"); code != http.StatusOK {
			t.Fatalf("stream %d on A: %d", i+1, code)
		}
	}
	if code := connect(t, b, "u1"); code != http.StatusServiceUnavailable {
		t.Fatalf("an 11th stream of u1 on B: %d, want 503 (the cap counts A's streams)", code)
	}
	if code := connect(t, b, "u2"); code != http.StatusOK {
		t.Fatalf("another user's stream on B: %d", code)
	}
}

// Redis down: each replica's own cap of 10 applies, and only it.
func TestSSECap_RedisDownKeepsEachReplicasOwnCap(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := capReplica(t, c.Replicas[0]), capReplica(t, c.Replicas[1])
	c.Kill()
	for _, srv := range []*httptest.Server{a, b} {
		for i := 0; i < maxSSEConnsPerClient; i++ {
			if code := connect(t, srv, "u1"); code != http.StatusOK {
				t.Fatalf("stream %d with Redis down: %d (the cap fails open to the replica's own)", i+1, code)
			}
		}
		if code := connect(t, srv, "u1"); code != http.StatusServiceUnavailable {
			t.Fatalf("an 11th stream on one replica with Redis down: %d, want 503", code)
		}
	}
}

// A stream that closes gives its slot back across the replicas.
func TestSSECap_AClosedStreamFreesItsSlot(t *testing.T) {
	c := redisreplica.New(t, 2)
	m := NewEventManager(make(chan struct{}), WithConnCap(c.Replicas[0].Client, c.Replicas[0].Health))
	m.SetClientKeyFunc(func(r *http.Request) string { return "user:" + r.Header.Get("X-Test-User") })
	a := httptest.NewServer(http.HandlerFunc(m.HandleEvents))
	t.Cleanup(a.Close)
	b := capReplica(t, c.Replicas[1])
	for i := 0; i < maxSSEConnsPerClient-1; i++ {
		connect(t, a, "u1")
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	req.Header.Set("X-Test-User", "u1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the 10th stream on A: %v %v", resp, err)
	}
	cancel()
	_ = resp.Body.Close()
	redisreplica.Eventually(t, 3*time.Second, "A to release the closed stream's slot", func() bool { return m.connSlots.Held() == maxSSEConnsPerClient-1 })
	if code := connect(t, b, "u1"); code != http.StatusOK {
		t.Fatalf("a stream on B after one closed on A: %d", code)
	}
}

// 3 uploads of one user in progress on A leave none on B: B's waits for a slot and is refused (429) when its request
// ends, as the replica's own cap does.
func TestUploadCap_CountsEveryReplicasUploads(t *testing.T) {
	c := redisreplica.New(t, 2)
	a := &FlowHandler{uploadLimiter: newUploadLimiter()}
	b := &FlowHandler{uploadLimiter: newUploadLimiter()}
	a.ShareUploadCap(c.Replicas[0].Client, c.Replicas[0].Health)
	b.ShareUploadCap(c.Replicas[1].Client, c.Replicas[1].Health)
	var releases []func()
	for i := 0; i < maxConcurrentUploadsPerUser; i++ {
		release, ok := a.uploadLimiter.acquire(context.Background(), "u1")
		if !ok {
			t.Fatalf("upload %d on A refused", i+1)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, ok := b.uploadLimiter.acquire(ctx, "u1"); ok {
		t.Fatal("a 4th upload of u1 on B was admitted: the cap must count A's uploads")
	}
	if release, ok := b.uploadLimiter.acquire(context.Background(), "u2"); !ok {
		t.Fatal("another user's upload on B refused")
	} else {
		release()
	}
	// One finishes on A while B's waits: B's goes ahead.
	got := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		release, ok := b.uploadLimiter.acquire(ctx, "u1")
		release()
		got <- ok
	}()
	time.Sleep(100 * time.Millisecond)
	releases[0]()
	if !<-got {
		t.Fatal("B's waiting upload was not admitted after one finished on A")
	}
	for _, r := range releases[1:] {
		r()
	}
}

// Redis down: the uploads are capped per replica only.
func TestUploadCap_RedisDownKeepsEachReplicasOwnCap(t *testing.T) {
	c := redisreplica.New(t, 2)
	a := &FlowHandler{uploadLimiter: newUploadLimiter()}
	a.ShareUploadCap(c.Replicas[0].Client, c.Replicas[0].Health)
	c.Kill()
	for i := 0; i < maxConcurrentUploadsPerUser; i++ {
		release, ok := a.uploadLimiter.acquire(context.Background(), "u1")
		if !ok {
			t.Fatalf("upload %d with Redis down refused", i+1)
		}
		defer release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, ok := a.uploadLimiter.acquire(ctx, "u1"); ok {
		t.Fatal("a 4th upload on one replica with Redis down was admitted")
	}
}

// Without the shared cap (one replica) nothing touches Redis.
func TestReplicaCaps_NilClientIsLocalOnly(t *testing.T) {
	if NewEventManager(make(chan struct{}), WithConnCap(nil, nil)).connSlots != nil {
		t.Error("an event manager without a shared client has a shared cap")
	}
	h := &FlowHandler{uploadLimiter: newUploadLimiter()}
	h.ShareUploadCap(nil, nil)
	if h.uploadLimiter.shared != nil {
		t.Error("an upload limiter without a shared client has a shared cap")
	}
}

// An upload that waits for a slot across the replicas is one refusal in pad_redis_caps_total, however many times it
// asks again while it waits: the metric counts refused requests, not polls.
func TestUploadCap_AWaitingUploadIsOneRefusal(t *testing.T) {
	c := redisreplica.New(t, 2)
	a := &FlowHandler{uploadLimiter: newUploadLimiter()}
	b := &FlowHandler{uploadLimiter: newUploadLimiter()}
	a.ShareUploadCap(c.Replicas[0].Client, c.Replicas[0].Health)
	b.ShareUploadCap(c.Replicas[1].Client, c.Replicas[1].Health)
	for i := 0; i < maxConcurrentUploadsPerUser; i++ {
		release, ok := a.uploadLimiter.acquire(context.Background(), "u1")
		if !ok {
			t.Fatalf("upload %d on A refused", i+1)
		}
		defer release()
	}
	before := uploadCapsRefused(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*uploadSlotPoll)
	defer cancel()
	if _, ok := b.uploadLimiter.acquire(ctx, "u1"); ok {
		t.Fatal("premise: the 4th upload on B waits and is refused")
	}
	if got := uploadCapsRefused(t) - before; got != 1 {
		t.Errorf(`pad_redis_caps_total{cap="flow.upload-slots",outcome="refused"} += %v for one waiting upload, want 1`, got)
	}
}

// uploadCapsRefused reads pad_redis_caps_total{cap="flow.upload-slots",outcome="refused"} from the served registry.
func uploadCapsRefused(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "pad_redis_caps_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["cap"] == uploadSlotsKey.Name() && labels["outcome"] == "refused" {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
