package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// maxConcurrentStreamsPerScope caps how many chat streams one caller may run
// in parallel (the UI streams one per thread). It bounds a single user's
// provider spend and goroutine footprint regardless of client behaviour.
const maxConcurrentStreamsPerScope = 3

// maxToolIterations bounds the read-only agentic loop so a misbehaving model
// that keeps requesting tools can't run forever.
const maxToolIterations = 6

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

// errStreamIDInUse is returned by tryStartStream when a client-provided stream
// ID collides with an active or recently-finished stream. It is a sentinel so
// the caller can distinguish a collision (reject the request as "already in
// use") from the per-caller cap being exceeded.
var errStreamIDInUse = errors.New("clientStreamId already in use")

// convMutexFor returns the per-conversation mutex used to serialize the
// reconstruct-history + persist-user-turn critical section. LoadOrStore makes
// the lookup-and-create atomic so two concurrent callers get the same mutex.
func (s *ChatService) convMutexFor(flowID, convKey string) *sync.Mutex {
	v, _ := s.convMu.LoadOrStore(flowID+"\x00"+convKey, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// tryStartStream atomically checks the per-caller concurrency cap AND reserves
// the stream ID. The ID uniqueness check and the store happen under the same
// mutex (via LoadOrStore) so two concurrent requests sharing a client-provided
// UUID cannot both succeed — one overwriting the other's control, which would
// orphan the loser's cancel/resume and interleave two LLM streams on the same
// streamId. Returns:
//   - (true, nil) on success
//   - (false, nil) when the per-caller cap is exceeded
//   - (false, errStreamIDInUse) when the stream ID is already active/finished
func (s *ChatService) tryStartStream(streamID, scope string, ctl *streamCtl) (bool, error) {
	s.streamCapMu.Lock()
	defer s.streamCapMu.Unlock()
	if _, dup := s.finishedStreams.Load(streamID); dup {
		return false, errStreamIDInUse
	}
	// LoadOrStore is the atomic reservation: if a concurrent request already
	// stored this ID, the second caller gets back the existing control and is
	// rejected rather than overwriting it.
	if _, loaded := s.activeStreams.LoadOrStore(streamID, ctl); loaded {
		return false, errStreamIDInUse
	}
	active := 0
	s.activeStreams.Range(func(_, v interface{}) bool {
		if v.(*streamCtl).ownerID == scope {
			active++
		}
		return active < maxConcurrentStreamsPerScope
	})
	// The just-stored entry counts toward the cap; if storing it pushed the
	// caller over, undo the reservation and reject as cap-exceeded (not
	// collision) so the caller reports the right reason.
	if active > maxConcurrentStreamsPerScope {
		s.activeStreams.Delete(streamID)
		return false, nil
	}
	return true, nil
}

// chatStore is the narrow slice of StorageBackend that ChatService actually
// uses: conversation persistence and daily-usage lookups for budget
// enforcement. Depending on this instead of the full StorageBackend means a
// change to an unrelated storage domain (users, orgs, audit, …) can't affect
// (or require updating) chat wiring.
type chatStore interface {
	storageif.ConversationStore
	storageif.UsageStore
}

// ChatService owns chat stream state and operations.
type ChatService struct {
	notifier      EventNotifier
	configDir     string
	flowCache     *FlowService
	analysisCache *AnalysisService
	settings      SettingsProvider
	factory       *ai.ProviderFactory
	demoLimiter   *ai.DemoLimiter
	backend       chatStore
	// mode selects where conversations persist: ModeLocal uses the on-disk file
	// store (desktop), cloud routes through the backend (Postgres + RLS).
	mode      config.DeploymentMode
	knowledge *rag.KnowledgeService
	// watchdogInterval / idleTimeout override subscriberCheckInterval and
	// streamIdleTimeout in tests; 0 ⇒ defaults.
	watchdogInterval time.Duration
	idleTimeout      time.Duration
	activeStreams    sync.Map   // map[streamID]*streamCtl — in-flight streams
	streamCapMu      sync.Mutex // serializes the per-caller concurrency check + store
	// finishedStreams holds recently-completed streams for a short grace period
	// (resumeRetention) so a client that reconnects after a stream ended can
	// still fetch its final buffer via ResumeStream.
	finishedStreams sync.Map // map[streamID]*streamCtl
	// convMu serializes the reconstruct-history + persist-user-turn critical
	// section per (flowID, convKey). Without it, two concurrent streams on the
	// same conversation both reconstruct the same history and then clobber each
	// other's SaveConversation (a full-file replace), silently losing one user
	// turn. Keyed by flowID+"\x00"+convKey; each entry is a *sync.Mutex.
	convMu sync.Map
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
	// chatCtxGen is a bounded LRU (not a plain map) so a long-lived process that
	// edits many distinct flows over its uptime — including deleted flows,
	// which also invalidate through this path — can't grow it without limit.
	// It self-initializes on first use (see chatCtxGenCache) so a ChatService
	// built as a bare struct literal (common in tests) still gets a working
	// cache instead of a nil interface, mirroring sync.Map's old zero-value-
	// ready behavior without reintroducing unbounded growth.
	chatCtxCache   cache.Cache
	chatCtxGen     cache.Cache
	chatCtxGenOnce sync.Once
}

func NewChatService(
	notifier EventNotifier,
	configDir string,
	flowCache *FlowService,
	analysisCache *AnalysisService,
	settings SettingsProvider,
	factory *ai.ProviderFactory,
	demoLimiter *ai.DemoLimiter,
	backend chatStore,
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
		chatCtxGen:    newChatCtxGenCache(),
	}
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

// turnSetup is the resolved provider + attribution + concurrent RAG lookup for
// one turn — the output of prepareTurn. ragDone is nil when RAG isn't wired for
// this doc/knowledge combination.
type turnSetup struct {
	provider ai.Provider
	orgID    string
	ragDone  <-chan string
}

// prepareTurn resolves the provider, computes org attribution, enforces the
// daily AI budget, reserves a demo-mode slot when applicable, and — if a
// knowledge base is configured for the flow's org — kicks off the per-turn RAG
// guidelines lookup concurrently so its embedding API call overlaps with these
// checks instead of gating first token (C-3). The returned ragDone channel (if
// non-nil) delivers exactly one value; the caller must receive from it (or
// abandon it on ctx cancellation) before finishing the turn.
func (s *ChatService) prepareTurn(ctx context.Context, scope string, doc *models.FlowDocument, req models.ChatRequest) (turnSetup, error) {
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
		return turnSetup{}, err
	}

	// Org-scoped flows attribute usage and enforce the daily budget at the
	// org level; personal flows fall back to the per-user total.
	orgID := ""
	if doc != nil {
		orgID = doc.OrganizationID
	}

	if err := s.enforceBudget(ctx, scope, orgID); err != nil {
		return turnSetup{}, err
	}

	if req.Provider == "demo" && s.demoLimiter != nil {
		if _, err := s.demoLimiter.ReserveForDisplay(); err != nil {
			return turnSetup{}, err
		}
	}

	return turnSetup{provider: provider, orgID: orgID, ragDone: ragDone}, nil
}

// persistInitialUserTurn stores the user's turn at stream start (BUG-5) so
// closing the app mid-stream (or a crash before onDone) doesn't lose the typed
// message. Only called on the history-reconstruction path — a client that sent
// Messages explicitly (legacy client, or a resend that locally truncated
// history) owns persistence itself. Synchronous and before the provider dial;
// errors are non-fatal (a store hiccup just means the turn isn't pre-persisted,
// the turn itself still runs).
func (s *ChatService) persistInitialUserTurn(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest) {
	if doc == nil || req.UserMessage == "" {
		return
	}
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

// buildStreamRequest assembles the ai.Request for a turn: it resolves the
// (possibly cached) scrubbed context, waits for the concurrent RAG lookup
// (started by prepareTurn) up to its own deadline, merges history + context
// into the message list, clamps generation params to the model's limits, and
// enforces the context window (truncating oldest history if needed). Returns
// the scrubbed document alongside the request so the caller can hand it to the
// tool loop without recomputing it.
//
// ctx.Done() during the RAG wait aborts the turn (ok=false, no error to report
// — the caller's own ctx-cancellation handling already covers this case).
func (s *ChatService) buildStreamRequest(ctx context.Context, ts turnSetup, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (aiReq ai.Request, scrubbedDoc *models.FlowDocument, ok bool, err error) {
	core := s.cachedContextCore(ctx, scope, ts.provider, doc, report, req)

	// Collect RAG (launched by prepareTurn) up to its deadline; on timeout the
	// goroutine aborts via its own ragCtx and sends "" → guidelines are simply
	// skipped for this turn (skip-on-timeout). ctx.Done covers a stream
	// cancelled while waiting.
	sysPrompt := core.sysPrompt
	if ts.ragDone != nil {
		select {
		case guidelines := <-ts.ragDone:
			if guidelines != "" {
				sysPrompt += "\n\n" + guidelines
			}
		case <-ctx.Done():
			return ai.Request{}, nil, false, nil
		}
	}
	scrubbedDoc = core.scrubbedDoc
	sysPrompt = scrubber.ScrubText(sysPrompt)
	contextText := scrubber.ScrubText(core.contextText)

	temperature, maxTokens := normalizeChatParams(req.Temperature, req.MaxTokens, ts.provider.ContextLimit(), ai.ModelMaxOutputTokens(ctx, ts.provider, req.Model))
	aiReq = ai.Request{
		SystemPrompt: sysPrompt,
		Messages:     buildMessages(req, contextText),
		Model:        req.Model,
		Temperature:  temperature,
		MaxTokens:    maxTokens,
		OrgID:        ts.orgID,
	}

	// Enforce the model's context window before sending: truncate oldest
	// history if the prompt is too large, or fail cleanly with
	// ErrContextLimit if even the system prompt + current turn overflow.
	// Without this, an oversized prompt reaches the provider and surfaces as
	// a raw, confusing 400 ("context length exceeded") instead of a friendly
	// "conversation too long" message.
	if err := truncateForContextWindow(ts.provider, &aiReq, ts.provider.ContextLimit()); err != nil {
		return ai.Request{}, nil, false, err
	}

	return aiReq, scrubbedDoc, true, nil
}

// resolveStreamID assigns the stream's identity: a client-generated UUID (C-1)
// so the client can subscribe its SSE listener BEFORE creating the stream,
// letting the backend emit immediately with no /chat/begin round-trip, or —
// when absent — a backend-generated ID for the legacy explicit-begin handshake.
// It validates the format only; the uniqueness/atomicity check lives in
// tryStartStream (LoadOrStore under streamCapMu) so the check-and-store cannot
// be split by a concurrent request reusing the same UUID.
func (s *ChatService) resolveStreamID(req models.ChatRequest) (streamID string, clientProvided bool, err error) {
	if req.ClientStreamID == "" {
		return uuid.NewString(), false, nil
	}
	if _, parseErr := uuid.Parse(req.ClientStreamID); parseErr != nil {
		return "", false, fmt.Errorf("clientStreamId must be a UUID: %w", parseErr)
	}
	return req.ClientStreamID, true, nil
}

func (s *ChatService) StreamChatMessage(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (streamID string, err error) {
	defer logger.Guard("App.StreamChatMessage", &err)

	streamID, clientProvided, err := s.resolveStreamID(req)
	if err != nil {
		return "", err
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
	// Per-caller concurrency cap + stream-ID reservation, performed atomically
	// so two concurrent requests can't both observe active < cap and both
	// proceed, and can't both reserve the same client-provided stream ID.
	reserved, resErr := s.tryStartStream(streamID, scope, ctl)
	if resErr != nil {
		cancel()
		return "", resErr
	}
	if !reserved {
		cancel()
		return "", fmt.Errorf("too many chat responses running at once (max %d) — wait for one to finish or stop it", maxConcurrentStreamsPerScope)
	}

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
			ctl.setError(msg)
			emit("error", map[string]interface{}{"message": msg})
		}

		// C-3: RAG lookup, provider resolution, budget, and demo-limiter checks.
		ts, err := s.prepareTurn(ctx, scope, doc, req)
		if err != nil {
			fail(err.Error())
			return
		}

		// Server-side history reconstruction: when the client omits Messages
		// (the efficient path — it sends only the new userMessage), load the
		// prior conversation from the store so the LLM gets full context
		// without the client re-sending the whole history each turn. A
		// non-empty Messages slice (legacy client, or a resend that locally
		// truncated history) is used as-is.
		if len(req.Messages) == 0 {
			// Hold the per-conversation mutex across reconstruct + persist so two
			// concurrent streams on the same (flowID, convKey) can't both read
			// the same history and then overwrite each other's user-turn persist
			// (a lost update — SaveConversation replaces the whole file).
			convKey := req.ContextBlockID
			if convKey == "" {
				convKey = "flow"
			}
			mu := s.convMutexFor(doc.ID, convKey)
			mu.Lock()
			req.Messages = s.reconstructHistory(ctx, doc, req)
			s.persistInitialUserTurn(ctx, doc, req)
			mu.Unlock()
		}

		aiReq, scrubbedDoc, ok, err := s.buildStreamRequest(ctx, ts, scope, doc, report, req)
		if err != nil {
			msg := "conversation is too long for this model's context window — start a new conversation or remove some history"
			fail(msg)
			return
		}
		if !ok {
			return // ctx cancelled while waiting on RAG
		}

		// When the caller opted into tools and the provider supports them, run the
		// read-only agentic tool loop (streamed turns + tool status events). It is
		// fully self-contained — emits chunk/tool/done/error and updates ctl — so the
		// normal streaming path below is skipped entirely (zero regression when off).
		if req.UseTools && ts.provider.SupportsTools() {
			s.runToolLoop(ctx, ts.provider, aiReq, scrubbedDoc, ctl, awaitStart, emit, coalesce.flushAndCount)
			return
		}

		// out masks secrets in the model's output (even across chunk splits),
		// maintains the resume buffer, and counts emitted chunk events; the done
		// event carries the count so the client can detect chunks dropped by a
		// saturated SSE buffer and recover the authoritative text via resume.
		out := newScrubbedEmitter(ctl, emit)
		err = ts.provider.Stream(ctx, aiReq, func(chunk ai.Chunk) {
			ctl.touch()
			if !awaitStart() {
				return
			}
			if chunk.Error != nil {
				out.flush()
				ctl.setError(chunk.Error.Error())
				emit("error", map[string]interface{}{"message": chunk.Error.Error()})
				return
			}
			if chunk.Done {
				out.flush()
				ctl.markDone(chunk.TokensIn, chunk.TokensOut)
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
			ctl.setError(msg)
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
		ctl.setError(msg)
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

		// Re-enforce the context window each turn: tool results + assistant
		// text accumulate and can overflow the model's context window by
		// iteration 4-6, surfacing as a raw provider 400. Truncating oldest
		// history here keeps the loop within bounds.
		if err := truncateForContextWindow(provider, &turn, provider.ContextLimit()); err != nil {
			fail("conversation is too long for this model's context window — too many tool results, try a simpler question")
			return
		}
		msgs = turn.Messages

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
			ctl.markDone(totalIn, totalOut)
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

func (s *ChatService) GetDemoRemaining() (int, error) {
	if s.demoLimiter == nil {
		return 0, nil
	}
	rem, _ := s.demoLimiter.Remaining()
	return rem, nil
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
