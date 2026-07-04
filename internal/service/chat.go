package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/rag"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/ai/scrubber"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// maxChatStreamDuration is the hard wall-clock cap on a stream, tool loop
// included. It is a backstop, not the primary guard: abandoned clients are
// cancelled by the subscriber watchdog and hung providers by the idle timeout,
// so the cap can be generous enough that a long healthy answer isn't cut.
const maxChatStreamDuration = 10 * time.Minute

// maxChatContextCache bounds the cached scrubbed-context entries. Each entry
// holds a scrubbed (redacted) flow clone plus the built system/context text;
// the cost is dominated by the deep clone + per-block scrub walk + token
// estimation in BuildContext, none of which change across consecutive turns in
// the same thread. A small per-scope bound covers a few concurrent threads.
const maxChatContextCache = 32

// ragGuidelinesDeadline caps how long the streaming chat path waits for RAG
// knowledge-base guidelines before first token. RAG issues a synchronous
// embedding API call + vector query; on a slow embedding provider this would
// otherwise gate the whole turn. On timeout the turn proceeds without
// guidelines (skip-on-timeout) rather than stalling the user.
const ragGuidelinesDeadline = 800 * time.Millisecond

// chunkBatchInterval caps how long the chunk coalescer holds small deltas
// before flushing them as one merged "chunk" event. Each SSE event carries
// ~115 bytes of framing, so token-at-a-time providers spend most of the wire
// budget on framing; batching for one frame (~16ms) cuts that by an order of
// magnitude on fast streams. The first chunk of a stream and every terminal /
// non-chunk event (done/error/tool) bypass the batch, so first-token latency
// and completion ordering are unaffected.
const chunkBatchInterval = 16 * time.Millisecond

// subscriberCheckInterval is how often the stream watchdog verifies the
// stream's owner still has a live SSE connection; subscriberMissLimit is how
// many consecutive failed checks cancel the stream. Two 15s ticks give a
// closed-then-reopened tab (and the SSE client's reconnect backoff) ~30s of
// grace before an abandoned stream stops billing provider tokens.
const (
	subscriberCheckInterval = 15 * time.Second
	subscriberMissLimit     = 2
)

// streamIdleTimeout cancels a stream whose provider has gone silent — no
// chunk of any kind — for this long. A healthy model emits at least metadata
// every few seconds; 90s of nothing means the upstream hung, and waiting out
// the full wall-clock cap would just stall the user silently.
const streamIdleTimeout = 90 * time.Second

// maxConcurrentStreamsPerScope caps how many chat streams one caller may run
// in parallel (the UI streams one per thread). It bounds a single user's
// provider spend and goroutine footprint regardless of client behaviour.
const maxConcurrentStreamsPerScope = 3

// contextReserve is the number of tokens kept free for the prompt/context when
// clamping a caller-supplied MaxTokens against the model's context window.
const contextReserve = 4000

// normalizeChatParams clamps caller-supplied generation params into safe ranges
// so an out-of-range value can't reach the provider (some reject Temperature
// outside [0,2] or a MaxTokens above the model's limits with a 400). It is pure
// so it can be unit-tested independently of provider wiring.
//
// maxTokens is an *output* cap, so it must be bounded by the model's output
// ceiling (maxOutput) — not the input context window. We still keep the older
// ctxLimit-contextReserve guard as a backstop for models whose output ceiling
// is unknown (maxOutput == 0). A value of 0 for either limit means "unknown" and
// that particular clamp is skipped.
func normalizeChatParams(temperature float64, maxTokens, ctxLimit, maxOutput int) (float64, int) {
	if temperature < 0 {
		temperature = 0
	} else if temperature > 2 {
		temperature = 2
	}
	if maxTokens < 0 {
		maxTokens = 0
	}
	// Prefer the model's real output ceiling when known.
	if maxOutput > 0 && maxTokens > maxOutput {
		maxTokens = maxOutput
	}
	// Backstop: never let MaxTokens approach the input window (only meaningful
	// when the output ceiling is unknown, but harmless otherwise).
	if ctxLimit > 0 {
		cap := ctxLimit - contextReserve
		if cap < 0 {
			cap = 0
		}
		if maxTokens > cap {
			maxTokens = cap
		}
	}
	return temperature, maxTokens
}

// dailyUsage returns today's AI spend used for the budget check. The storage
// backend is nil in local/desktop mode (which has no usage store), so this
// returns (0, nil) there. A non-nil error means the spend could not be verified
// and the caller must fail closed (deny) rather than treat it as 0 — otherwise
// a DB hiccup silently removes the cost guardrail.
func (s *ChatService) dailyUsage(ctx context.Context, scope, orgID string) (float64, error) {
	if s.backend == nil {
		return 0, nil
	}
	usage, err := s.backend.GetDailyUsage(ctx, scope, orgID)
	if err != nil {
		return 0, err
	}
	return usage, nil
}

// resumeRetention is how long a finished stream's buffer is kept so a client
// that was disconnected when the stream ended can still fetch the final text
// (and done/error) on reconnect via ResumeStream.
const resumeRetention = 2 * time.Minute

type streamCtl struct {
	cancel context.CancelFunc
	// started is closed (once) by BeginStream so every waiter — the emit gate
	// and the subscriber watchdog — unblocks together.
	started   chan struct{}
	startOnce sync.Once
	mu        sync.Mutex
	buffer    strings.Builder
	done      bool   // stream finished successfully
	errMsg    string // non-empty if the stream ended with an error
	tokensIn  int
	tokensOut int
	ownerID   string // caller identity (scope) that created this stream
	// cancelReason is the client-facing explanation for a deliberate
	// cancellation (user stop, watchdog, shutdown); it replaces the provider's
	// raw "context canceled" wrapping when the failure is reported.
	cancelReason string
	// lastActivity is the UnixNano of the most recent provider chunk (any
	// kind, before scrub holdback), read by the idle check in watchStream.
	lastActivity atomic.Int64
}

// touch records provider activity for the idle timeout.
func (c *streamCtl) touch() { c.lastActivity.Store(time.Now().UnixNano()) }

// cancelWithReason records why the stream is being deliberately cancelled,
// then cancels it. The first reason wins.
func (c *streamCtl) cancelWithReason(reason string) {
	c.mu.Lock()
	if c.cancelReason == "" {
		c.cancelReason = reason
	}
	c.mu.Unlock()
	c.cancel()
}

// failureMessage returns the client-facing message for a stream error. A
// deliberate cancellation surfaces its stored reason, and the stream-duration
// timeout gets a readable message — everything else is the error as-is.
func (c *streamCtl) failureMessage(ctx context.Context, err error) string {
	c.mu.Lock()
	reason := c.cancelReason
	c.mu.Unlock()
	if ctx.Err() != nil {
		if reason != "" {
			return reason
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "response stopped: maximum response time reached"
		}
	}
	return err.Error()
}

