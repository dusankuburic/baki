package ai

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"

	"github.com/google/uuid"
)

// UsageRecorder represents a function capable of saving usage metrics.
type UsageRecorder func(ctx context.Context, metric *interfaces.UsageMetric) error

type auditedProvider struct {
	inner      Provider
	recorder   UsageRecorder
	scope      string
	providerID string
	recordSem  chan struct{}
	// countTimeout overrides countTokensTimeout when non-zero (tests: proving the bound on a hung count
	// endpoint need not take the full 5 s).
	countTimeout time.Duration
	// memoScope keys the served-model memo: the user scope, plus — for alias providers (servedMemoResourcer) —
	// the resource, so a new resource's same-named deployment never inherits the old resource's mapping.
	memoScope string
	// usageOrg is the organisation whose SHARED credential this chain runs on ("" = the caller's own): a usage
	// row without an org of its own is attributed to it, so every call on an org credential is capped by its budget.
	usageOrg string
	// embedNamer names the embedding model for usage records; nil falls back to a type assertion on inner.
	embedNamer embedModelNamer
	// embedIdentifier is the RAW provider's EmbeddingIdentifier, adopted for every provider (the wrappers hide it).
	embedIdentifier EmbeddingIdentifier
	// tokenCounter is the RAW provider's exact prompt-token count capability (TokenCounter), adopted before the
	// wrappers hid it; nil means CountTokens errors and the caller falls back to the estimate.
	tokenCounter TokenCounter
	// registry is the RAW provider's model registry (an Azure AI Foundry resource), adopted before the wrappers
	// hid it: Models lists the entries the live listing lacks and applies their overrides.
	registry []registryModel
	// modelsOnce memoises the inner provider's model list for this instance's lifetime: one request consults
	// Models() up to ~8 times, once a live HTTP GET for wire-dialing providers.
	modelsOnce sync.Once
	models     []ModelInfo
	modelsErr  error
}

// NewAuditedProvider wraps an existing provider and intercepts Chat/Stream calls to record cost metrics when
// they finish. scope is the caller's key scope (user ID); providerID identifies which provider generated the usage.
const maxConcurrentRecords = 16

func NewAuditedProvider(inner Provider, recorder UsageRecorder, scope, providerID string) Provider {
	return newAuditedProvider(inner, recorder, scope, providerID)
}

// usage is one call's token usage: In/Out are the totals, CacheRead/CacheWrite the cached-input slice INCLUDED
// in In (see Response). The totals drive the meter and the row; the slice is priced at the cached rates.
type usage struct {
	In, Out, CacheRead, CacheWrite int
}

// clamped bounds a call's usage before it is priced: upstream counts can be negative, or a slice beyond the
// total it is part of. CacheRead stays within [0, In], CacheWrite within [0, In-CacheRead].
func (u usage) clamped() usage {
	u.In, u.Out = max(u.In, 0), max(u.Out, 0)
	u.CacheRead = min(max(u.CacheRead, 0), u.In)
	u.CacheWrite = min(max(u.CacheWrite, 0), u.In-u.CacheRead)
	return u
}

func newAuditedProvider(inner Provider, recorder UsageRecorder, scope, providerID string) *auditedProvider {
	return &auditedProvider{
		inner:      inner,
		recorder:   recorder,
		scope:      scope,
		providerID: providerID,
		recordSem:  make(chan struct{}, maxConcurrentRecords),
		memoScope:  scope,
	}
}

// servedMemoResourcer is implemented by alias providers whose deployment
// names are only unique within one resource (Azure AI Foundry: the endpoint).
type servedMemoResourcer interface {
	servedMemoResource() string
}

// adoptRawHints reads, from the UNWRAPPED provider (the wrappers expose only Provider), what the audited
// wrapper needs: the token counter and embedding identifier, and — for alias providers — the memo resource,
// embedding model name and registry.
func (p *auditedProvider) adoptRawHints(raw Provider) {
	if tc, ok := raw.(TokenCounter); ok {
		p.tokenCounter = tc
	}
	if id, ok := raw.(EmbeddingIdentifier); ok {
		p.embedIdentifier = id
	}
	if !aliasModelProviders[p.providerID] {
		return
	}
	if r, ok := raw.(servedMemoResourcer); ok {
		if res := r.servedMemoResource(); res != "" {
			// memoScope, not scope: a provider built from an org's shared
			// credential memoises under OrgScope (factory.wrapChainScoped).
			p.memoScope = p.memoScope + "\x00" + res
		}
	}
	if n, ok := raw.(embedModelNamer); ok {
		p.embedNamer = n
	}
	if r, ok := raw.(registryModelSource); ok {
		p.registry = r.registryModels()
	}
}

