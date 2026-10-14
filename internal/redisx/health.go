package redisx

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"pad-core/logger"

	"pad-analyzer/internal/metrics"
)

// A Redis outage is a degraded deployment, not an unready one: every mechanism that uses Redis has a declared
// on-down behaviour (Spec.OnDown), so the platform keeps the replica in rotation and the signal is this gauge and
// the error counter. Alert on pad_redis_up == 0 for a few minutes instead of on readiness.
var (
	// A vector without labels, not a plain gauge: it emits nothing until a Health exists, so a deployment without
	// Redis has no series and an alert on == 0 cannot fire for it.
	redisUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pad_redis_up",
		Help: "1 while the Redis backplane answers (a ping every 5 s and the outcome of every operation), 0 while it is degraded: the last ping failed or three operations in a row failed. Absent when PAD_REDIS_URL is unset. A sustained 0 means the cross-replica parts run in their declared on-down mode (rate limits per replica, no room fan-out, no resume across replicas) and replaces readiness as the signal.",
	}, nil)
	redisErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_redis_errors_total",
		Help: "Redis operations that failed, by mechanism (the declared key family the operation touched, ping for the health ping, other for a key no declaration claims). A cancelled caller is not counted: it says nothing about Redis.",
	}, []string{"mechanism"})
)

// redisEvictedKeys counts the keys the server evicted for memory since this process first looked (INFO stats). Every key the
// app writes has a TTL, so under volatile-lru any of them can be the one evicted: the in-flight budget ledger, an
// approval claim, a resume snapshot. Nothing else tells the operator that memory pressure is eating state.
var redisEvictedKeys = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "pad_redis_evicted_keys_total",
	Help: "Keys the Redis server evicted for memory (INFO stats evicted_keys, summed as deltas since this process started looking, polled every 30 s). Any increase means maxmemory is reached and cross-replica state (the AI budget ledger, approval claims, resume snapshots, the SSE log) is being dropped early: raise maxmemory.",
})

func init() { metrics.MustRegister(redisUp, redisErrors, redisEvictedKeys) }

const (
	// pingInterval is how often the health ping runs. A failed ping degrades at once, a good one recovers at once.
	pingInterval = 5 * time.Second
	// pingTimeout bounds one ping.
	pingTimeout = 2 * time.Second
	// opFailuresToDegrade is how many operations in a row must fail, with no success between, before the
	// operations alone degrade Redis: one slow call (the rate limiter gives up after 75 ms) says nothing.
	opFailuresToDegrade = 3
)

// Health tracks whether the Redis backplane is reachable: a ping loop every 5 s and the outcome of every operation
// of the shared client (it is a go-redis hook, so no call site reports anything). A nil *Health, and one built for
// a nil client, is a deployment without Redis: never degraded.
//
// Degraded() is the one question the rest of the app asks. /readyz adds "degraded":["redis"] to its answer when it
// is true, and later mechanisms use OnChange to resynchronise when Redis comes back.
type Health struct {
	client   *redis.Client
	interval time.Duration

	down     atomic.Bool  // degraded
	failures atomic.Int32 // operations failed in a row
	// pingDown: the last ping failed. Cleared by any success.
	pingDown atomic.Bool

	mu        sync.Mutex
	since     time.Time
	lastError string
	listeners []func(up bool)

	// The server-info poll (pollServerInfo): the last evicted_keys seen (-1: none yet), whether the maxmemory policy was
	// checked, and whether the server refuses INFO (a managed service can), after which it is not asked again.
	evictedSeen    int64
	policyChecked  bool
	infoRefused    bool
	lastInfoPollAt time.Time
	// info reads one INFO section; the client's, replaced by tests (miniredis does not report evictions).
	info func(ctx context.Context, section string) (string, error)
}

// NewHealth watches client (nil: no Redis). It installs the operation hook, so build it before the client is shared.
// A configured Health starts up: the first ping (Ping) decides.
func NewHealth(client *redis.Client) *Health {
	h := &Health{client: client, interval: pingInterval, since: time.Now(), evictedSeen: -1}
	h.info = func(ctx context.Context, section string) (string, error) { return h.client.Info(ctx, section).Result() }
	if client != nil {
		client.AddHook(h)
		redisUp.WithLabelValues().Set(1)
	}
	return h
}

// Configured reports whether a Redis client exists.
func (h *Health) Configured() bool { return h != nil && h.client != nil }