// snapshot captures the stream's resumable state under lock, for mirroring to
// the resume backplane and for building a ResumeResult.
func (c *streamCtl) snapshot() resumeSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return resumeSnapshot{
		Owner:     c.ownerID,
		Text:      c.buffer.String(),
		Done:      c.done,
		Error:     c.errMsg,
		TokensIn:  c.tokensIn,
		TokensOut: c.tokensOut,
	}
}

// scrubbedEmitter forwards model output to the client with secrets masked.
// Each text delta passes through a StreamScrubber — so a secret split across
// chunk boundaries is still caught — before being appended to the resume
// buffer and emitted as a chunk event. flush must be called at stream end
// (done or error) so text still held by the scrubber is delivered. The model's
// raw output never reaches the client or the resume buffer unscrubbed. (The
// emitted-chunk count for the done event's dropped-chunk detection lives on
// chunkCoalescer, which is the one that actually emits over SSE.)
type scrubbedEmitter struct {
	ctl   *streamCtl
	emit  func(string, map[string]interface{})
	scrub *scrubber.StreamScrubber
}

func newScrubbedEmitter(ctl *streamCtl, emit func(string, map[string]interface{})) *scrubbedEmitter {
	return &scrubbedEmitter{ctl: ctl, emit: emit, scrub: scrubber.NewStreamScrubber()}
}

func (e *scrubbedEmitter) text(t string) { e.push(e.scrub.Write(t)) }
func (e *scrubbedEmitter) flush()        { e.push(e.scrub.Flush()) }

func (e *scrubbedEmitter) push(t string) {
	if t == "" {
		return
	}
	e.ctl.mu.Lock()
	e.ctl.buffer.WriteString(t)
	e.ctl.mu.Unlock()
	e.emit("chunk", map[string]interface{}{"content": t})
}

// chunkCoalescer batches consecutive "chunk" events to cut SSE framing
// overhead on fast token streams. Each SSE event carries ~115 bytes of framing
// regardless of payload size; a token-at-a-time provider emitting 5-char deltas
// spends ~95% of the wire budget on framing. The coalescer merges deltas for up
// to chunkBatchInterval before forwarding a single merged "chunk" event.
//
// Ordering guarantees:
//   - The FIRST chunk of a stream is emitted immediately so the user sees the
//     first token without a frame of added latency.
//   - Non-chunk events (done/error/tool) flush the pending batch FIRST, then
//     pass through, so chunks always precede the terminal event in order.
//   - flush is goroutine-safe: the batch timer fires from time.AfterFunc's
//     goroutine while the worker thread drives flush via non-chunk events.
//
// The emitted-chunk count (count) replaces scrubbedEmitter.chunks in the done
// event's dropped-chunk field so the client's received-vs-expected check stays
// meaningful (it now counts EMITTED events, which is what the client observes).
type chunkCoalescer struct {
	emit  func(string, map[string]interface{}) // raw notifier emit (NOT the wrapped one)
	mu    sync.Mutex
	buf   strings.Builder
	timer *time.Timer
	first bool
	count int // emitted chunk events (for done's dropped-chunk detection)
}

func newChunkCoalescer(emit func(string, map[string]interface{})) *chunkCoalescer {
	return &chunkCoalescer{emit: emit, first: true}
}

// wrap returns an emit-shaped function that routes "chunk" through the batch
// and flushes before any other event type. Bind this as the worker's emit so
// every event (scrubbedEmitter + direct done/error/tool) routes through here.
func (c *chunkCoalescer) wrap() func(string, map[string]interface{}) {
	return func(eventType string, data map[string]interface{}) {
		if eventType != "chunk" {
			c.flush()
			c.emit(eventType, data)
			return
		}
		content, _ := data["content"].(string)
		if content == "" {
			return
		}
		c.add(content)
	}
}

// add appends a delta to the batch, or emits immediately for the stream's first
// delta (first-token latency protection).
func (c *chunkCoalescer) add(content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.first {
		c.first = false
		c.count++
		c.emit("chunk", map[string]interface{}{"content": content})
		return
	}
	c.buf.WriteString(content)
	if c.timer == nil {
		c.timer = time.AfterFunc(chunkBatchInterval, c.flush)
	}
}

// flush emits any pending batch. Safe for concurrent use (timer goroutine +
// worker). A no-op when nothing is pending.
func (c *chunkCoalescer) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.buf.Len() == 0 {
		return
	}
	content := c.buf.String()
	c.buf.Reset()
	c.count++
	c.emit("chunk", map[string]interface{}{"content": content})
}

// flushAndCount flushes the pending batch and returns the total emitted-chunk
// count, for the done event's dropped-chunk detection field.
func (c *chunkCoalescer) flushAndCount() int {
	c.flush()
	c.mu.Lock()
	n := c.count
	c.mu.Unlock()
	return n
}

// ResumeResult is the buffered state of a stream returned to a reconnecting client.
type ResumeResult struct {
	Text      string `json:"text"`
	Done      bool   `json:"done"`
	Error     string `json:"error"`
	TokensIn  int    `json:"tokensIn"`
	TokensOut int    `json:"tokensOut"`
}

// watchdogTick returns the subscriber-check interval, honouring the test
// override so watchdog tests don't sleep for real 15s ticks.
func (s *ChatService) watchdogTick() time.Duration {
	if s.watchdogInterval > 0 {
		return s.watchdogInterval
	}
	return subscriberCheckInterval
}

// idleLimit returns the provider-idle timeout, honouring the test override.
func (s *ChatService) idleLimit() time.Duration {
	if s.idleTimeout > 0 {
		return s.idleTimeout
	}
	return streamIdleTimeout
}

// watchStream is the stream watchdog. Two independent guards share one ticker:
//
//   - Idle: the provider has emitted nothing for idleLimit — the upstream is
//     hung, and waiting out the wall-clock cap would just stall the user
//     silently. Active from stream start (a provider can hang before /begin).
//   - Subscriber: a closed tab can't send /cancel, and the stream context is
//     deliberately detached from the request, so once the client has begun,
//     cancel when the owner has had no SSE connection for subscriberMissLimit
//     consecutive checks (~30s grace covers the SSE reconnect backoff).
//
// Exits with the stream via ctx, which the worker's deferred cleanup cancels.
func (s *ChatService) watchStream(ctx context.Context, streamID, scope string, ctl *streamCtl) {
	ticker := time.NewTicker(s.watchdogTick())
	defer ticker.Stop()
	begun := false
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if idle := time.Since(time.Unix(0, ctl.lastActivity.Load())); idle > s.idleLimit() {
				logger.Info("chat stream cancelled: provider idle", "streamID", streamID, "idle", idle)
				ctl.cancelWithReason("response stopped: the AI provider stopped responding")
				return
			}
			if !begun {
				select {
				case <-ctl.started:
					begun = true
				default:
					continue
				}
			}
			if s.notifier.HasSubscriber(scope) {
				misses = 0
				continue
			}
			misses++
			if misses >= subscriberMissLimit {
				logger.Info("chat stream cancelled: client SSE connection gone", "streamID", streamID)
				ctl.cancelWithReason("response stopped: you were disconnected while it was generating")
				return
			}
		}
	}
}