func (p *auditedProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	resp, err := p.inner.Chat(ctx, req)
	metrics.ObserveAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil {
		metrics.RecordAIError(p.providerID)
		// A context-length rejection that states the window sizes the next request to this deployment (learned_window.go).
		learnContextWindow(p.memoScope, p.providerID, req.Model, err)
		// A failed non-streamed call can still have been billed (a content-filter finish, a stopped agent run):
		// the usage rides on the error (heldUsageError), as the stream path does.
		if in, out, served, ok := usageOf(err); ok && (in > 0 || out > 0) {
			recordServedModel(p.memoScope, p.providerID, req.Model, served)
			metrics.RecordAITokens(p.providerID, in, out)
			cr, cw := cacheUsageOf(err)
			p.record(ctx, req.Model, served, req.OrgID, usage{In: in, Out: out, CacheRead: cr, CacheWrite: cw})
		}
		return resp, err
	}
	if resp != nil {
		metrics.RecordAITokens(p.providerID, resp.TokensIn, resp.TokensOut)
		recordServedModel(p.memoScope, p.providerID, req.Model, resp.ServedModel)
		p.record(ctx, req.Model, resp.ServedModel, req.OrgID, usage{
			In:         resp.TokensIn,
			Out:        resp.TokensOut,
			CacheRead:  resp.CacheReadTokens,
			CacheWrite: resp.CacheWriteTokens,
		})
	}
	return resp, err
}

func (p *auditedProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	start := time.Now()
	var tokensIn, tokensOut int
	var cacheRead, cacheWrite int
	// sawAny: any non-keepalive upstream chunk. sawContent: text or reasoning delivered. estOut is that
	// content's token estimate, summed per delta (over-counting at boundaries — the safe direction for a spend
	// cap) and folded early on long streams so memory stays bounded.
	var sawChunkErr, sawDone, sawAny, sawContent bool
	estOut := deltaEstimate{estimate: p.inner.EstimateTokens}
	var servedModel string

	wrappedOnChunk := func(c Chunk) {
		if !c.LocalKeepalive {
			sawAny = true
		}
		if c.TokensIn > 0 {
			tokensIn = c.TokensIn
		}
		if c.TokensOut > 0 {
			tokensOut = c.TokensOut
		}
		if c.CacheReadTokens > 0 || c.CacheWriteTokens > 0 {
			cacheRead, cacheWrite = c.CacheReadTokens, c.CacheWriteTokens
		}
		if c.ServedModel != "" {
			servedModel = c.ServedModel
		}
		if c.Error != nil {
			sawChunkErr = true
			learnContextWindow(p.memoScope, p.providerID, req.Model, c.Error)
		}
		if c.Text != "" {
			sawContent = true
			estOut.add(c.Text)
		}
		// Split-off reasoning is billed output too: a stream cancelled or cut while the model is still thinking
		// must not escape the estimate.
		if c.Reasoning != "" {
			sawContent = true
			estOut.add(c.Reasoning)
		}
		if c.ToolEvent != nil && c.ToolEvent.Done {
			// Hosted steps are billed by Azure per call / session / GB-day,
			// outside the token spend; count them so the gap is visible.
			metrics.RecordAIHostedToolStep(p.providerID, c.ToolEvent.Name)
		}
		if c.Done {
			sawDone = true
			recordServedModel(p.memoScope, p.providerID, req.Model, servedModel)
			// Compat servers that ignore stream_options (older vLLM/Ollama/proxies — reachable because base
			// URLs are env-overridable) end with Done(0,0); without this fallback every such stream billed $0.
			if tokensIn == 0 && tokensOut == 0 && (sawContent || len(c.ToolCalls) > 0) {
				tokensOut = estOut.total()
				tokensIn = p.estimateRequestTokens(req)
			}
			metrics.RecordAITokens(p.providerID, tokensIn, tokensOut)
			p.record(ctx, req.Model, servedModel, req.OrgID, usage{In: tokensIn, Out: tokensOut, CacheRead: cacheRead, CacheWrite: cacheWrite})
		}
		onChunk(c)
	}

	err := p.inner.Stream(ctx, req, wrappedOnChunk)
	metrics.ObserveAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil || sawChunkErr {
		metrics.RecordAIError(p.providerID)
	}
	if err != nil {
		learnContextWindow(p.memoScope, p.providerID, req.Model, err)
		// The retry wrapper re-attaches the usage a withheld pre-content error chunk carried (heldUsageError);
		// pick it up here, or a failed run's tokens are estimated instead of recorded.
		if in, out, served, ok := usageOf(err); ok {
			if in > 0 {
				tokensIn = in
			}
			if out > 0 {
				tokensOut = out
			}
			if served != "" {
				servedModel = served
			}
			if cr, cw := cacheUsageOf(err); cr > 0 || cw > 0 {
				cacheRead, cacheWrite = cr, cw
			}
		}
	}
	// A stream that ended without Done (truncation, mid-stream error, cancel) still consumed tokens; record
	// what the provider reported, falling back to estimates. Gated on evidence the request reached the provider,
	// so a circuit-open or refused connection isn't billed an estimate.
	if !sawDone && (sawAny || sawContent || tokensIn > 0 || tokensOut > 0 || sawChunkErr) {
		if tokensOut == 0 {
			tokensOut = estOut.total()
		}
		if tokensIn == 0 {
			tokensIn = p.estimateRequestTokens(req)
		}
		// The model that answered rides on every content chunk, so a stream cut before Done still teaches the memo.
		recordServedModel(p.memoScope, p.providerID, req.Model, servedModel)
		metrics.RecordAITokens(p.providerID, tokensIn, tokensOut)
		p.record(ctx, req.Model, servedModel, req.OrgID, usage{In: tokensIn, Out: tokensOut, CacheRead: cacheRead, CacheWrite: cacheWrite})
	}
	return err
}

