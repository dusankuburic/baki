package di

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
)

// The SSE fan-out is wired exactly when the deployment has the shared Redis (capability shared-state) and a client:
// two such managers see each other's streams; a manager built without either is the in-process one it always was.
func TestProvideEventManager_FanoutFollowsSharedState(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = client.Close() })
	health := redisx.NewHealth(client)
	lc := fxtest.NewLifecycle(t)

	shared := func(instance string) *config.Config {
		cfg := config.Default()
		cfg.Redis.URL = secret.New("redis://" + mr.Addr())
		cfg.Runtime.InstanceID = instance
		return cfg
	}
	holder := ProvideEventManager(lc, shared("holder"), make(chan struct{}), nil, client, health)
	asker := ProvideEventManager(lc, shared("asker"), make(chan struct{}), nil, client, health)
	// A client is configured but the deployment says no shared state: the fan-out stays off.
	solo := ProvideEventManager(lc, config.Default(), make(chan struct{}), nil, client, health)
	lc.RequireStart()
	t.Cleanup(lc.RequireStop)

	srv := httptest.NewServer(http.HandlerFunc(holder.HandleEvents))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })

	deadline := time.Now().Add(3 * time.Second)
	for !holdsField(mr, service.LocalUser, "holder#") {
		if time.Now().After(deadline) {
			t.Fatal("the holder never recorded its stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !asker.HasSubscriber(service.LocalUser) {
		t.Fatal("a manager with the fan-out did not see the stream held by the other one")
	}
	if solo.HasSubscriber(service.LocalUser) {
		t.Error("a manager without shared state answered from Redis")
	}
}

// holdsField reports whether the user's subscriber hash has a field that starts with prefix: a replica is named by
// its instance id and a boot suffix in Redis.
func holdsField(mr *miniredis.Miniredis, user, prefix string) bool {
	keys, err := mr.HKeys(redisx.SSESubsKey.Key(user))
	if err != nil {
		return false
	}
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// Two replicas that were given the same PAD_INSTANCE_ID (a copied setting, a fixed hostname, host networking) are
// still two replicas to the SSE backplane: each sees the other's streams and keeps its own presence record
// (REVIEW_REDIS M3). With the bare id the second took the first's events for its own and overwrote its record.
func TestProvideEventManager_TheSameInstanceIDOnTwoReplicasStaysTwoReplicas(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	newClient := func() (*redis.Client, *redisx.Health) {
		c := redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
		t.Cleanup(func() { _ = c.Close() })
		return c, redisx.NewHealth(c)
	}
	cfg := config.Default()
	cfg.Redis.URL = secret.New("redis://" + mr.Addr())
	cfg.Runtime.InstanceID = "copied-instance-id"
	lc := fxtest.NewLifecycle(t)
	c1, h1 := newClient()
	c2, h2 := newClient()
	holder := ProvideEventManager(lc, cfg, make(chan struct{}), nil, c1, h1)
	asker := ProvideEventManager(lc, cfg, make(chan struct{}), nil, c2, h2)
	lc.RequireStart()
	t.Cleanup(lc.RequireStop)

	srv := httptest.NewServer(http.HandlerFunc(holder.HandleEvents))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })

	deadline := time.Now().Add(3 * time.Second)
	for !holdsField(mr, service.LocalUser, "copied-instance-id#") {
		if time.Now().After(deadline) {
			t.Fatal("the holder never recorded its stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !asker.HasSubscriber(service.LocalUser) {
		t.Fatal("a replica configured with the same instance id did not see the other's stream: it took the record for its own")
	}
}