// ChatService owns chat stream state and operations.
type ChatService struct {
	notifier      Notifier
	configDir     string
	flowCache     *FlowService
	analysisCache *AnalysisService
	settings      SettingsProvider
	factory       *ai.ProviderFactory
	demoLimiter   *ai.DemoLimiter
	backend       storageif.StorageBackend
	// mode selects where conversations persist: ModeLocal uses the on-disk file
	// store (desktop), cloud routes through the backend (Postgres + RLS).
	mode      config.DeploymentMode
	knowledge *rag.KnowledgeService
	// watchdogInterval / idleTimeout override subscriberCheckInterval and
	// streamIdleTimeout in tests; 0 ⇒ defaults.
	watchdogInterval time.Duration
	idleTimeout      time.Duration
	activeStreams    sync.Map // map[streamID]*streamCtl — in-flight streams
	// finishedStreams holds recently-completed streams for a short grace period
	// (resumeRetention) so a client that reconnects after a stream ended can
	// still fetch its final buffer via ResumeStream.
	finishedStreams sync.Map // map[streamID]*streamCtl
	// resume mirrors resumable stream state to a shared backplane so a client
	// that reconnects to a DIFFERENT replica can still fetch the buffer. The
	// default is a no-op (single-replica); SetResumeBackplane swaps in Redis.
	resume resumeStore

	// chatCtxCache memoises the scrubbed document + built system/context text
	// (the expensive, turn-invariant part of buildScrubbedContext) across
	// consecutive turns in a thread. The cached value is the REDACTED output of
	// scrubber.ScrubDocument, so it carries no secrets. Keys are per-scope and
	// embed a per-flow generation counter (chatCtxGen) so an in-place flow edit
	// (InvalidateChatContext) cheaply invalidates without enumerating keys.
	chatCtxCache cache.Cache
	chatCtxGen   sync.Map // flowID → uint64 generation
}

func NewChatService(
	notifier Notifier,
	configDir string,
	flowCache *FlowService,
	analysisCache *AnalysisService,
	settings SettingsProvider,
	factory *ai.ProviderFactory,
	demoLimiter *ai.DemoLimiter,
	backend storageif.StorageBackend,
	mode config.DeploymentMode,
) *ChatService {
	return &ChatService{
		notifier:      notifier,
		configDir:     configDir,
		flowCache:     flowCache,
		analysisCache: analysisCache,
		settings:      settings,
		factory:       factory,
		demoLimiter:   demoLimiter,
		backend:       backend,
		mode:          mode,
		resume:        noopResumeStore{},
		chatCtxCache:  newChatContextCache(),
	}
}

// newChatContextCache builds the bounded LRU for scrubbed-context memoisation.
// size > 0 ⇒ the constructor error is impossible, so it is swallowed.
func newChatContextCache() cache.Cache {
	c, _ := cache.NewLRUCache(maxChatContextCache)
	return c
}

// InvalidateChatContext drops any cached scrubbed context for a flow. Call on
// in-place content updates (mirrors FlowService.InvalidateSearchIndex). It
// bumps a per-flow generation counter rather than enumerating keys; stale
// entries become unreachable (different key) and age out via the LRU bound.
func (s *ChatService) InvalidateChatContext(flowID string) {
	if s.chatCtxCache == nil {
		return
	}
	v, _ := s.chatCtxGen.LoadOrStore(flowID, uint64(0))
	s.chatCtxGen.Store(flowID, v.(uint64)+1)
}

// chatContextKey builds the cache key for a turn's scrubbed context. It mixes
// every input to the turn-invariant core of buildScrubbedContext: the owning
// scope (authz isolation), the flow's edit-generation, the selected block /
// system-prompt suffix, the provider+model (token math differs), and a cheap
// fingerprint of the analysis report (regenerated → GeneratedAt moves).
func (s *ChatService) chatContextKey(scope, flowID string, req models.ChatRequest, providerID, model string, report *models.AnalysisReport) string {
	gen, _ := s.chatCtxGen.LoadOrStore(flowID, uint64(0))
	reportFP := ""
	if report != nil {
		reportFP = fmt.Sprintf("%d-%d", report.GeneratedAt.UnixNano(), len(report.Findings))
	}
	// Include the selected source files so a different selection (or a change
	// to a file's contents between flow reloads) misses the cache. Sort for a
	// stable key regardless of selection order.
	sourceFP := sourceFilesFingerprint(req.SelectedSourceFiles)
	// ExcludeContext changes what computeContextCore produces (it gates both the
	// SelectedBlock and RawSourceFiles context injection), so it MUST be in the
	// key — otherwise a free-form turn caches a source/block-less contextText
	// that a later context-bearing turn (same key) would reuse.
	exclude := fmt.Sprintf("%t", req.ExcludeContext)
	return strings.Join([]string{
		scope, flowID, fmt.Sprintf("%d", gen.(uint64)),
		req.ContextBlockID, req.SystemPrompt, providerID, model, reportFP, sourceFP, exclude,
	}, "|")
}

// sourceFilesFingerprint reduces the selected-source-files list to a stable
// cache-key fragment (sorted names joined by ";"). Contents aren't hashed — a
// flow reload bumps the per-flow generation in the key, covering on-disk edits.
func sourceFilesFingerprint(files []string) string {
	if len(files) == 0 {
		return ""
	}
	cp := make([]string, len(files))
	copy(cp, files)
	sort.Strings(cp)
	return strings.Join(cp, ";")
}

// chatContextValue is the cached payload: the redacted flow clone plus the
// pre-RAG system prompt and context text. RAG depends on the per-turn user
// message and is appended after the cache lookup (see buildScrubbedContext).
// The scrubbed document is treated as read-only by all downstream consumers
// (BuildContext, the tool loop), so sharing one clone across turns is safe.
type chatContextValue struct {
	scrubbedDoc *models.FlowDocument
	sysPrompt   string
	contextText string
}

func (s *ChatService) SetKnowledgeService(ks *rag.KnowledgeService) {
	s.knowledge = ks
}

// resumeBackplane returns the configured resume store, or a no-op when unset
// (a ChatService built via a struct literal — e.g. in tests — has a nil field).
func (s *ChatService) resumeBackplane() resumeStore {
	if s.resume == nil {
		return noopResumeStore{}
	}
	return s.resume
}

// SetResumeBackplane enables cross-replica stream resume backed by Redis. A nil
// client leaves the single-replica default (resume served from local maps only),
// mirroring the rate limiter's nil-client fallback so multi-replica wiring is
// opt-in via PAD_REDIS_URL and desktop/single-instance behavior is unchanged.
func (s *ChatService) SetResumeBackplane(client *redis.Client) {
	if client == nil {
		return
	}
	s.resume = redisResumeStore{c: client}
}