// deltaEstimate is a stream's output-token estimate as the sum of each delta's own estimate, computed lazily
// (only when the provider reported no usage) and folded early past deltaEstimateFoldBytes/Count to bound memory.
type deltaEstimate struct {
	estimate func(string) int
	sum      int
	pending  []string
	bytes    int
}

// Fold thresholds of deltaEstimate: an answer of ordinary length is never counted unless needed; a stream past a MiB of deltas pays as it goes.
const (
	deltaEstimateFoldBytes = 1 << 20
	deltaEstimateFoldCount = 1 << 16
)

func (d *deltaEstimate) add(s string) {
	d.pending = append(d.pending, s)
	d.bytes += len(s)
	if d.bytes >= deltaEstimateFoldBytes || len(d.pending) >= deltaEstimateFoldCount {
		d.fold()
	}
}

func (d *deltaEstimate) fold() {
	for _, s := range d.pending {
		d.sum += d.estimate(s)
	}
	clear(d.pending)
	d.pending = d.pending[:0]
	d.bytes = 0
}

// total is the estimate of every delta added so far.
func (d *deltaEstimate) total() int {
	d.fold()
	return d.sum
}

// estimateRequestTokensMemoSize bounds the estimate memo: its repeats are consecutive (the next retry, the next
// tool-loop iteration), so arbitrary eviction is fine.
const estimateRequestTokensMemoSize = 16

// estimateRequestTokensMemo remembers estimateRequestTokens results by request fingerprint: each run pays the
// BPE approximation over the WHOLE request, and the no-usage fallbacks re-run it per retry and tool-loop
// iteration. Package-global (a provider per request); the key carries the providerID.
type estimateRequestTokensMemo struct {
	mu sync.Mutex
	m  map[uint64]int
}

var requestTokenEstimates = estimateRequestTokensMemo{m: map[uint64]int{}}

func (memo *estimateRequestTokensMemo) get(key uint64) (int, bool) {
	memo.mu.Lock()
	defer memo.mu.Unlock()
	n, ok := memo.m[key]
	return n, ok
}

// put remembers tokens under key, evicting one arbitrary entry when full.
func (memo *estimateRequestTokensMemo) put(key uint64, tokens int) {
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if len(memo.m) >= estimateRequestTokensMemoSize {
		for k := range memo.m {
			delete(memo.m, k)
			break
		}
	}
	memo.m[key] = tokens
}

// requestEstimateHash fingerprints every input estimateRequestTokens tokenizes — length-prefixed (a separator
// alone would let neighbouring fields splice) and hashed where they sit, never joined into one request-sized
// string; what the estimator never reads stays out, so identical counts share one entry.
func requestEstimateHash(providerID string, req Request) uint64 {
	h := fnv.New64a()
	var size [8]byte
	field := func(b []byte) {
		binary.BigEndian.PutUint64(size[:], uint64(len(b)))
		h.Write(size[:])
		h.Write(b)
	}
	field([]byte(providerID))
	field([]byte(req.SystemPrompt))
	for _, m := range req.Messages {
		field([]byte(m.Content))
		for _, tc := range m.ToolCalls {
			field(tc.Input)
		}
		field(m.ProviderItems)
	}
	return h.Sum64()
}