// Degraded reports whether Redis is configured and unreachable right now. It reads state; it never pings.
func (h *Health) Degraded() bool { return h.Configured() && h.down.Load() }

// Status is a point-in-time view for the admin health page.
type Status struct {
	Configured bool
	Degraded   bool
	// Since is when the current state began.
	Since time.Time
	// LastError is a short class of the last failure ("timeout", "connection refused", ...), never the error text.
	LastError string
}

// Status returns the current state.
func (h *Health) Status() Status {
	if !h.Configured() {
		return Status{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return Status{Configured: true, Degraded: h.down.Load(), Since: h.since, LastError: h.lastError}
}

// OnChange registers fn to run, on its own goroutine, whenever the state flips: up is true after a recovery. A
// mechanism that loses messages while Redis is away (pub/sub) resynchronises from it.
func (h *Health) OnChange(fn func(up bool)) {
	if !h.Configured() {
		return
	}
	h.mu.Lock()
	h.listeners = append(h.listeners, fn)
	h.mu.Unlock()
}

// Ping pings Redis now, records the outcome and returns it. It is the health ping of the loop and of the admin
// health page.
func (h *Health) Ping(ctx context.Context) error {
	if !h.Configured() {
		return nil
	}
	err := h.client.Ping(ctx).Err()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			h.fail("ping", err, true)
		}
		return err
	}
	h.RecordOK()
	return nil
}

// Run pings every interval until ctx ends.
func (h *Health) Run(ctx context.Context) {
	if !h.Configured() {
		return
	}
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			if err := h.Ping(pctx); err == nil && time.Since(h.lastInfoPollAt) >= serverInfoEvery {
				h.lastInfoPollAt = time.Now()
				h.pollServerInfo(pctx)
			}
			cancel()
		}
	}
}

// RecordOK notes a successful operation: Redis answered, so any failure streak and a failed ping are over.
func (h *Health) RecordOK() {
	if !h.Configured() {
		return
	}
	// The common case, once per rate-limited request: nothing to undo.
	if h.failures.Load() == 0 && !h.down.Load() && !h.pingDown.Load() {
		return
	}
	h.failures.Store(0)
	h.pingDown.Store(false)
	h.setDown(false, "")
}

// RecordError notes a failed operation of mechanism. A caller that gave up (context.Canceled) is not a Redis
// failure. mechanism is a declared name with dots as underscores (MechanismOf), or ping.
func (h *Health) RecordError(mechanism string, err error) {
	if !h.Configured() || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	h.fail(mechanism, err, false)
}

func (h *Health) fail(mechanism string, err error, ping bool) {
	redisErrors.WithLabelValues(mechanism).Inc()
	if ping {
		h.pingDown.Store(true)
	}
	if n := h.failures.Add(1); ping || n >= opFailuresToDegrade {
		h.setDown(true, errorClass(err))
	}
}

// setDown flips the state, once per transition.
func (h *Health) setDown(down bool, class string) {
	h.mu.Lock()
	if h.down.Load() == down {
		if down && class != "" {
			h.lastError = class
		}
		h.mu.Unlock()
		return
	}
	h.down.Store(down)
	h.since = time.Now()
	if down {
		h.lastError = class
		redisUp.WithLabelValues().Set(0)
	} else {
		redisUp.WithLabelValues().Set(1)
	}
	listeners := append([]func(bool){}, h.listeners...)
	h.mu.Unlock()

	if down {
		logger.Warn("redis degraded: the cross-replica mechanisms run in their on-down mode until it answers", "class", class)
	} else {
		logger.Info("redis recovered")
	}
	for _, fn := range listeners {
		go fn(!down)
	}
}

// errorClass reduces an error to a short class for the log line and the admin page. The error text itself is not
// used: it can carry the server's address.
func errorClass(err error) string {
	msg := strings.ToLower(err.Error())
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) || strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "refused"), strings.Contains(msg, "failed to dial"), strings.Contains(msg, "no route"), strings.Contains(msg, "unreachable"), strings.Contains(msg, "no such host"):
		return "connection refused"
	case strings.Contains(msg, "reset by peer"), strings.Contains(msg, "broken pipe"), strings.Contains(msg, "eof"), strings.Contains(msg, "closed"):
		return "connection reset"
	case strings.Contains(msg, "noauth"), strings.Contains(msg, "wrongpass"), strings.Contains(msg, "auth"):
		return "auth failed"
	default:
		return "unavailable"
	}
}

// --- go-redis hook: the outcome of every operation of the shared client ---