func (s *ChatService) GetAuthorizedFlow(ctx context.Context, flowID, userID, minPerm string) (*models.FlowDocument, error) {
	return s.flowCache.GetAuthorized(ctx, flowID, userID, minPerm)
}

// buildScrubbedContext scrubs the document, builds the system prompt and context
// text, augments the system prompt with RAG knowledge-base guidelines, and
// scrubs both strings. It also returns the scrubbed document so the caller can
// hand it to the tool loop. Shared by StreamChatMessage and PreviewContext so
// the two context-preparation paths cannot drift apart.
//
// This is the SYNCHRONOUS path (RAG blocks). The streaming worker instead uses
// cachedContextCore + a concurrent, deadline-bounded ragGuidelines call so a
// slow embedding provider cannot gate first token (C-3).
func (s *ChatService) buildScrubbedContext(ctx context.Context, scope string, provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (scrubbedDoc *models.FlowDocument, sysPrompt, contextText string) {
	cv := s.cachedContextCore(ctx, scope, provider, doc, report, req)
	sysPrompt = cv.sysPrompt
	if guidelines := s.ragGuidelines(ctx, scope, doc, req.UserMessage); guidelines != "" {
		sysPrompt += "\n\n" + guidelines
	}
	return cv.scrubbedDoc, scrubber.ScrubText(sysPrompt), scrubber.ScrubText(cv.contextText)
}

// ragGuidelines returns the RAG knowledge-base guidelines for a turn, or "" if
// the knowledge service is unset, the flow has no org, or the search fails. The
// caller controls the deadline via the passed context (the streaming worker
// uses a short timeout so a slow embedding provider skips guidelines for the
// turn instead of stalling first token).
func (s *ChatService) ragGuidelines(ctx context.Context, scope string, doc *models.FlowDocument, userMessage string) string {
	if s.knowledge == nil || doc == nil || doc.OrganizationID == "" {
		return ""
	}
	guidelines, err := s.knowledge.Search(ctx, scope, doc.OrganizationID, userMessage)
	if err != nil || guidelines == "" {
		return ""
	}
	return guidelines
}

// cachedContextCore returns the memoised scrub+BuildContext result, computing
// and storing it on a miss. The cache is bypassed when there is no document
// (nothing to key on) or when the cache is unset (e.g. a test-built service).
func (s *ChatService) cachedContextCore(ctx context.Context, scope string, provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) chatContextValue {
	if s.chatCtxCache == nil || doc == nil {
		return s.computeContextCore(provider, doc, report, req)
	}
	key := s.chatContextKey(scope, doc.ID, req, provider.ID(), req.Model, report)
	if v, ok := s.chatCtxCache.Get(ctx, key); ok {
		return v.(chatContextValue)
	}
	cv := s.computeContextCore(provider, doc, report, req)
	s.chatCtxCache.Set(ctx, key, cv, 0)
	return cv
}

// computeContextCore is the uncached scrub+BuildContext path — the expensive
// per-block work that is identical across consecutive turns in a thread.
func (s *ChatService) computeContextCore(provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) chatContextValue {
	scrubbedDoc, err := scrubber.ScrubDocument(doc)
	if err != nil {
		// Fall back to the unscrubbed doc rather than failing the request; the
		// per-string ScrubText calls below still redact obvious secrets in output.
		logger.Error("Failed to scrub document", map[string]interface{}{"error": err})
		scrubbedDoc = doc
	}

	ctxReq := ai.ContextRequest{
		Flow:               scrubbedDoc,
		TokenBudget:        4000,
		Provider:           provider,
		SystemPromptSuffix: req.SystemPrompt,
	}
	if report != nil {
		ctxReq.Findings = report.Findings
	}
	// Context injection (selected block + source files) is gated on
	// !ExcludeContext. Note: ContextBlockID is ALSO the conversation-history
	// key (see reconstructHistory), so it is always present on the request even
	// for a free-form turn — only the context INJECTION is skipped here.
	if !req.ExcludeContext && req.ContextBlockID != "" && scrubbedDoc != nil && scrubbedDoc.BlocksByID != nil {
		ctxReq.SelectedBlock = scrubbedDoc.BlocksByID[req.ContextBlockID]
		ctxReq.SelectedSubflow = scrubbedDoc.BlockSubflow[req.ContextBlockID]
	}
	// C-10: honour the client's selected source files. Previously the field was
	// collected and sent but never wired in, so the picker did nothing. Read the
	// file contents here (desktop/local mode only — cloud flows have no on-disk
	// sources); the result is cached at the chatContextCore layer keyed on the
	// selection fingerprint, so this disk I/O happens once per selection per
	// flow generation, not per turn. ExcludeContext skips the sources along with
	// the rest of context.
	//
	// doc.FilePath != "" confines reads to the opened flow's directory and rules
	// out cloud mode (where FilePath is empty and dir would resolve to ".",
	// letting a hand-crafted request read arbitrary server files).
	if !req.ExcludeContext && len(req.SelectedSourceFiles) > 0 && s.flowCache != nil && doc != nil && doc.FilePath != "" {
		if sources, sErr := s.flowCache.ReadSourceFiles(doc, req.SelectedSourceFiles); sErr == nil && len(sources) > 0 {
			ctxReq.RawSourceFiles = sources
		}
	}

	sysPrompt, contextText := ai.BuildContext(ctxReq)
	return chatContextValue{scrubbedDoc: scrubbedDoc, sysPrompt: sysPrompt, contextText: contextText}
}

// buildMessages assembles the provider message list: prior history (scrubbed)
// followed by a single user turn. Flow context is merged INTO that user turn
// rather than appended as a separate user message — two consecutive user-role
// messages are rejected by many providers (400). ExcludeContext or empty
// context skips the merge.
func buildMessages(req models.ChatRequest, contextText string) []ai.Message {
	messages := make([]ai.Message, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = ai.Message{Role: m.Role, Content: scrubber.ScrubText(m.Content)}
	}
	userContent := scrubber.ScrubText(req.UserMessage)
	if !req.ExcludeContext && contextText != "" {
		userContent = "Context:\n" + contextText + "\n\n" + userContent
	}
	return append(messages, ai.Message{Role: "user", Content: userContent})
}

// enforceBudget returns an error when the day's AI spend has reached the
// configured DailyBudget. A budget of 0 (or absent settings) means unlimited.
// It fails closed: if the current spend cannot be read from the store the
// request is denied, so a DB hiccup can't open an unlimited-spend window.
func (s *ChatService) enforceBudget(ctx context.Context, scope, orgID string) error {
	settings := s.settings.Get()
	if settings == nil || settings.AI.DailyBudget <= 0 {
		return nil
	}
	usage, err := s.dailyUsage(ctx, scope, orgID)
	if err != nil {
		return fmt.Errorf("daily AI budget check unavailable: %w", err)
	}
	if usage >= settings.AI.DailyBudget {
		return fmt.Errorf("daily AI budget exceeded ($%.2f / $%.2f)", usage, settings.AI.DailyBudget)
	}
	return nil
}

func (s *ChatService) StreamChatMessage(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (streamID string, err error) {
	defer logger.Guard("App.StreamChatMessage", &err)

	// Per-caller concurrency cap: reject synchronously so the POST surfaces
	// the error instead of a stream being created and immediately failed.
	active := 0
	s.activeStreams.Range(func(_, v interface{}) bool {
		if v.(*streamCtl).ownerID == scope {
			active++
		}
		return active < maxConcurrentStreamsPerScope
	})
	if active >= maxConcurrentStreamsPerScope {
		return "", fmt.Errorf("too many chat responses running at once (max %d) — wait for one to finish or stop it", maxConcurrentStreamsPerScope)
	}

	// Stream ID: prefer a client-generated UUID (C-1) so the client can subscribe
	// its SSE listener BEFORE creating the stream, letting the backend emit
	// immediately with no /chat/begin round-trip. When absent, fall back to the
	// legacy backend-generated ID + explicit begin handshake.
	clientProvided := false
	if req.ClientStreamID != "" {
		if _, parseErr := uuid.Parse(req.ClientStreamID); parseErr != nil {
			return "", fmt.Errorf("clientStreamId must be a UUID: %w", parseErr)
		}
		if _, dup := s.activeStreams.Load(req.ClientStreamID); dup {
			return "", fmt.Errorf("clientStreamId already in use")
		}
		if _, dup := s.finishedStreams.Load(req.ClientStreamID); dup {
			return "", fmt.Errorf("clientStreamId already in use")
		}
		streamID = req.ClientStreamID
		clientProvided = true
	} else {
		streamID = uuid.NewString()
	}
	// The stream deliberately outlives the HTTP request that created it (begin/
	// cancel/resume are separate requests; chunks are delivered over SSE), so it
	// must NOT inherit r.Context() — net/http cancels that the instant the create
	// handler returns, which would abort the provider call (notably Copilot's
	// session-token exchange on a cold cache) with "context canceled". Cancellation
	// is handled explicitly via ctl.cancel() (CancelStream) and the timeout below.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxChatStreamDuration)
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{}), ownerID: scope}
	ctl.touch() // pre-first-chunk work (context build, provider dial) counts as activity
	s.activeStreams.Store(streamID, ctl)

	// C-1: when the client supplied the stream ID it has already registered its
	// SSE listener, so unblock emission immediately (idempotent via startOnce —
	// a late /chat/begin or the resume path can still close it harmlessly).
	// The legacy path leaves ctl.started open for BeginStream to close.
	if clientProvided {
		ctl.startOnce.Do(func() { close(ctl.started) })
	}

	// Multi-replica: mirror the stream to the shared backplane so a client that
	// reconnects to a different replica can resume it (and OwnerOf can authorize
	// there). The initial save publishes the owner immediately; mirrorStream then
	// snapshots the growing buffer until the stream ends. No-op single-replica.
	if rs := s.resumeBackplane(); rs.enabled() {
		rs.Save(ctx, streamID, ctl.snapshot(), maxChatStreamDuration+resumeRetention)
		go s.mirrorStream(ctx, streamID, ctl)
	}

	go s.watchStream(ctx, streamID, scope, ctl)

	emit := func(eventType string, data map[string]interface{}) {
		s.notifier.EmitTo(scope, "chat:event",
			map[string]interface{}{"streamId": streamID, "type": eventType, "data": data})
	}
	// C-6: coalesce consecutive chunk deltas into one SSE event to cut framing
	// overhead. wrap() flushes before any non-chunk event, so done/error/tool
	// ordering is preserved; the first chunk bypasses the batch.
	coalesce := newChunkCoalescer(emit)
	emit = coalesce.wrap()

	started := false
	awaitStart := func() bool {
		if started {
			return true
		}
		select {
		case <-ctl.started:
			started = true
			return true
		case <-ctx.Done():
			return false
		}
	}

	go func() {
		metrics.ChatStreamStart()
		defer metrics.ChatStreamEnd()
		defer func() {
			if r := recover(); r != nil {
				logger.Error("chat stream goroutine panicked", "streamID", streamID, "panic", r)
			}
		}()
		// On exit, remove the stream from the active set (so cancel/begin and
		// leak-detection see it as gone) and move it to the finished set for a
		// short grace period, so a client that reconnects after completion can
		// still fetch the final buffer via ResumeStream instead of hanging.
		defer func() {
			coalesce.flush() // deliver any batched-but-unsent tail before exit
			cancel()
			s.activeStreams.Delete(streamID)
			s.finishedStreams.Store(streamID, ctl)
			time.AfterFunc(resumeRetention, func() { s.finishedStreams.Delete(streamID) })
		}()

		// fail stores the error in the stream buffer AND emits the SSE event so
		// both live SSE clients and reconnecting clients (via resumeStream) see it.
		// It deliberately does NOT wait for BeginStream: pre-stream failures must
		// surface immediately instead of parking the goroutine (and the client)
		// until begin or the 5-minute cap. The errMsg must be stored before the
		// emit — BeginStream returns the stored state when the emit predated the
		// client's SSE subscription.
		fail := func(msg string) {
			ctl.mu.Lock()
			ctl.errMsg = msg
			ctl.mu.Unlock()
			emit("error", map[string]interface{}{"message": msg})
		}

		// C-3: launch the per-turn RAG search BEFORE provider resolution so the
		// synchronous embedding API call + vector query overlap with factory.For
		// and budget enforcement. ragDone is buffered (1) so the goroutine never
		// leaks if the worker moves on; ragCtx bounds the embedding call itself
		// so a slow provider aborts instead of billing a huge token request.
		var ragDone chan string
		if s.knowledge != nil && doc != nil && doc.OrganizationID != "" {
			ragDone = make(chan string, 1)
			ragCtx, ragCancel := context.WithTimeout(ctx, ragGuidelinesDeadline)
			go func() {
				defer ragCancel()
				ragDone <- s.ragGuidelines(ragCtx, scope, doc, req.UserMessage)
			}()
		}

		provider, err := s.factory.For(scope, req.Provider)
		if err != nil {
			fail(err.Error())
			return
		}

		// Org-scoped flows attribute usage and enforce the daily budget at the
		// org level; personal flows fall back to the per-user total.
		orgID := ""
		if doc != nil {
			orgID = doc.OrganizationID
		}

		if err := s.enforceBudget(ctx, scope, orgID); err != nil {
			fail(err.Error())
			return
		}

		if req.Provider == "demo" && s.demoLimiter != nil {
			if _, err := s.demoLimiter.ReserveForDisplay(); err != nil {
				fail(err.Error())
				return
			}
		}

		// Server-side history reconstruction: when the client omits Messages
		// (the efficient path — it sends only the new userMessage), load the
		// prior conversation from the store so the LLM gets full context
		// without the client re-sending the whole history each turn. A
		// non-empty Messages slice (legacy client, or a resend that locally
		// truncated history) is used as-is.
		reconstructed := false
		if len(req.Messages) == 0 {
			req.Messages = s.reconstructHistory(ctx, doc, req)
			reconstructed = true
		}

		// BUG-5: persist the user turn at stream start so closing the app
		// mid-stream (or a crash before onDone) doesn't lose the typed message.
		// Only on the reconstruction path — a client that sent Messages (legacy
		// / resend override) owns persistence itself. Synchronous and before the
		// provider dial: one store write (comparable to enforceBudget's DB read),
		// and it must complete before the client's save-on-done to avoid the
		// done-write racing a still-in-flight start-write. The client's
		// save-on-done later overwrites this with the complete
		// [history+user+assistant]; on a mid-stream close this write is what
		// retains the user message. Errors are non-fatal (a store hiccup just
		// means the turn isn't pre-persisted; the turn itself still runs).
		if reconstructed && doc != nil && req.UserMessage != "" {
			convKey := req.ContextBlockID
			if convKey == "" {
				convKey = "flow"
			}
			userMsg := models.ChatMessage{
				ID:             uuid.NewString(),
				Role:           "user",
				Content:        req.UserMessage,
				Timestamp:      time.Now(),
				ContextBlockID: req.ContextBlockID,
			}
			persisted := make([]models.ChatMessage, 0, len(req.Messages)+1)
			persisted = append(persisted, req.Messages...)
			persisted = append(persisted, userMsg)
			if err := s.SaveConversation(ctx, doc, convKey, persisted); err != nil {
				logger.Warn("chat: failed to persist user turn at stream start", "error", err)
			}
		}

		core := s.cachedContextCore(ctx, scope, provider, doc, report, req)

		// Collect RAG (launched above) up to its deadline; on timeout the
		// goroutine aborts via ragCtx and sends "" → guidelines are simply
		// skipped for this turn (skip-on-timeout). ctx.Done covers a stream
		// cancelled while waiting.
		sysPrompt := core.sysPrompt
		if ragDone != nil {
			select {
			case guidelines := <-ragDone:
				if guidelines != "" {
					sysPrompt += "\n\n" + guidelines
				}
			case <-ctx.Done():
				return
			}
		}
		scrubbedDoc := core.scrubbedDoc
		sysPrompt = scrubber.ScrubText(sysPrompt)
		contextText := scrubber.ScrubText(core.contextText)

		temperature, maxTokens := normalizeChatParams(req.Temperature, req.MaxTokens, provider.ContextLimit(), ai.ModelMaxOutputTokens(ctx, provider, req.Model))
		aiReq := ai.Request{
			SystemPrompt: sysPrompt,
			Messages:     buildMessages(req, contextText),
			Model:        req.Model,
			Temperature:  temperature,
			MaxTokens:    maxTokens,
			OrgID:        orgID,
		}

		// When the caller opted into tools and the provider supports them, run the
		// read-only agentic tool loop (streamed turns + tool status events). It is
		// fully self-contained — emits chunk/tool/done/error and updates ctl — so the
		// normal streaming path below is skipped entirely (zero regression when off).
		if req.UseTools && provider.SupportsTools() {
			s.runToolLoop(ctx, provider, aiReq, scrubbedDoc, ctl, awaitStart, emit, coalesce.flushAndCount)
			return
		}

		// out masks secrets in the model's output (even across chunk splits),
		// maintains the resume buffer, and counts emitted chunk events; the done
		// event carries the count so the client can detect chunks dropped by a
		// saturated SSE buffer and recover the authoritative text via resume.
		out := newScrubbedEmitter(ctl, emit)
		err = provider.Stream(ctx, aiReq, func(chunk ai.Chunk) {
			ctl.touch()
			if !awaitStart() {
				return
			}
			if chunk.Error != nil {
				out.flush()
				ctl.mu.Lock()
				ctl.errMsg = chunk.Error.Error()
				ctl.mu.Unlock()
				emit("error", map[string]interface{}{"message": chunk.Error.Error()})
				return
			}
			if chunk.Done {
				out.flush()
				ctl.mu.Lock()
				ctl.done = true
				ctl.tokensIn = chunk.TokensIn
				ctl.tokensOut = chunk.TokensOut
				ctl.mu.Unlock()
				emit("done", map[string]interface{}{
					"tokensIn":  chunk.TokensIn,
					"tokensOut": chunk.TokensOut,
					"chunks":    coalesce.flushAndCount(),
				})
				return
			}

			out.text(chunk.Text)
		})

		if err != nil {
			msg := ctl.failureMessage(ctx, err)
			ctl.mu.Lock()
			ctl.errMsg = msg
			ctl.mu.Unlock()
			if !awaitStart() {
				return
			}
			// Deliver whatever the scrubber was still holding before reporting
			// the failure, so the partial response isn't truncated client-side.
			out.flush()
			emit("error", map[string]interface{}{"message": msg})
		}
	}()

	return streamID, nil
}

