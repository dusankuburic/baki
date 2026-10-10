package ai

import (
	"context"
	"errors"
	"sync"
	"time"

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

// breakerState is the circuit state for one upstream provider, shared process-wide (breakerStateFor) so the
// breaker persists across the chain rebuilds ProviderFactory.For makes on every call.
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

// breakerRegistryMax bounds the registry: keys are provider × model plus the resource for per-user endpoints
// (breakerScoper); past the bound, cold entries are dropped and the oldest evicted.
const breakerRegistryMax = 4096

// breakerStaleFailure is how long a CLOSED circuit's last failure still protects it from pruning (failures
// only reset on a same-key success, so without this the registry could never shrink).
const breakerStaleFailure = 4 * cbOpenDuration

// breakerStateFor returns the shared circuit state for a registry key, creating it on first use. State is per
// upstream, not per user: a provider that is down is down for everyone.
func breakerStateFor(key string) *breakerState {
	breakerRegistryMu.Lock()
	defer breakerRegistryMu.Unlock()
	s, ok := breakerRegistry[key]
	if !ok {
		if len(breakerRegistry) >= breakerRegistryMax {
			pruneHealthyBreakersLocked()
			// Pruning only drops cold entries; a registry full of recently failed keys stays full. Evict down
			// to max-1 so the insert below cannot push past the cap.
			evictBreakersToLocked(breakerRegistryMax - 1)
		}
		s = &breakerState{}
		breakerRegistry[key] = s
	}
	return s
}

// pruneHealthyBreakersLocked drops every closed circuit whose failure history has gone cold; open and
// half-open circuits are kept, and a dropped state an in-flight call still references just records into an orphan.
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

// evictBreakersToLocked evicts entries until the registry holds at most want: closed entries with the OLDEST
// lastFailure first, and only when no closed entry is left the oldest regardless of state.
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

// breakerScoper is implemented by providers whose upstream is per user (an endpoint the user configured —
// Azure AI Foundry resources): its scope joins the provider ID in the registry key, since a 5xx from one
// tenant's resource says nothing about another's. The scope holds no secret and never reaches logs.
type breakerScoper interface {
	breakerScope() string
}

func breakerScopeOf(p Provider) string {
	if s, ok := p.(breakerScoper); ok {
		return s.breakerScope()
	}
	return ""
}

// breakerKey scopes circuit state per model: provider families mix per-model health (a deprecated model ID
// 500-ing while its siblings are fine), and a provider-wide key meant one bad model blocked every user.
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

// CircuitBreakerProvider opens the circuit after failureThreshold consecutive retryable failures; while open,
// calls fail immediately with ErrCircuitOpen. After openDuration one probe is allowed — success closes, a
// failure reopens. Permanent errors (auth, config) never trip it. State lives in a shared *breakerState
// (provider ID + model + endpoint scope), so every provider for the same upstream observes the same circuit.
type CircuitBreakerProvider struct {
	Provider
	providerID string
	// scope is the wrapped provider's breakerScope ("" for shared
	// upstreams). It only keys the state; metrics and logs keep providerID.
	scope            string
	failureThreshold int
	openDuration     time.Duration
}

// stateFor resolves the shared circuit for this call's scope. Chat/Stream key by the request's model
// (breakerKey); anything without a model uses the provider-wide key — per resource for a scoped provider.
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

// recordPanic resolves the circuit when the wrapped provider panics — without it a half-open PROBE is never
// resolved and check() rejects every caller forever. On the normal path recover() returns nil (no double-record).
func (cb *CircuitBreakerProvider) recordPanicSt(st *breakerState) {
	if r := recover(); r != nil {
		cb.recordSt(st, ErrProviderDown)
		panic(r)
	}
}

// Stream counts mid-stream failures too: the SSE parsers surface an upstream error as Chunk{Error} and then
// return nil, which would otherwise be a recorded success. A half-open probe is resolved on the first
// non-error chunk (the upstream has accepted the request; every other caller was rejected for the whole probe).
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

// resolveProbeSt closes a half-open circuit as soon as its probe is answered (see Stream); a no-op in any
// other state, so an ordinary stream's first chunk never resets the failure count a closed circuit accumulates.
func (cb *CircuitBreakerProvider) resolveProbeSt(st *breakerState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.state != circuitHalfOpen {
		return
	}
	cb.transitionLocked(st, circuitClosed)
	st.failures = 0
}

// probeSaysNothing reports a half-open probe outcome that belongs to the caller rather than the upstream
// (credentials, configuration, request, cancellation). Keying Azure circuits by resource only, anyone who
// saves a victim's endpoint with a garbage key could otherwise race each probe window and keep the victim's
// circuit open with 401s.
func probeSaysNothing(err error) bool {
	return errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrProviderConfigInvalid) ||
		errors.Is(err, ErrKeyNotConfigured) || errors.Is(err, ErrContentFiltered) ||
		errors.Is(err, ErrContextLimit) || errors.Is(err, ErrToolsUnsupported) ||
		errors.Is(err, context.Canceled)
}

// check returns ErrCircuitOpen when the circuit is open and the cooldown has not elapsed yet. In half-open
// state only ONE probe is allowed through; subsequent callers are rejected until the probe completes.
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
	// A failed half-open probe must always resolve the probe, or the circuit stays half-open and rejects
	// every caller forever: reopen and restart the cooldown regardless of retryability.
	if st.state == circuitHalfOpen {
		if probeSaysNothing(err) {
			// The probe's failure is the caller's own and says nothing about the upstream: release the probe
			// without restarting the cooldown, so the next checkSt admits the next caller as the probe.
			cb.transitionLocked(st, circuitOpen)
			return
		}
		st.lastFailure = time.Now()
		cb.transitionLocked(st, circuitOpen)
		return
	}
	// Closed circuit: only transient provider conditions count toward tripping; permanent errors must not
	// open the circuit for everyone.
	if !isRetryable(err) {
		return
	}
	// Rate-limit (429) is per-API-key, not "provider down for everyone": in multi-tenant deployments one
	// noisy tenant's 429s must not trip the shared breaker; the retry layer still backs off on them.
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
	recordCircuitBreakerTransition(cb.providerID, next.String())
	if next == circuitClosed {
		logger.Info("AI circuit breaker closed", "provider", cb.providerID)
	} else {
		logger.Warn("AI circuit breaker transition", "provider", cb.providerID, "state", next.String(), "failures", st.failures)
	}
}
