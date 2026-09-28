package ai

import (
	"context"
	"errors"
	"sync"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-core/logger"
)

func (s circuitState) String() string {
	switch s {
	case circuitOpen:
		return "open"
	case circuitHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

const (
	cbFailureThreshold = 5                // consecutive retryable failures before opening
	cbOpenDuration     = 30 * time.Second // minimum time before allowing a probe
)

type circuitState int

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

// breakerState is the circuit state for one upstream provider. It is shared,
// process-wide and keyed by provider ID (see breakerStateFor) so the breaker
// persists across requests even though ProviderFactory.For rebuilds the whole
// decorator chain — and therefore a fresh CircuitBreakerProvider — on every
// call. Without this, the per-instance state reset each request and the breaker
// could never accumulate enough failures to open.
type breakerState struct {
	mu          sync.Mutex
	state       circuitState
	failures    int
	lastFailure time.Time
}

var (
	breakerRegistry   = map[string]*breakerState{}
	breakerRegistryMu sync.Mutex
)

// breakerRegistryMax bounds the registry. Keys are provider × model, plus the
// resource for providers with per-user endpoints (breakerScoper), so a
// multi-tenant deployment's key set grows with its users' Azure resources;
// past the bound, cold entries are dropped and the oldest are evicted (see
// breakerStateFor).
const breakerRegistryMax = 4096

// breakerStaleFailure is how long a CLOSED circuit's last failure still
// protects it from pruning: several open-cooldowns, so a circuit that tripped
// recently keeps its history, while a key that failed once long ago becomes
// reclaimable. Failures only reset on a same-key success, so without this the
// registry could never shrink below the set of keys that EVER failed.
const breakerStaleFailure = 4 * cbOpenDuration

// breakerStateFor returns the shared circuit state for a registry key,
// creating it on first use. State is per upstream (not per user): a provider
// that is down is down for everyone — where "upstream" is the provider ID
// plus, for providers whose endpoint belongs to the user, that endpoint
// (breakerScoper).
func breakerStateFor(key string) *breakerState {
	breakerRegistryMu.Lock()
	defer breakerRegistryMu.Unlock()
	s, ok := breakerRegistry[key]
	if !ok {
		if len(breakerRegistry) >= breakerRegistryMax {
			pruneHealthyBreakersLocked()
			// Pruning only drops cold entries; a registry full of recently
			// failed keys stays full. Evict down to max-1 so the insert below
			// cannot push past the cap.
			evictBreakersToLocked(breakerRegistryMax - 1)
		}
		s = &breakerState{}
		breakerRegistry[key] = s
	}
	return s
}

// pruneHealthyBreakersLocked drops every closed circuit whose failure history
// has gone cold: entries without failures (forgetting one loses nothing — it
// starts from the same zero state) and entries whose last failure is older
// than breakerStaleFailure. Open and half-open circuits are always kept, and a
// dropped state still referenced by an in-flight call just records into an
// orphan. Caller holds breakerRegistryMu.
func pruneHealthyBreakersLocked() {
	cutoff := time.Now().Add(-breakerStaleFailure)
	for k, st := range breakerRegistry {
		st.mu.Lock()
		idle := st.state == circuitClosed && (st.failures == 0 || st.lastFailure.Before(cutoff))
		st.mu.Unlock()
		if idle {
			delete(breakerRegistry, k)
		}
	}
}

// evictBreakersToLocked evicts entries until the registry holds at most want:
// closed entries with the OLDEST lastFailure first (a never-failed entry's
// zero time counts as oldest), and only when no closed entry is left — every
// survivor open or half-open — the oldest entry regardless of state. The cap
// is a memory bound on a key domain the caller population drives (per-user
// endpoints, per-model keys), and losing one circuit's history costs at most
// re-learning a few failures. Caller holds breakerRegistryMu.
func evictBreakersToLocked(want int) {
	for len(breakerRegistry) > want {
		oldestKey, oldestAnyKey := "", ""
		var oldest, oldestAny time.Time
		for k, st := range breakerRegistry {
			st.mu.Lock()
			state, lastFailure := st.state, st.lastFailure
			st.mu.Unlock()
			if oldestAnyKey == "" || lastFailure.Before(oldestAny) {
				oldestAnyKey, oldestAny = k, lastFailure
			}
			if state != circuitClosed {
				continue
			}
			if oldestKey == "" || lastFailure.Before(oldest) {
				oldestKey, oldest = k, lastFailure
			}
		}
		if oldestKey != "" {
			delete(breakerRegistry, oldestKey)
			continue
		}
		if oldestAnyKey == "" {
			return
		}
		delete(breakerRegistry, oldestAnyKey)
	}
}

// breakerScoper is implemented by providers whose upstream is per user — an
// endpoint the user configured (Azure AI Foundry resources). Its scope joins
// the provider ID in the registry key: a 5xx from one tenant's resource says
// nothing about another tenant's resource that happens to use the same
// deployment name, so it must not open that tenant's circuit. The scope must
// not contain secrets; it never reaches metrics or logs.
type breakerScoper interface {
	breakerScope() string
}

func breakerScopeOf(p Provider) string {
	if s, ok := p.(breakerScoper); ok {
		return s.breakerScope()
	}
	return ""
}

// breakerKey scopes circuit state. Model-level: provider families mix
// per-model health (a deprecated model ID 500-ing while its siblings are
// fine — common on GitHub Models), and a provider-wide key meant five
// consecutive failures on ONE bad model blocked every model and every user
// for the cooldown. Empty model (Embed, model-less requests) falls back to
// the provider-wide key. Registry growth is bounded by distinct models in
// use, which the caller population bounds naturally.
func breakerKey(providerID, model string) string {
	if model == "" {
		return providerID
	}
	return providerID + "|" + model
}

// resetBreakerRegistry clears all shared breaker state. Test-only — it lets a
// test start from a known-closed circuit without leaking state into the next.
func resetBreakerRegistry() {
	breakerRegistryMu.Lock()
	defer breakerRegistryMu.Unlock()
	breakerRegistry = map[string]*breakerState{}
}

// CircuitBreakerProvider wraps a Provider and opens the circuit after
// cbFailureThreshold consecutive retryable failures. While open, calls fail
// immediately with ErrCircuitOpen (which is not retryable) to avoid hammering
// a known-down provider. After cbOpenDuration the circuit enters half-open and
// allows one probe — success closes it, another failure reopens it.
//
// Only retryable errors (ErrRateLimited, ErrProviderDown, transport-level
// network failures — see isTransportError) count toward the threshold;
// permanent errors like ErrApiKeyInvalid do not trip the circuit.
//
// The mutable state lives in a shared *breakerState (keyed by provider ID and,
// for providers with per-user endpoints, the endpoint — see breakerScoper), so
// every CircuitBreakerProvider for the same upstream — across users and across
// per-request chain rebuilds — observes the same circuit.
type CircuitBreakerProvider struct {
	Provider
	providerID string
	// scope is the wrapped provider's breakerScope ("" for shared
	// upstreams). It only keys the state; metrics and logs keep providerID.
	scope            string
	failureThreshold int
	openDuration     time.Duration
}

// stateFor resolves the shared circuit for this call's scope. Chat/Stream key
// by the request's model (see breakerKey); anything without a model uses the
// provider-wide key — per resource for a scoped provider.
func (cb *CircuitBreakerProvider) stateFor(model string) *breakerState {
	id := cb.providerID
	if cb.scope != "" {
		id += "#" + cb.scope
	}
	return breakerStateFor(breakerKey(id, model))
}

func NewCircuitBreakerProvider(p Provider) *CircuitBreakerProvider {
	return &CircuitBreakerProvider{
		Provider:         p,
		providerID:       p.ID(),
		scope:            breakerScopeOf(p),
		failureThreshold: cbFailureThreshold,
		openDuration:     cbOpenDuration,
	}
}

func NewCircuitBreakerProviderWithConfig(p Provider, threshold int, openDur time.Duration) *CircuitBreakerProvider {
	return &CircuitBreakerProvider{
		Provider:         p,
		providerID:       p.ID(),
		scope:            breakerScopeOf(p),
		failureThreshold: threshold,
		openDuration:     openDur,
	}
}

func (cb *CircuitBreakerProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	st := cb.stateFor(req.Model)
	if err := cb.checkSt(st); err != nil {
		return nil, err
	}
	defer cb.recordPanicSt(st)
	resp, err := cb.Provider.Chat(ctx, req)
	cb.recordSt(st, err)
	return resp, err
}

func (cb *CircuitBreakerProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	st := cb.stateFor("")
	if err := cb.checkSt(st); err != nil {
		return nil, err
	}
	defer cb.recordPanicSt(st)
	res, err := cb.Provider.Embed(ctx, text)
	cb.recordSt(st, err)
	return res, err
}

// recordPanic resolves the circuit when the wrapped provider panics. Without it,
// a panic skips the record() call in each method, so a half-open PROBE is never
// resolved — check() then rejects every subsequent caller to that provider
// forever (wedged until process restart). It records a failure (reopening a
// half-open probe / counting toward the threshold) and re-panics so the original
// crash still propagates to the caller's recover for logging. On the normal
// (no-panic) path recover() returns nil and this is a no-op — the explicit
// record() above already ran, so there is no double-record.
func (cb *CircuitBreakerProvider) recordPanicSt(st *breakerState) {
	if r := recover(); r != nil {
		cb.recordSt(st, ErrProviderDown)
		panic(r)
	}
}

// Stream counts mid-stream failures too: the SSE parsers surface an upstream
// error event as a Chunk{Error} and then return nil, which would otherwise be
// recorded as a success — a provider that always streams an error event could
// never trip the breaker. record() still filters by isRetryable, so permanent
// or caller-cancelled errors don't open the circuit.
//
// A half-open probe is resolved on the first non-error chunk (Azure's first
// event is a keepalive), not when the stream returns: the upstream has
// accepted the request and is answering, and every other caller of that
// resource was rejected with ErrCircuitOpen for the whole probe stream —
// tens of seconds to minutes after the provider had recovered. Once
// answered, the stream's outcome is recorded against a closed circuit: a
// later provider-side failure counts as one, a user stop or a content-filter
// stop says nothing.
func (cb *CircuitBreakerProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	st := cb.stateFor(req.Model)
	if err := cb.checkSt(st); err != nil {
		return err
	}
	defer cb.recordPanicSt(st)
	var chunkErr error
	answered := false
	err := cb.Provider.Stream(ctx, req, func(c Chunk) {
		if c.Error != nil {
			chunkErr = c.Error
		} else if !answered {
			answered = true
			cb.resolveProbeSt(st)
		}
		onChunk(c)
	})
	outcome := err
	if outcome == nil {
		outcome = chunkErr
	}
	cb.recordSt(st, outcome)
	return err
}

// resolveProbeSt closes a half-open circuit as soon as its probe is answered
// (see Stream); a no-op in any other state, so an ordinary stream's first
// chunk never resets the failure count a closed circuit is accumulating.
func (cb *CircuitBreakerProvider) resolveProbeSt(st *breakerState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.state != circuitHalfOpen {
		return
	}
	cb.transitionLocked(st, circuitClosed)
	st.failures = 0
}

// probeSaysNothing reports a half-open probe outcome that belongs to the
// caller rather than to the upstream: its credentials, its configuration,
// its request (filtered, too long, tools the model lacks) or its own
// cancellation. Keying Azure circuits by resource only (breakerScope) means
// anyone who saves a config with a victim's endpoint and a garbage key could
// otherwise race each probe window and keep the victim's circuit open with
// 401s; the same applies to every provider's shared circuits.
func probeSaysNothing(err error) bool {
	return errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrProviderConfigInvalid) ||
		errors.Is(err, ErrKeyNotConfigured) || errors.Is(err, ErrContentFiltered) ||
		errors.Is(err, ErrContextLimit) || errors.Is(err, ErrToolsUnsupported) ||
		errors.Is(err, context.Canceled)
}

// check returns ErrCircuitOpen when the circuit is open and the cooldown has
// not elapsed yet. In half-open state only ONE probe is allowed through;
// subsequent callers are rejected until the probe completes via record().
func (cb *CircuitBreakerProvider) checkSt(st *breakerState) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch st.state {
	case circuitOpen:
		if time.Since(st.lastFailure) >= cb.openDuration {
			cb.transitionLocked(st, circuitHalfOpen)
			return nil // allow the single probe
		}
		return ErrCircuitOpen
	case circuitHalfOpen:
		// A probe is already in flight — reject all others until it resolves.
		return ErrCircuitOpen
	default:
		return nil
	}
}