// maxToolIterations bounds the read-only agentic loop so a misbehaving model
// that keeps requesting tools can't run forever.
const maxToolIterations = 6

// runToolLoop drives the read-only tool/agent loop: it offers the registered
// tools to the model, executes any tool calls against the (already scrubbed)
// doc + analysis cache, feeds results back, and repeats until the model returns
// a final text answer (or the iteration cap is hit). It is self-contained —
// emits "tool"/"chunk"/"done"/"error" events and updates ctl — so the caller
// does not run its own streaming/error handling for this path.
//
// Token consumption of ctl.started is gated through a single cached
// ensureStarted so it is consumed at most once (the channel only holds one
// token), regardless of how many turns/events the loop produces.
func (s *ChatService) runToolLoop(
	ctx context.Context,
	provider ai.Provider,
	base ai.Request,
	doc *models.FlowDocument,
	ctl *streamCtl,
	awaitStart func() bool,
	emit func(string, map[string]interface{}),
	doneChunks func() int,
) {
	base.Tools = ai.ToolDefinitions()
	msgs := base.Messages
	tctx := ai.ToolContext{Ctx: ctx, Doc: doc, Analysis: s.analysisCache}

	started := false
	ensureStarted := func() bool {
		if !started && awaitStart() {
			started = true
		}
		return started
	}
	fail := func(msg string) {
		ctl.mu.Lock()
		ctl.errMsg = msg
		ctl.mu.Unlock()
		if ensureStarted() {
			emit("error", map[string]interface{}{"message": msg})
		}
	}

	var totalIn, totalOut int
	// out masks secrets in streamed turn text and counts emitted chunk events
	// across all turns; the count rides the done event so the client can detect
	// dropped SSE chunks (see StreamChatMessage). One scrubber spans the loop —
	// each turn's text is flushed before its tool events, so held text can't
	// bleed between turns.
	out := newScrubbedEmitter(ctl, emit)
	for i := 0; i < maxToolIterations; i++ {
		turn := base
		turn.Messages = msgs

		// Stream the turn: forward assistant text to the client as it arrives, and
		// capture the tool calls / token usage from the terminal (Done) chunk.
		var (
			turnText  strings.Builder
			turnCalls []ai.ToolCall
			turnIn    int
			turnOut   int
			chunkErr  error
		)
		streamErr := provider.Stream(ctx, turn, func(c ai.Chunk) {
			ctl.touch()
			switch {
			case c.Error != nil:
				chunkErr = c.Error
			case c.Done:
				turnIn = c.TokensIn
				turnOut = c.TokensOut
				turnCalls = c.ToolCalls
			case c.Text != "":
				if !ensureStarted() {
					return
				}
				// turnText keeps the raw text: it only feeds the next model turn
				// (server-side); everything client-bound goes through the scrubber.
				turnText.WriteString(c.Text)
				out.text(c.Text)
			}
		})
		if started {
			// Release scrubber-held text before any error/tool/done event so the
			// turn's tail isn't withheld or attributed to the next turn.
			out.flush()
		}
		if streamErr != nil {
			fail(ctl.failureMessage(ctx, streamErr))
			return
		}
		if chunkErr != nil {
			fail(chunkErr.Error())
			return
		}
		totalIn += turnIn
		totalOut += turnOut

		// No tool calls → this turn is the final answer (its text already
		// streamed above, so there's nothing more to emit but the done event).
		if len(turnCalls) == 0 {
			if !ensureStarted() {
				return
			}
			ctl.mu.Lock()
			ctl.done = true
			ctl.tokensIn = totalIn
			ctl.tokensOut = totalOut
			ctl.mu.Unlock()
			emit("done", map[string]interface{}{"tokensIn": totalIn, "tokensOut": totalOut, "chunks": doneChunks()})
			return
		}

		// Record the assistant turn that issued the calls (including any streamed
		// preamble text), then run each tool and append its result so the next
		// turn can see them.
		msgs = append(msgs, ai.Message{Role: "assistant", Content: turnText.String(), ToolCalls: turnCalls})
		for _, tc := range turnCalls {
			if !ensureStarted() {
				return
			}
			emit("tool", map[string]interface{}{"name": tc.Name, "label": ai.ToolLabel(tc.Name)})
			result := ai.ExecuteTool(tc.Name, tc.Input, tctx)
			msgs = append(msgs, ai.Message{Role: "tool", Content: result, ToolCallID: tc.ID})
		}
	}

	fail(fmt.Sprintf("tool loop exceeded %d iterations without a final answer", maxToolIterations))
}