// estimateRequestTokens estimates a full request's input tokens: system prompt + message contents + tool-call
// arguments; shared by the no-usage fallbacks, whose repeats the memo above absorbs.
func (p *auditedProvider) estimateRequestTokens(req Request) int {
	key := requestEstimateHash(p.providerID, req)
	if n, ok := requestTokenEstimates.get(key); ok {
		return n
	}
	total := p.inner.EstimateTokens(req.SystemPrompt)
	for _, m := range req.Messages {
		c := p.inner.EstimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			c += p.inner.EstimateTokens(string(tc.Input))
		}
		// A replayed provider record is re-sent verbatim and its bytes bound the message's real context cost — take the max, not the sum.
		if len(m.ProviderItems) > 0 {
			c = max(c, p.inner.EstimateTokens(string(m.ProviderItems)))
		}
		total += c
	}
	requestTokenEstimates.put(key, total)
	return total
}

// detachedPricingTimeout bounds the background pricing step of recordDetached (a provider model listing at worst).
const detachedPricingTimeout = 10 * time.Second

func (p *auditedProvider) record(ctx context.Context, modelID, servedModel, orgID string, u usage) {
	u = u.clamped()
	if p.recorder == nil || (u.In == 0 && u.Out == 0) {
		return
	}
	metric := p.usageMetric(ctx, modelID, servedModel, orgID, u)

	// Asynchronously record the usage so it doesn't block the caller. The semaphore acquire is NON-blocking:
	// when all slots are in flight the metric is DROPPED (metrics.RecordUsageDropped) rather than parking
	// another goroutine — in practice never hit (~7 metrics per request against 16 slots), a valve against a stuck recorder.
	if !p.acquireRecordSlot(modelID) {
		return
	}
	// #nosec G118 -- usage accounting must survive the request that generated it: binding this to the caller's
	// context would drop the spend record for exactly the turns a user cancels, under-counting the daily budget
	// the chat path then enforces. The semaphore above bounds concurrency instead.
	go func() {
		defer p.releaseRecordSlot(modelID)
		p.save(&metric)
	}()
}

// recordDetached is record for callers on a short deadline (the per-turn RAG query embeds under 800 ms):
// pricing may need the live model listing, so pricing AND saving run in the background slot detached from the
// caller's cancellation, with their own timeout.
func (p *auditedProvider) recordDetached(ctx context.Context, modelID, servedModel, orgID string, u usage) {
	u = u.clamped()
	if p.recorder == nil || (u.In == 0 && u.Out == 0) {
		return
	}
	if !p.acquireRecordSlot(modelID) {
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedPricingTimeout)
	go func() {
		defer cancel()
		defer p.releaseRecordSlot(modelID)
		metric := p.usageMetric(pctx, modelID, servedModel, orgID, u)
		p.save(&metric)
	}()
}

// acquireRecordSlot takes one of the maxConcurrentRecords background slots without blocking; false (metric dropped and counted) when all are busy.
func (p *auditedProvider) acquireRecordSlot(modelID string) bool {
	select {
	case p.recordSem <- struct{}{}:
		return true
	default:
		metrics.RecordUsageDropped("saturated")
		logger.Warn("AI usage metric dropped — recorder saturated",
			"provider", p.providerID, "model", modelID)
		return false
	}
}

// releaseRecordSlot is deferred by every background record goroutine: it frees the slot and keeps a panicking
// recorder from taking the process down.
func (p *auditedProvider) releaseRecordSlot(modelID string) {
	<-p.recordSem
	if r := recover(); r != nil {
		logger.Warn("AI usage recording panicked",
			"provider", p.providerID, "model", modelID, "panic", r)
	}
}

// save writes one usage row on its own bounded context.
func (p *auditedProvider) save(metric *interfaces.UsageMetric) {
	// #nosec G118 -- intentionally detached: usage logging must outlive the request ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.recorder(ctx, metric); err != nil {
		logger.Warn("AI usage recording failed",
			"provider", p.providerID, "model", metric.Model, "error", err)
	}
}

