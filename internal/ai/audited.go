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

	"pad-analyzer/internal/lifecycle"
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
	// row without an org of its own is attributed to it.
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

// NewAuditedProvider wraps an existing provider and intercepts Chat/Stream calls to record token usage when
// they finish. scope is the caller's key scope (user ID); providerID identifies which provider generated the usage.
const maxConcurrentRecords = 16

func NewAuditedProvider(inner Provider, recorder UsageRecorder, scope, providerID string) Provider {
	return newAuditedProvider(inner, recorder, scope, providerID)
}

// usage is one call's token usage: the input and output totals the meter and the row record.
type usage struct {
	In, Out int
}

// clamped bounds a call's usage before it is recorded: upstream counts can be negative.
func (u usage) clamped() usage {
	u.In, u.Out = max(u.In, 0), max(u.Out, 0)
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

// user is who a call under ctx is recorded for: the context's usage scope, else the chain's scope with the
// request's org (reqOrgID) or, without one, the shared credential's.
func (p *auditedProvider) user(ctx context.Context, reqOrgID string) (scope, orgID string) {
	if s, o, ok := UsageScopeFrom(ctx); ok {
		return s, o
	}
	if reqOrgID == "" {
		reqOrgID = p.usageOrg
	}
	return p.scope, reqOrgID
}

func (p *auditedProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	resp, err := p.inner.Chat(ctx, req)
	observeAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil {
		recordAIError(p.providerID)
		// A context-length rejection that states the window sizes the next request to this deployment (learned_window.go).
		learnContextWindow(p.memoScope, p.providerID, req.Model, err)
		// A failed non-streamed call can still have been billed (a content-filter finish, a stopped agent run):
		// the usage rides on the error (heldUsageError), as the stream path does.
		if in, out, served, ok := usageOf(err); ok && (in > 0 || out > 0) {
			recordServedModel(p.memoScope, p.providerID, req.Model, served)
			recordAITokens(p.providerID, in, out)
			p.record(ctx, req.Model, served, req.OrgID, usage{In: in, Out: out})
		}
		return resp, err
	}
	if resp != nil {
		recordAITokens(p.providerID, resp.TokensIn, resp.TokensOut)
		recordServedModel(p.memoScope, p.providerID, req.Model, resp.ServedModel)
		p.record(ctx, req.Model, resp.ServedModel, req.OrgID, usage{In: resp.TokensIn, Out: resp.TokensOut})
	}
	return resp, err
}

func (p *auditedProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	start := time.Now()
	var tokensIn, tokensOut int
	// sawAny: any non-keepalive upstream chunk. sawContent: text or reasoning delivered. estOut is that
	// content's token estimate, summed per delta (over-counting at boundaries) and folded early on long streams
	// so memory stays bounded.
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
		// Split-off reasoning is output too: a stream cancelled or cut while the model is still thinking must
		// not escape the estimate.
		if c.Reasoning != "" {
			sawContent = true
			estOut.add(c.Reasoning)
		}
		if c.ToolEvent != nil && c.ToolEvent.Done {
			// Hosted steps are billed by Azure per call / session / GB-day,
			// outside the token counts; count them so they are visible.
			recordAIHostedToolStep(p.providerID, c.ToolEvent.Name)
		}
		if c.Done {
			sawDone = true
			recordServedModel(p.memoScope, p.providerID, req.Model, servedModel)
			// Compat servers that ignore stream_options (older vLLM/Ollama/proxies — reachable because base
			// URLs are env-overridable) end with Done(0,0); without this fallback every such stream recorded no tokens.
			if tokensIn == 0 && tokensOut == 0 && (sawContent || len(c.ToolCalls) > 0) {
				tokensOut = estOut.total()
				tokensIn = p.estimateRequestTokens(req)
			}
			recordAITokens(p.providerID, tokensIn, tokensOut)
			p.record(ctx, req.Model, servedModel, req.OrgID, usage{In: tokensIn, Out: tokensOut})
		}
		onChunk(c)
	}

	err := p.inner.Stream(ctx, req, wrappedOnChunk)
	observeAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil || sawChunkErr {
		recordAIError(p.providerID)
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
		}
	}
	// A stream that ended without Done (truncation, mid-stream error, cancel) still consumed tokens; record
	// what the provider reported, falling back to estimates. Gated on evidence the request reached the provider,
	// so a circuit-open or refused connection records no estimate.
	if !sawDone && (sawAny || sawContent || tokensIn > 0 || tokensOut > 0 || sawChunkErr) {
		if tokensOut == 0 {
			tokensOut = estOut.total()
		}
		if tokensIn == 0 {
			tokensIn = p.estimateRequestTokens(req)
		}
		// The model that answered rides on every content chunk, so a stream cut before Done still teaches the memo.
		recordServedModel(p.memoScope, p.providerID, req.Model, servedModel)
		recordAITokens(p.providerID, tokensIn, tokensOut)
		p.record(ctx, req.Model, servedModel, req.OrgID, usage{In: tokensIn, Out: tokensOut})
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

func (p *auditedProvider) record(ctx context.Context, modelID, servedModel, orgID string, u usage) {
	u = u.clamped()
	if p.recorder == nil || (u.In == 0 && u.Out == 0) {
		return
	}
	metric := p.usageMetric(modelID, servedModel, orgID, u)

	// Asynchronously record the usage so it doesn't block the caller. The semaphore acquire is NON-blocking:
	// when all slots are in flight the metric is DROPPED (recordUsageDropped) rather than parking
	// another goroutine — in practice never hit (~7 metrics per request against 16 slots), a valve against a stuck recorder.
	if !p.acquireRecordSlot(modelID) {
		return
	}
	// Usage accounting must survive the request that generated it: binding this to the caller's context would
	// drop the record for exactly the turns a user cancels. usageRecords drains it at shutdown; the semaphore
	// above bounds concurrency.
	p.goRecord(modelID, func() { p.save(&metric) })
}

// usageRecords runs every provider's background usage writes, so shutdown waits for the rows in flight (they
// used to be bare goroutines a deploy cut off) and a panicking recorder is reported like any other.
var usageRecords = lifecycle.NewBounded("ai_usage_record", maxConcurrentUsageRecords)

// maxConcurrentUsageRecords caps usage writes across all providers; each wrapper is also capped at
// maxConcurrentRecords.
const maxConcurrentUsageRecords = 1024

// goRecord runs fn on usageRecords holding one of p's record slots; the slot is released when fn returns, or
// at once when the write cannot start (shutting down).
func (p *auditedProvider) goRecord(modelID string, fn func()) {
	if !usageRecords.Go(func() {
		defer p.releaseRecordSlot()
		fn()
	}) {
		// The runner refused: at its process-wide cap or draining for shutdown (it counts which).
		p.releaseRecordSlot()
		recordUsageDropped("runner")
		logger.Warn("AI usage metric dropped — usage runner refused it", "provider", p.providerID, "model", modelID)
	}
}

// acquireRecordSlot takes one of the maxConcurrentRecords background slots without blocking; false (metric dropped and counted) when all are busy.
func (p *auditedProvider) acquireRecordSlot(modelID string) bool {
	select {
	case p.recordSem <- struct{}{}:
		return true
	default:
		recordUsageDropped("saturated")
		logger.Warn("AI usage metric dropped — recorder saturated",
			"provider", p.providerID, "model", modelID)
		return false
	}
}

// releaseRecordSlot frees a slot taken by acquireRecordSlot. It is deferred by every background record, so a
// panicking recorder (reported by usageRecords) cannot keep its slot.
func (p *auditedProvider) releaseRecordSlot() {
	<-p.recordSem
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

// usageMetric builds one call's usage row.
func (p *auditedProvider) usageMetric(modelID, servedModel, orgID string, u usage) interfaces.UsageMetric {
	if orgID == "" {
		// A chain built from an org's shared credential records the org even when the call carried no org.
		orgID = p.usageOrg
	}

	metric := interfaces.UsageMetric{
		ID:     uuid.NewString(),
		UserID: p.scope,
		// OrgID is carried on the request, not fixed at construction, so one audited provider attributes
		// org-scoped and personal usage correctly.
		OrgID:    orgID,
		Provider: p.providerID,
		Model:    modelID,
		// What answered, next to what was asked for (see usageServedModel).
		ServedModel:      usageServedModel(p.providerID, modelID, servedModel),
		PromptTokens:     u.In,
		CompletionTokens: u.Out,
		CreatedAt:        time.Now(),
	}
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

// Delegate everything else to the inner provider
func (p *auditedProvider) SupportsTools() bool { return p.inner.SupportsTools() }
func (p *auditedProvider) ID() string          { return p.inner.ID() }
func (p *auditedProvider) Name() string        { return p.inner.Name() }
func (p *auditedProvider) ContextLimit() int   { return p.inner.ContextLimit() }

// embedModelNamer is implemented by providers that expose the model name sent to their /embeddings-equivalent
// endpoint, so embedding usage is recorded under the model actually used.
type embedModelNamer interface {
	EmbeddingModel() string
}

func (p *auditedProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	// Embed carries no Request: the model is the embedding model named below, the org the context's.
	start := time.Now()
	res, err := p.inner.Embed(ctx, text)
	observeAIRequest(p.providerID, time.Since(start).Seconds())
	if err != nil {
		recordAIError(p.providerID)
		return res, err
	}
	// Embedding APIs count INPUT tokens only: estimate the batch's input tokens and record under the embedding
	// model's name, for the context's usage scope (an org corpus embedded through a member's own key is the
	// org's), else the shared credential's org (usageOrg).
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
		recordAITokens(p.providerID, tokensIn, 0)
		_, orgID := p.user(ctx, "")
		p.record(ctx, model, "", orgID, usage{In: tokensIn})
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
			// Deployment aliases (Azure AI Foundry) learn their served model and context window from
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