// resumeMirrorInterval is how often the growing stream buffer is snapshotted to
// the resume backplane while streaming. It bounds Redis writes to ~1/sec/stream
// (the live delta stream is unaffected — this only backs cross-replica resume).
const resumeMirrorInterval = time.Second

// mirrorStream periodically snapshots ctl to the resume backplane until the
// stream's context ends (completion, cancel, or timeout), then writes a final
// snapshot with the short resumeRetention TTL — matching the local
// finishedStreams grace window so both expire together. Only run when the
// backplane is enabled.
func (s *ChatService) mirrorStream(ctx context.Context, streamID string, ctl *streamCtl) {
	t := time.NewTicker(resumeMirrorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Detached context: the stream ctx is already done, but the final
			// buffer must still be published for reconnecting clients.
			s.resumeBackplane().Save(context.WithoutCancel(ctx), streamID, ctl.snapshot(), resumeRetention)
			return
		case <-t.C:
			s.resumeBackplane().Save(ctx, streamID, ctl.snapshot(), maxChatStreamDuration+resumeRetention)
		}
	}
}

// sliceFrom returns s[from:] clamped to the string bounds. from beyond the end
// returns "" (a delta resume for an offset the client already has).
func sliceFrom(s string, from int) string {
	if from <= 0 {
		return s
	}
	if from >= len(s) {
		return ""
	}
	return s[from:]
}