// DialHook is a pass-through: a failed dial surfaces as the failed command that needed the connection, which
// ProcessHook records once. (A subscription that cannot reconnect is the ping loop's to notice.)
func (h *Health) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook records one command's outcome. The health ping reports for itself.
func (h *Health) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if cmd.Name() == "ping" {
			return err
		}
		h.record(cmdMechanism(cmd), err)
		return err
	}
}

// ProcessPipelineHook records a pipeline as one operation.
func (h *Health) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		mech := otherMechanism
		if err != nil {
			for _, c := range cmds {
				if m := cmdMechanism(c); m != otherMechanism {
					mech = m
					break
				}
			}
		}
		h.record(mech, err)
		return err
	}
}

// record classifies an operation's outcome. A miss (redis.Nil) is an answer, and so is NOSCRIPT: a script run tries
// EVALSHA first and falls back to EVAL when the server has not cached it (every restart), which is Redis working.
func (h *Health) record(mechanism string, err error) {
	switch {
	case err == nil || errors.Is(err, redis.Nil) || strings.HasPrefix(err.Error(), "NOSCRIPT"):
		h.RecordOK()
	default:
		h.RecordError(mechanism, err)
	}
}

// cmdMechanism names the mechanism a command touched by its key or channel: the first argument after the command
// name, or the first key of an EVAL.
func cmdMechanism(cmd redis.Cmder) string {
	args := cmd.Args()
	i := 1
	if n := cmd.Name(); n == "eval" || n == "evalsha" {
		i = 3 // script, numkeys, then the first key
	}
	if i >= len(args) {
		return otherMechanism
	}
	switch k := args[i].(type) {
	case string:
		return MechanismOf(k)
	case []byte:
		return MechanismOf(string(k))
	}
	return otherMechanism
}

// serverInfoEvery is how often the health loop reads the server's INFO for evictions.
const serverInfoEvery = 30 * time.Second

// pollServerInfo reads evicted_keys (INFO stats) and, once, the maxmemory policy (INFO memory), and reports what
// they mean for the app's state: every key it writes carries a TTL so that volatile-lru is the right policy, and an
// eviction at all is state dropped early (the AI budget ledger under-counts, an approval claim is lost, a resume
// snapshot is gone). It is best effort: a server that refuses INFO is not asked again, and nothing here changes
// Redis's health beyond the one failed command the hook sees.
func (h *Health) pollServerInfo(ctx context.Context) {
	if !h.Configured() {
		return
	}
	h.mu.Lock()
	refused := h.infoRefused
	h.mu.Unlock()
	if refused {
		return
	}
	stats, err := h.info(ctx, "stats")
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			h.mu.Lock()
			h.infoRefused = true
			h.mu.Unlock()
		}
		return
	}
	if v, ok := infoField(stats, "evicted_keys"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			h.noteEvicted(n)
		}
	}
	h.mu.Lock()
	checked := h.policyChecked
	h.mu.Unlock()
	if checked {
		return
	}
	mem, err := h.info(ctx, "memory")
	if err != nil {
		return
	}
	h.mu.Lock()
	h.policyChecked = true
	h.mu.Unlock()
	if policy, ok := infoField(mem, "maxmemory_policy"); ok && strings.HasPrefix(policy, "allkeys-") {
		LimitedWarn("redis_policy", "redis maxmemory-policy evicts keys without a TTL too; the app expects volatile-lru (every key it writes has a TTL, "+
			"and the budget ledger and approval claims are lost when evicted)", nil, "policy", policy)
	}
}

// noteEvicted folds one reading of the server's cumulative counter into pad_redis_evicted_keys_total.
func (h *Health) noteEvicted(n int64) {
	h.mu.Lock()
	prev := h.evictedSeen
	h.evictedSeen = n
	h.mu.Unlock()
	var delta int64
	switch {
	case prev < 0:
		return // the first reading is the baseline: what the server evicted before this process looked is not ours
	case n >= prev:
		delta = n - prev
	default:
		delta = n // the server restarted and began counting again
	}
	if delta == 0 {
		return
	}
	redisEvictedKeys.Add(float64(delta))
	LimitedWarn("redis_evicted", "redis is evicting keys for memory: cross-replica state (the AI budget ledger, approval claims, resume snapshots, the SSE log) is dropped early; raise maxmemory", nil, "evicted", delta)
}

// infoField returns the value of "name:value" in an INFO reply.
func infoField(info, name string) (string, bool) {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, name+":"); ok {
			return v, true
		}
	}
	return "", false
}