// record updates the failure count and circuit state based on a call outcome.
func (cb *CircuitBreakerProvider) recordSt(st *breakerState, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err == nil {
		// Any success (including the half-open probe) closes the circuit.
		cb.transitionLocked(st, circuitClosed)
		st.failures = 0
		return
	}
	// A failed half-open probe must always resolve the probe — otherwise the
	// circuit stays half-open and checkSt() rejects every caller forever.
	// Reopen and restart the cooldown regardless of whether the error is
	// "retryable": a timeout/cancel means the provider is still unhealthy, and
	// a permanent error is surfaced to the next admitted probe after the
	// cooldown rather than wedging the breaker until process restart.
	if st.state == circuitHalfOpen {
		if probeSaysNothing(err) {
			// The probe's failure is the caller's own and says nothing about
			// the upstream: release the probe without restarting the
			// cooldown (lastFailure is left as it was, so the next checkSt
			// sees it elapsed and admits the next caller as the probe).
			cb.transitionLocked(st, circuitOpen)
			return
		}
		st.lastFailure = time.Now()
		cb.transitionLocked(st, circuitOpen)
		return
	}
	// Closed circuit: only transient provider conditions count toward tripping.
	// Permanent errors (bad key, invalid request, user cancellation) must not
	// open the circuit for everyone.
	if !isRetryable(err) {
		return
	}
	// Rate-limit (429) is per-API-key, not "provider down for everyone." In
	// multi-tenant deployments each tenant has their own key, so one noisy
	// tenant's 429s must not trip the shared breaker and block healthy tenants.
	// The retry layer still backs off on 429 (isRetryable returns true); we just
	// don't count it toward the circuit-open threshold.
	if errors.Is(err, ErrRateLimited) {
		return
	}
	st.failures++
	st.lastFailure = time.Now()
	if st.failures >= cb.failureThreshold {
		cb.transitionLocked(st, circuitOpen)
	}
}

// transitionLocked sets the circuit to next, emitting a log line and metric only
// when the state actually changes. Caller must hold cb.st.mu.
func (cb *CircuitBreakerProvider) transitionLocked(st *breakerState, next circuitState) {
	if st.state == next {
		return
	}
	st.state = next
	metrics.RecordCircuitBreakerTransition(cb.providerID, next.String())
	if next == circuitClosed {
		logger.Info("AI circuit breaker closed", "provider", cb.providerID)
	} else {
		logger.Warn("AI circuit breaker transition", "provider", cb.providerID, "state", next.String(), "failures", st.failures)
	}
}