// BeginStream unblocks the stream goroutine so it may start emitting. When the
// stream has already finished — typically a pre-stream failure (bad provider,
// budget exceeded) whose error event was emitted before the client's SSE
// subscription existed — the final buffered state is returned instead so the
// caller can deliver it directly. nil means the stream is live (or unknown)
// and events arrive over SSE.
func (s *ChatService) BeginStream(ctx context.Context, streamID string) *ResumeResult {
	if val, ok := s.activeStreams.Load(streamID); ok {
		ctl := val.(*streamCtl)
		ctl.startOnce.Do(func() { close(ctl.started) })
		// A failed-fast stream may still be in the active set for an instant;
		// its error was emitted before this call (pre-subscription, so lost) —
		// hand it back synchronously. A snapshot without done/error means any
		// later terminal event is emitted after this request arrived, i.e.
		// after the client subscribed, so SSE delivery covers it.
		if snap := ctl.snapshot(); snap.Done || snap.Error != "" {
			return &ResumeResult{Text: snap.Text, Done: snap.Done, Error: snap.Error, TokensIn: snap.TokensIn, TokensOut: snap.TokensOut}
		}
		return nil
	}
	if res, err := s.ResumeStream(ctx, streamID, 0); err == nil && (res.Done || res.Error != "") {
		return res
	}
	return nil
}

func (s *ChatService) CancelStream(streamID string) {
	if val, ok := s.activeStreams.Load(streamID); ok {
		ctl := val.(*streamCtl)
		ctl.cancelWithReason("response stopped")
	}
}

func (s *ChatService) CancelAll() {
	s.activeStreams.Range(func(key, value interface{}) bool {
		ctl := value.(*streamCtl)
		ctl.cancelWithReason("response stopped: the server is shutting down")
		return true
	})
}

// OwnerOf returns the scope/caller that created the given stream, or "" if the
// stream is unknown. It checks active and recently-finished streams locally,
// then falls back to the resume backplane so an authz check on a replica that
// did not create the stream still resolves the owner (multi-replica).
func (s *ChatService) OwnerOf(ctx context.Context, streamID string) string {
	if val, ok := s.activeStreams.Load(streamID); ok {
		return val.(*streamCtl).ownerID
	}
	if val, ok := s.finishedStreams.Load(streamID); ok {
		return val.(*streamCtl).ownerID
	}
	if snap, ok := s.resumeBackplane().Load(ctx, streamID); ok {
		return snap.Owner
	}
	return ""
}