// usageMetric prices one call's tokens and builds its usage row. A model with no known price falls back to the
// provider-wide PricePerMillionTokens — recording $0 would let the daily budget never trip.
func (p *auditedProvider) usageMetric(ctx context.Context, modelID, servedModel, orgID string, u usage) interfaces.UsageMetric {
	pricing, found := p.pricingFor(ctx, modelID, servedModel)
	if !found {
		pricing = p.inner.PricePerMillionTokens()
		metrics.RecordPricingFallback(p.providerID, pricingMetricModel(p.providerID, modelID))
		logger.Warn("AI usage priced from provider default — model not in catalog",
			"provider", p.providerID, "model", modelID, "servedModel", servedModel)
	}
	// Cached input tokens bill at their own rates (Claude reads 0.025-0.1× by family, 5-minute writes 1.25×;
	// every other route keeps 1.0; Foundry unpriced). u.In stays the TOTAL the meter and row record.
	uncached := max(u.In-u.CacheRead-u.CacheWrite, 0)
	readMul := cacheReadMultiplier(p.providerID, modelID, servedModel)
	inputCost := (float64(uncached) + readMul*float64(u.CacheRead) + cacheWriteMultiplier*float64(u.CacheWrite)) / 1000000.0 * pricing.InputCostPerM
	outputCost := (float64(u.Out) / 1000000.0) * pricing.OutputCostPerM

	if orgID == "" {
		// A chain built from an org's shared credential bills the org even when the call carried no org.
		orgID = p.usageOrg
	}

	metric := interfaces.UsageMetric{
		ID:     uuid.NewString(),
		UserID: p.scope,
		// OrgID is carried on the request, not fixed at construction, so one audited provider attributes
		// org-scoped and personal usage correctly — this is what lets GetDailyUsage enforce org budgets.
		OrgID:    orgID,
		Provider: p.providerID,
		Model:    modelID,
		// What answered, next to what was asked for (see usageServedModel).
		ServedModel:      usageServedModel(p.providerID, modelID, servedModel),
		PromptTokens:     u.In,
		CompletionTokens: u.Out,
		EstimatedCost:    inputCost + outputCost,
		CreatedAt:        time.Now(),
	}
	// Deployment-wide spend only: a user+org label would mint one permanent Prometheus series per user and
	// would take the scrape down (per-tenant attribution is the UsageMetric row above).
	metrics.RecordAIUsageCost(inputCost + outputCost)
	return metric
}

// maxUsageServedModel caps the served model stored on a usage row: it is
// whatever the upstream's "model" field said, and feeds analytics grouping.
const maxUsageServedModel = 128

// usageServedModel is the served model a usage row records next to the requested one: only alias providers
// record one (every other provider's id already names the model). Blank when none or when it normalises to the
// requested id; capped at maxUsageServedModel bytes on a rune boundary.
func usageServedModel(providerID, modelID, servedModel string) string {
	if !UsesModelAliases(providerID) {
		return ""
	}
	served := strings.TrimSpace(servedModel)
	if served == "" || normalizeServedModel(served) == normalizeServedModel(modelID) {
		return ""
	}
	if len(served) > maxUsageServedModel {
		cut := maxUsageServedModel
		for cut > 0 && !utf8.RuneStart(served[cut]) {
			cut--
		}
		served = served[:cut]
	}
	return served
}

// ReservationPricing is the rate the chat service reserves in-flight budget at for model. Alias providers
// (Azure AI Foundry) are unpriced, so nothing is reserved; every other provider keeps its provider-wide rate.
func (p *auditedProvider) ReservationPricing(ctx context.Context, model string) Pricing {
	return p.inner.PricePerMillionTokens()
}

// UsagePricing is the rate a finished turn on model (answered by served) was billed at — the same resolution
// as usageMetric, without recording — for the done payload. false only when nothing at all prices the call.
func (p *auditedProvider) UsagePricing(ctx context.Context, model, served string) (Pricing, bool) {
	if pricing, ok := p.pricingFor(ctx, model, served); ok {
		return pricing, true
	}
	if pricing := p.inner.PricePerMillionTokens(); pricing != (Pricing{}) {
		return pricing, true
	}
	return Pricing{}, false
}

// pricingFor resolves the per-million rates a call is billed at; false sends the caller to the provider-wide
// fallback. Model-name providers price by the request model's own listing entry — a live listing is
// authoritative, including deliberate $0 entries, and the served model is never consulted; alias providers
// (Foundry deployments, model-router, agents) are unpriced: a zero price that counts as found.
func (p *auditedProvider) pricingFor(ctx context.Context, modelID, servedModel string) (Pricing, bool) {
	if aliasModelProviders[p.providerID] {
		return Pricing{}, true
	}
	if models, err := p.Models(ctx); err == nil {
		for _, m := range models {
			if m.ID == modelID {
				return m.Pricing, true
			}
		}
	}
	return Pricing{}, false
}