// ResumeStream returns a stream's buffered state. It prefers the live local
// stream; when the stream ran on a different replica it falls back to the shared
// backplane so the client can still resume after reconnecting elsewhere.
//
// The `from` offset (bytes) enables delta resume for reconnects: a client whose
// accumulated text is a clean prefix of the buffer sends its length and receives
// only the tail, avoiding a full-buffer re-fetch + re-parse over flaky links.
// from is clamped to [0, len(buffer)]; a chunk-count-mismatch (possible mid-
// stream gaps) should pass from=0 for a full authoritative replace.
func (s *ChatService) ResumeStream(ctx context.Context, streamID string, from int) (*ResumeResult, error) {
	if from < 0 {
		from = 0
	}
	val, ok := s.activeStreams.Load(streamID)
	if !ok {
		val, ok = s.finishedStreams.Load(streamID)
	}
	if ok {
		ctl := val.(*streamCtl)
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		return &ResumeResult{
			Text:      sliceFrom(ctl.buffer.String(), from),
			Done:      ctl.done,
			Error:     ctl.errMsg,
			TokensIn:  ctl.tokensIn,
			TokensOut: ctl.tokensOut,
		}, nil
	}
	if snap, ok := s.resumeBackplane().Load(ctx, streamID); ok {
		return &ResumeResult{
			Text:      sliceFrom(snap.Text, from),
			Done:      snap.Done,
			Error:     snap.Error,
			TokensIn:  snap.TokensIn,
			TokensOut: snap.TokensOut,
		}, nil
	}
	return nil, fmt.Errorf("stream not found or already completed")
}

// useBackendConversations reports whether conversations should persist through
// the storage backend (cloud mode: Postgres, durable + cross-replica + RLS)
// rather than the local on-disk file store (desktop, single persistent instance).
// Explicit allow-list on ModeCloud: an unset/zero mode (or any future mode) must
// fall back to the local file store, never silently route chat history to Postgres.
// reconstructHistory loads the stored conversation for the request's context
// key so a client that omitted Messages (the efficient path — it sends only the
// new userMessage) still gets full LLM context. The key matches the client's
// save key: contextBlockId, or "flow" when none. A store error degrades to
// empty history rather than failing the turn. Returns nil when doc is nil.
func (s *ChatService) reconstructHistory(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest) []models.ChatMessage {
	if doc == nil {
		return nil
	}
	convKey := req.ContextBlockID
	if convKey == "" {
		convKey = "flow"
	}
	loaded, err := s.GetConversation(ctx, doc, convKey)
	if err != nil {
		logger.Warn("chat history reconstruction failed; continuing with empty history", "error", err)
		return nil
	}
	return loaded
}

func (lsb *ChatService) useBackendConversations() bool {
	return lsb.mode == config.ModeCloud && lsb.backend != nil
}

func (lsb *ChatService) GetConversation(ctx context.Context, doc *models.FlowDocument, provider string) ([]models.ChatMessage, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if lsb.useBackendConversations() {
		stored, err := lsb.backend.LoadConversation(ctx, doc.ID, provider)
		if err != nil {
			return nil, err
		}
		return toModelMessages(stored), nil
	}
	path, err := convFilePath(lsb.configDir, provider, doc.ID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path derived from configDir+provider+flowID, not raw user input
	if err != nil {
		if os.IsNotExist(err) {
			return []models.ChatMessage{}, nil
		}
		return nil, fmt.Errorf("failed to read conversation file: %w", err)
	}

	var conv models.ConversationFile
	if err := json.Unmarshal(data, &conv); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation: %w", err)
	}
	return conv.Messages, nil
}

func (lsb *ChatService) SaveConversation(ctx context.Context, doc *models.FlowDocument, provider string, messages []models.ChatMessage) error {
	if doc == nil {
		return fmt.Errorf("no flow loaded")
	}
	trimmed := evictConvMessages(messages)

	if lsb.useBackendConversations() {
		return lsb.backend.SaveConversation(ctx, doc.ID, provider, toStorageMessages(trimmed))
	}

	path, err := convFilePath(lsb.configDir, provider, doc.ID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("failed to create conversations directory: %w", err)
	}

	conv := models.ConversationFile{
		Version:   1,
		FlowKey:   doc.ID,
		Scope:     provider,
		UpdatedAt: time.Now(),
		Messages:  trimmed,
	}

	data, err := json.MarshalIndent(conv, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}

	// Atomic write at 0600: an interrupted write leaves the prior conversation
	// intact rather than truncating it, and the file isn't world-readable.
	return atomicWriteConv(dir, path, data)
}

func (lsb *ChatService) ClearConversation(ctx context.Context, doc *models.FlowDocument, provider string) error {
	if doc == nil {
		return fmt.Errorf("no flow loaded")
	}
	if lsb.useBackendConversations() {
		return lsb.backend.DeleteConversation(ctx, doc.ID, provider)
	}
	path, err := convFilePath(lsb.configDir, provider, doc.ID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete conversation file: %w", err)
	}
	return nil
}

func (lsb *ChatService) ExportConversation(ctx context.Context, doc *models.FlowDocument, provider string, path string) error {
	if err := validateUserPath(path); err != nil {
		return err
	}
	msgs, err := lsb.GetConversation(ctx, doc, provider)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "### %s (%s)\n\n%s\n\n", m.Role, m.Timestamp.Format(time.RFC3339), m.Content)
	}
	return os.WriteFile(path, []byte(b.String()), 0644) // #nosec G306 -- user-chosen export path; world-readable is intended for sharing
}

func (s *ChatService) GetDemoRemaining() (int, error) {
	if s.demoLimiter == nil {
		return 0, nil
	}
	rem, _ := s.demoLimiter.Remaining()
	return rem, nil
}

func (s *ChatService) PreviewContext(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (res *models.ContextPreview, err error) {
	defer logger.Guard("App.PreviewContext", &err)

	provider, err := s.factory.For(scope, req.Provider)
	if err != nil {
		return nil, err
	}

	_, sysPrompt, contextText := s.buildScrubbedContext(ctx, scope, provider, doc, report, req)
	scrubbedUserMsg := scrubber.ScrubText(req.UserMessage)

	total := provider.EstimateTokens(sysPrompt + contextText + scrubbedUserMsg)

	return &models.ContextPreview{
		SystemPrompt:    sysPrompt,
		ContextText:     contextText,
		UserMessage:     scrubbedUserMsg,
		EstimatedTokens: total,
		ContextLimit:    provider.ContextLimit(),
	}, nil
}

func (s *ChatService) GetSuggestedPrompts(hasBlock, hasFindings bool) (prompts []string, err error) {
	defer logger.Guard("App.GetSuggestedPrompts", &err)

	settings := s.settings.Get()
	if settings == nil {
		return nil, fmt.Errorf("settings not found")
	}

	p := settings.AI.Prompts

	if hasBlock && hasFindings {
		prompts = append(prompts, p.BlockWithFindings...)
	} else if hasFindings {
		prompts = append(prompts, p.Finding...)
	} else if hasBlock {
		prompts = append(prompts, p.Block...)
	} else {
		prompts = append(prompts, p.Flow...)
	}

	return prompts, nil
}