// Delegate everything else to the inner provider
func (p *auditedProvider) SupportsTools() bool            { return p.inner.SupportsTools() }
func (p *auditedProvider) ID() string                     { return p.inner.ID() }
func (p *auditedProvider) Name() string                   { return p.inner.Name() }
func (p *auditedProvider) ContextLimit() int              { return p.inner.ContextLimit() }
func (p *auditedProvider) PricePerMillionTokens() Pricing { return p.inner.PricePerMillionTokens() }

// embedModelNamer is implemented by providers that expose the model name sent to their /embeddings-equivalent
// endpoint, so embedding usage is priced against the model actually used.
type embedModelNamer interface {
	EmbeddingModel() string
}

func (p *auditedProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	start := time.Now()
	res, err := p.inner.Embed(ctx, text)
	metrics.ObserveAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil {
		metrics.RecordAIError(p.providerID)
		return res, err
	}
	// Embedding APIs bill per INPUT token (no output): estimate the batch's input tokens and record under the
	// embedding model's name; spend attributes to the user, or the org when the credential is shared (usageOrg).
	if len(text) > 0 {
		tokensIn := 0
		for _, t := range text {
			tokensIn += p.inner.EstimateTokens(t)
		}
		model := ""
		if p.embedNamer != nil {
			model = p.embedNamer.EmbeddingModel()
		} else if namer, ok := p.inner.(embedModelNamer); ok {
			model = namer.EmbeddingModel()
		}
		if model == "" {
			model = p.inner.DefaultModel()
		}
		metrics.RecordAITokens(p.providerID, tokensIn, 0)
		// Pricing may need the live model listing; the per-turn RAG query embeds under an 800 ms budget, which
		// must never be spent on it.
		p.recordDetached(ctx, model, "", "", usage{In: tokensIn})
	}
	return res, err
}
func (p *auditedProvider) EstimateTokens(t string) int { return p.inner.EstimateTokens(t) }

// countTokensTimeout bounds one exact prompt-token count: a stalled count endpoint costs seconds, then the estimate — not the turn.
const countTokensTimeout = 5 * time.Second

// CountTokens forwards the RAW provider's exact prompt-token count (TokenCounter, adopted in adoptRawHints —
// the wrappers hide optional methods), bounded by countTokensTimeout; an error means "no exact count".
func (p *auditedProvider) CountTokens(ctx context.Context, req Request) (int, error) {
	if p.tokenCounter == nil {
		return 0, fmt.Errorf("%s: no exact token counting available", p.providerID)
	}
	timeout := countTokensTimeout
	if p.countTimeout > 0 {
		timeout = p.countTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return p.tokenCounter.CountTokens(ctx, req)
}
func (p *auditedProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	p.modelsOnce.Do(func() {
		p.models, p.modelsErr = p.inner.Models(ctx)
		if p.modelsErr == nil {
			registry := p.appliedRegistry()
			// A registered model is listed whatever the live listing returned (an agent beyond its cap, a
			// failed listing), so it is sized like a listed one below.
			p.models = addUnlistedRegistryModels(p.models, registry)
			// Deployment aliases (Azure AI Foundry) learn their served model, price and context window from
			// the model that served them last on the same resource, clamped by their own rejection's window.
			p.models = enrichServedModels(p.memoScope, p.providerID, p.models)
			// The user's own sizing of a registered model beats the catalogue, the listing and the memo — but
			// not the model's own rejection: the learned window below still clamps it.
			p.models = applyRegistryOverrides(p.models, registry)
			p.models = applyLearnedWindows(p.memoScope, p.providerID, p.models)
		}
	})
	return p.models, p.modelsErr
}

// appliedRegistry is the model registry Models applies: the one adopted from the raw provider (factory
// chains), else the inner provider's own when it was wrapped directly (NewAuditedProvider).
func (p *auditedProvider) appliedRegistry() []registryModel {
	if p.registry != nil {
		return p.registry
	}
	if r, ok := p.inner.(registryModelSource); ok {
		return r.registryModels()
	}
	return nil
}

func (p *auditedProvider) DefaultModel() string { return p.inner.DefaultModel() }
func (p *auditedProvider) FreeModel() string    { return p.inner.FreeModel() }
