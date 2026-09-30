package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage"
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
// inFlightCost is the caller's own not-yet-persisted spend: usage records
// are written asynchronously after each provider turn, so a budget check that
// only reads the store lags the current stream by up to one full iteration
// (several turns' worth of tokens on a tool loop). The tool loops pass their
// locally-summed tokens priced at the turn's reservation rate
// (inFlightPricing, resolved once per turn); when the async record HAS landed
// both sides count, erring toward overstatement — the safe direction for a
// spend cap.
func inFlightCost(price ai.Pricing, tokensIn, tokensOut int) float64 {
	// Usage is whatever the upstream reported; a negative count (an
	// operator-allow-listed non-Microsoft host answering anything) must not
	// lower the ledger entry other streams' checks read.
	tokensIn, tokensOut = max(tokensIn, 0), max(tokensOut, 0)
	if tokensIn == 0 && tokensOut == 0 {
		return 0
	}
	return float64(tokensIn)/1e6*price.InputCostPerM + float64(tokensOut)/1e6*price.OutputCostPerM
}

// inFlightPricing is the rate a stream's in-flight spend and reservation are
// priced at. Every provider whose model ids are model names keeps its
// provider-wide rate. An alias provider (Azure AI Foundry) is not priced by
// baki — its cost is read in the Azure portal and the daily budget does not
// count it — so it reserves nothing.
func inFlightPricing(_ context.Context, p ai.Provider, _ string) ai.Pricing {
	if p == nil || ai.UsesModelAliases(p.ID()) {
		return ai.Pricing{}
	}
	return p.PricePerMillionTokens()
}

// self is the calling stream (nil before it is registered): its own ledger
// entry is excluded because the caller passes its own spend as inflight.
func (s *ChatService) enforceBudget(ctx context.Context, scope, orgID string, inflight float64, self *streamCtl) error {
	if s.settings == nil {
		return nil // no settings provider wired (bare-struct tests/local) — unlimited
	}
	settings := s.settings.Get()
	if settings == nil || settings.AI.DailyBudget <= 0 {
		return nil
	}
	usage, err := s.dailyUsage(ctx, scope, orgID)
	if err != nil {
		return budgetUnavailable(scope, err)
	}
	// Concurrent streams' in-flight spend (their spend so far plus one
	// reserved maximum response each). Without this term N streams started
	// together all passed the same store read; usage rows only land after
	// each turn completes.
	orgOthers, userOthers := s.othersInFlight(scope, orgID, self)
	concurrent := userOthers
	if orgID != "" {
		concurrent = orgOthers
	}
	if total := usage + inflight + concurrent; total >= settings.AI.DailyBudget {
		return &budgetError{
			kind: ErrAIBudgetExceeded,
			msg:  fmt.Sprintf("daily AI budget exceeded ($%.2f / $%.2f, of which $%.2f is in flight on other streams)", total, settings.AI.DailyBudget, concurrent),
		}
	}

	// When org-scoped, also enforce a per-user sub-limit so one member can't
	// exhaust the entire org's daily quota. scope is the caller's user ID in
	// cloud mode; passing "" as orgID queries that user's personal-only total.
	if orgID != "" && settings.AI.PerUserDailyBudget > 0 {
		userUsage, err := s.dailyUsage(ctx, scope, "")
		if err != nil {
			return budgetUnavailable(scope, err)
		}
		if total := userUsage + inflight + userOthers; total >= settings.AI.PerUserDailyBudget {
			return &budgetError{
				kind: ErrAIBudgetExceeded,
				msg:  fmt.Sprintf("per-user daily AI budget exceeded ($%.2f / $%.2f, of which $%.2f is in flight on other streams)", total, settings.AI.PerUserDailyBudget, userOthers),
			}
		}
	}
	return nil
}

// CheckBudget reports whether the caller may spend on AI for orgID ("" =
// personal) right now: the chat turn's daily-budget rule (org total plus the
// per-user sub-limit, other streams' in-flight spend included), with nothing
// of its own in flight. The knowledge base calls it before an upload or a
// re-index (rag.BudgetChecker) — both are billed embedding runs that used to
// bypass the daily budget entirely. Errors match ErrAIBudgetExceeded or
// ErrAIBudgetUnavailable.
func (s *ChatService) CheckBudget(ctx context.Context, scope, orgID string) error {
	return s.enforceBudget(ctx, scope, orgID, 0, nil)
}

// enforceTurnBudget is enforceBudget for a chat turn running on provider: an
// Azure AI Foundry turn (an alias provider — ai.UsesModelAliases) is never
// refused by the daily budget, at the turn's start or mid tool loop. baki
// does not price or count Azure usage (its cost is read in the Azure portal,
// Azure's own quotas are the limit), so a budget spent by other, priced
// providers must not stop it either.
func (s *ChatService) enforceTurnBudget(ctx context.Context, provider ai.Provider, scope, orgID string, inflight float64, self *streamCtl) error {
	if provider != nil && ai.UsesModelAliases(provider.ID()) {
		return nil
	}
	return s.enforceBudget(ctx, scope, orgID, inflight, self)
}

// othersInFlight sums the in-flight ledger of every active stream other than
// self: orgSum over streams attributed to orgID (when non-empty), userSum over
// streams owned by scope.
func (s *ChatService) othersInFlight(scope, orgID string, self *streamCtl) (orgSum, userSum float64) {
	s.activeStreams.Range(func(_, v interface{}) bool {
		c, ok := v.(*streamCtl)
		if !ok || c == self {
			return true
		}
		cost := c.inflight()
		if cost <= 0 {
			return true
		}
		if c.ownerID == scope {
			userSum += cost
		}
		if orgID != "" && c.orgID == orgID {
			orgSum += cost
		}
		return true
	})
	return orgSum, userSum
}

// maxOutputTokens is the per-response reservation basis: the request's cap,
// or the providers' common default when the caller left it unset.
func maxOutputTokens(req ai.Request) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return 4096
}

// modelOutputInfo reads the provider's listing entry for model: its output
// ceiling (0 = unknown, as ai.ModelMaxOutputTokens) and the model it serves
// when the id is only an alias for it (an Azure AI Foundry deployment name).
func modelOutputInfo(ctx context.Context, p ai.Provider, model string) (maxOutput int, served string) {
	models, err := p.Models(ctx)
	if err != nil {
		return 0, ""
	}
	for _, m := range models {
		if m.ID == model {
			return m.MaxOutputTokens, m.ServedModel
		}
	}
	return 0, ""
}

// reasoningFloorTokens raises a max-tokens budget below the reasoning floor
// of the turn's model (ai.ReasoningOutputFloor, known by its id or the model
// the listing says it serves) to that floor; other budgets are returned
// unchanged. Reasoning models count hidden reasoning against the same cap as
// the answer, so the app's 4096 default often ended such a turn with no
// answer at all (PROVIDER_OUTPUT_LIMIT). An unset budget (0) is raised too:
// the providers' own defaults (4096–8192) are below the floor.
//
// The floor never goes above maxOutput, the model's output ceiling (0 =
// unknown): a catalogue cap, or an Azure AI Foundry registry entry's "Max
// output tokens" — a small distilled model the user capped must not be sent
// (or reserved) 16384 tokens because its deployment name reads like a
// reasoning model's. A budget already above the ceiling is left to the
// caller's clamp (normalizeChatParams).
func reasoningFloorTokens(maxTokens int, model, served string, maxOutput int) int {
	floor := ai.ReasoningOutputFloor(model, served)
	if maxOutput > 0 && floor > maxOutput {
		floor = maxOutput
	}
	if floor > maxTokens {
		return floor
	}
	return maxTokens
}

// turnGenerationParams is the turn's temperature and max tokens on m, sized
// by its provider-level id (m.resolved — never an Azure AI Foundry registry
// id): normalizeChatParams (ranges, the model's output ceiling, the
// context-window backstop), then the reasoning floor (reasoningFloorTokens)
// re-capped by the same ceiling and backstop — the floor never pushes a
// request past what the model can produce. The ceiling is the listing
// entry's, a registry entry's "Max output tokens" override included (the
// audited listing applies it). The backstop is the MODEL's context window
// (ai.ContextLimitFor — the listing entry or a learned window), not the
// provider-wide placeholder: a 1M-window Claude deployment's 128000-token
// output cap used to be clamped to 124000 by the Azure provider's 128000
// default window (F-29).
//
// A request that leaves max tokens unset (an API client; the web client
// always sends one) goes out with the provider's default — 4096, 8192 on
// Claude, none at all on an agent run — which a registry entry's override
// may be below, or a small window cannot hold beside the prompt
// (outputBackstop below the common default: a registry entry's window
// override, a local server's small context). There the common default is
// sent instead — capped at the override, and by the backstop like any cap —
// so the truncation reserves what is really sent (truncationReserve).
func turnGenerationParams(ctx context.Context, p ai.Provider, req models.ChatRequest, m turnModel) (float64, int) {
	maxOutput, served := modelOutputInfo(ctx, p, m.resolved)
	ctxWindow := ai.ContextLimitFor(ctx, p, m.resolved)
	requested := req.MaxTokens
	if requested <= 0 {
		switch {
		case m.maxOutput > 0:
			requested = min(maxOutputTokens(ai.Request{}), m.maxOutput)
		case ctxWindow > 0 && outputBackstop(ctxWindow) < maxOutputTokens(ai.Request{}):
			requested = maxOutputTokens(ai.Request{})
		}
	}
	temperature, maxTokens := normalizeChatParams(req.Temperature, requested, ctxWindow, maxOutput)
	if raised := reasoningFloorTokens(maxTokens, m.resolved, served, maxOutput); raised != maxTokens {
		_, maxTokens = normalizeChatParams(temperature, raised, ctxWindow, maxOutput)
	}
	return temperature, maxTokens
}

// errStreamIDInUse is returned by tryStartStream when a client-provided stream
// ID collides with an active or recently-finished stream. It is a sentinel so
// the caller can distinguish a collision (reject the request as "already in
// use") from the per-caller cap being exceeded.
var errStreamIDInUse = errors.New("clientStreamId already in use")

// ErrTooManyChatStreams is returned when the caller's concurrent-stream cap
// (maxConcurrentStreamsPerScope) is exceeded. Exported so the HTTP layer can
// map it to a distinct machine-readable error code the frontend switches on.
var ErrTooManyChatStreams = errors.New("too many chat responses running at once")

// convMutexFor returns the mutex serializing the reconstruct-history +
// persist-user-turn critical section for a conversation.
//
// STRIPED, not exact-keyed: the original LoadOrStore map grew one *sync.Mutex
// per (flowID, convKey) FOREVER — a long-lived process over many flows
// accumulated unbounded lock entries for a critical section that holds for
// two storage round-trips. A fixed 256-way stripe set (hash of the key) gives
// the same mutual exclusion for any single conversation with bounded, constant
// memory; distinct conversations merely sharing a stripe serialize behind one
// another, which the two-I/O critical section tolerates fine.
const convMutexStripes = 256

func (s *ChatService) convMutexFor(flowID, convKey string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(flowID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(convKey))
	return &s.convMutexes[h.Sum32()%convMutexStripes]
}

// reconstructAndPersistUserTurn runs the reconstruct-history + persist-user-turn
// critical section under the per-conversation mutex, holding it across both so
// two concurrent streams on the same (flowID, convKey) can't read the same
// history and then clobber each other's persist (a lost user turn — persist does
// a full-file replace).
//
// The defer-unlock is essential, not stylistic: both calls do storage I/O and
// can panic. The stream goroutine's top-level recover() swallows the panic but
// does NOT release a manually-unlocked mutex, and this mutex lives in the
// persistent convMu map (via convMutexFor) — so a manual Lock/Unlock without
// defer would leave it held forever on panic, deadlocking every future stream on
// this conversation. req is taken by pointer so the reconstructed history
// propagates back to the caller. Returns whether the user's turn is now stored
// (see persistInitialUserTurn).
func (s *ChatService) reconstructAndPersistUserTurn(ctx context.Context, doc *models.FlowDocument, req *models.ChatRequest, convKey string) bool {
	mu := s.convMutexFor(doc.ID, convKey)
	mu.Lock()
	defer mu.Unlock()
	history, err := s.reconstructHistory(ctx, doc, *req)
	if err != nil {
		// Do NOT persist. persistInitialUserTurn writes req.Messages + the new
		// turn through SaveConversation, which is a FULL REPLACE
		// (`ON CONFLICT (flow_id, scope) DO UPDATE SET messages`). Persisting a
		// history we failed to read therefore deletes whatever was stored and
		// leaves only the message just typed — measured at 10 messages down to
		// 1 from a single transient load error, behind a log line nobody reads.
		//
		// The turn still proceeds with no prior context: a degraded answer is a
		// far better outcome than a destroyed record, and the next successful
		// turn restores full context because nothing was overwritten.
		logger.Warn("chat: history load failed; answering without prior context and "+
			"skipping persistence so the stored conversation is not overwritten", "error", err)
		req.Messages = nil
		return false
	}
	req.Messages = history
	return s.persistInitialUserTurn(ctx, doc, *req)
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
		return true // count every entry — an early-out here under-counts and
		// lets the cap be exceeded by one (the just-stored entry).
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

// readOnlyAnalysis adapts AnalysisService to the tool loop's ToolAnalysis
// using the read-only entry point: tool-driven analyses never record into the
// report-history pair / trend store and never emit progress events, so the
// chat's internal analyses can't overwrite the UI's CurrentReport or flash
// its progress bar mid-conversation.
type readOnlyAnalysis struct{ inner *AnalysisService }

func (r readOnlyAnalysis) AnalyzeFlow(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
	if r.inner == nil {
		return nil, fmt.Errorf("analysis not available")
	}
	return r.inner.AnalyzeFlowReadOnly(ctx, doc)
}

func (r readOnlyAnalysis) GetVariableLineage(doc *models.FlowDocument, varName string) (*models.VariableHistory, error) {
	if r.inner == nil {
		return nil, fmt.Errorf("analysis not available")
	}
	return r.inner.GetVariableLineage(doc, varName)
}

// ChatService owns chat stream state and operations.
type ChatService struct {
	notifier      EventNotifier
	configDir     string
	flowCache     *FlowService
	analysisCache *AnalysisService
	settings      SettingsProvider
	factory       *ai.ProviderFactory
	backend       chatStore
	// mode selects where conversations persist: ModeLocal uses the on-disk file
	// store (desktop), cloud routes through the backend (Postgres + RLS).
	mode      config.DeploymentMode
	knowledge *rag.KnowledgeService
	// watchdogInterval / idleTimeout / hostedIdleTimeout override
	// subscriberCheckInterval, streamIdleTimeout and hostedStepIdleTimeout in
	// tests; 0 ⇒ defaults.
	watchdogInterval  time.Duration
	idleTimeout       time.Duration
	hostedIdleTimeout time.Duration
	// heartbeatAfter / heartbeatTick override the client keepalive timing in
	// tests (see heartbeatStream); 0 ⇒ defaults.
	heartbeatAfter time.Duration
	heartbeatTick  time.Duration
	activeStreams  sync.Map   // map[streamID]*streamCtl — in-flight streams
	streamCapMu    sync.Mutex // serializes the per-caller concurrency check + store
	// finishedStreams holds recently-completed streams for a short grace period
	// (resumeRetention) so a client that reconnects after a stream ended can
	// still fetch its final buffer via ResumeStream.
	finishedStreams sync.Map // map[streamID]*streamCtl
	// convMutexes stripes the reconstruct/persist critical section across a
	// fixed lock set (see convMutexFor): same mutual exclusion per
	// conversation, constant memory. Without ANY lock two concurrent streams
	// on one conversation reconstructed the same history and clobbered each
	// other's SaveConversation (a full-file replace), silently losing a user
	// turn.
	convMutexes [convMutexStripes]sync.Mutex
	// resume mirrors resumable stream state to a shared backplane so a client
	// that reconnects to a DIFFERENT replica can still fetch the buffer. The
	// default is a no-op (single-replica); SetResumeBackplane swaps in Redis.
	resume resumeStore

	// pendingFixDecisions holds apply_fix proposals awaiting the stream
	// owner's approve/decline (chat_fixes.go). Keyed by proposal ID (UUID);
	// entries carry their stream ID and are removed when the wait resolves.
	pendingFixDecisions sync.Map
	// applyFixFunc overrides the FlowService-backed fix mutation in tests
	// (nil ⇒ FlowService.ApplyFix). See chat_fixes.go.
	applyFixFunc applyFixFunc
	// fixDecisionWait overrides fixDecisionTimeout in tests (0 ⇒ default).
	fixDecisionWait time.Duration

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
	// chatCtxGenMu serializes the read-modify-write of the per-flow generation
	// counter in InvalidateChatContext. The LRU get/set are individually
	// thread-safe but the increment is not atomic: without this lock two
	// concurrent invalidations both read gen=N and both write N+1, losing an
	// increment and serving a stale scrubbed-context cache entry to a chat turn.
	chatCtxGenMu sync.Mutex

	// azure is the Azure AI Foundry model registry (Part 9): with it, an
	// azure-foundry turn's model id — "m/<entryId>", a pre-registry id for
	// one release, or "" for the server default — resolves to one resource
	// and one entry before the stream starts (resolveAzureModel), and the
	// turn runs on exactly that resource's credential. nil keeps the
	// single-document behaviour.
	azure *AzureModelService
	// azureConnection builds the provider of one registry resource; nil ⇒
	// the factory's ForAzureConnection. Tests script it.
	azureConnection func(scope string, ref ai.AzureConnectionRef, entries []ai.AzureRegisteredModel) (ai.Provider, error)
}

func NewChatService(
	notifier EventNotifier,
	configDir string,
	flowCache *FlowService,
	analysisCache *AnalysisService,
	settings SettingsProvider,
	factory *ai.ProviderFactory,
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

// SetAzureModels wires the Azure AI Foundry model registry (Part 9): chat
// turns and context previews on azure-foundry then resolve their model id on
// it (resolveAzureModel). Set once at boot, before requests are served; nil
// keeps the single-document behaviour.
func (s *ChatService) SetAzureModels(azure *AzureModelService) { s.azure = azure }

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

// turnSetup is the resolved provider + model + attribution + concurrent RAG
// lookup for one turn — the output of prepareTurn. ragDone is nil when RAG
// isn't wired for this doc/knowledge combination.
type turnSetup struct {
	provider ai.Provider
	// model is what the turn runs on; every reader of the model takes it
	// from here, never from the request (see turnModel).
	model   turnModel
	orgID   string
	ragDone <-chan string
}

// turnModel is the model one turn (or its preview) runs on, as every reader
// needs it. It is fixed before the stream starts — for Azure AI Foundry on
// the model registry by AzureModelService.ResolveChat — and travels in
// turnSetup, never through the request: prepareTurn takes the request by
// value, and the request's model is still read after it returns.
type turnModel struct {
	// resolved is the provider-level id every reader sizes and sends by —
	// the in-flight reservation, the generation params, the stream request's
	// Model and its context window, the tool-loop gate, usage rows and the
	// provider's memos: the request's model for every provider but Azure AI
	// Foundry on the registry, where it is the entry's "<deployment>",
	// "claude:<deployment>" or "agent:<name>".
	resolved string
	// requested is the id the answer is persisted with: the request's model,
	// or — for "" on the registry — the server default's "m/<entryId>".
	requested string
	// label is the registry entry's display name at send time, stored with
	// the answer (ChatMessage.ModelLabel); "" off the registry.
	label string
	// kind is the registry entry's kind — chat, claude or agent; "" off the
	// registry.
	kind string
	// maxOutput is the registry entry's "Max output tokens" override (0 =
	// none, and off the registry). The listing already caps by it; this is
	// what caps a request that left max tokens unset (turnGenerationParams).
	maxOutput int
}

// newTurnModel is the turn model of req: res's when the registry resolved
// the request (res non-nil), else the request's own model.
func newTurnModel(req models.ChatRequest, res *AzureChatResolution) turnModel {
	if res == nil {
		return turnModel{resolved: req.Model, requested: req.Model}
	}
	m := turnModel{resolved: res.ResolvedModel, requested: res.RequestedModel, label: res.ModelLabel, kind: res.Kind}
	for _, e := range res.Entries {
		if res.EntryID != "" && e.EntryID == res.EntryID {
			m.maxOutput = e.MaxOutputTokens
		}
	}
	return m
}

// agent reports a Foundry agent turn: a registry agent entry, or a raw
// "agent:<name>" id.
func (m turnModel) agent() bool {
	return m.kind == ai.AzureModelKindAgent || ai.IsAgentModel(m.resolved)
}

// resolveAzureModel resolves the model id of an Azure AI Foundry turn (or
// preview) on the model registry, synchronously and before any stream
// starts: nil, nil for every other provider and while the registry is not
// wired (the single-document behaviour). The caller's own entries come first,
// then the flow organisation's — only for its admins and members
// (registryCredentialOrg; the organisation prepareTurn attributes the turn
// to).
//
// An id the caller cannot use — unknown, removed, another owner's, an
// embedding model, an organisation's the caller lost access to — is
// ErrModelNotAvailable, which the chat handlers answer with 400
// MODEL_NOT_AVAILABLE (the client puts the text back in the composer). Any
// other failure (the registry, the keystore or the caller's organisation
// role could not be read) is never a fallback to another model or
// credential: it is logged and replaced by the generic setup error
// (providerSetupError), whose copy — unlike the raw store error
// classifyFailureBase would echo — is safe to show. A failed role lookup
// still lets the caller's own entries answer (they come first, so theirs is
// the answer either way); only a miss, which the organisation's entries
// might have answered, is that setup error rather than a "removed" model.
func (s *ChatService) resolveAzureModel(ctx context.Context, scope string, doc *models.FlowDocument, req models.ChatRequest) (*AzureChatResolution, error) {
	if s.azure == nil || req.Provider != ai.AzureFoundryProviderID {
		return nil, nil
	}
	orgID := ""
	if doc != nil {
		orgID = doc.OrganizationID
	}
	credOrg, roleErr := s.registryCredentialOrg(ctx, orgID, scope)
	res, err := s.azure.ResolveChat(ctx, scope, credOrg, req.Model)
	switch {
	case err == nil:
		return &res, nil
	case errors.Is(err, ErrModelNotAvailable) && roleErr != nil:
		// Not the caller's own, and the organisation's entries could not
		// be consulted: "not available" would be a guess.
		return nil, providerSetupError(scope, req.Provider, roleErr)
	case errors.Is(err, ErrModelNotAvailable):
		return nil, ErrModelNotAvailable
	default:
		return nil, providerSetupError(scope, req.Provider, err)
	}
}

// turnProvider builds the provider a turn (or its preview) runs on: with a
// registry resolution, exactly the resolved resource's credential — the
// caller's own, or the organisation's shared one (ForAzureConnection; no
// member-first fallback between the two: the registry chose the resource);
// otherwise the factory's member-first rule for req.Provider, with the flow
// organisation's shared credential only for its admins and members. A
// registry resource whose credential is gone reports errAzureNoCredentials
// (still ai.ErrKeyNotConfigured), one whose stored credential the keystore
// cannot decrypt (the registry's "unreadable") errAzureUnreadableCredentials,
// so the copy names the resource and says the credentials must be entered
// again — neither is a "try again in a moment".
func (s *ChatService) turnProvider(ctx context.Context, scope, orgID string, req models.ChatRequest, res *AzureChatResolution) (ai.Provider, error) {
	if res == nil {
		return s.factory.ForOrg(scope, s.credentialOrg(ctx, orgID, scope), req.Provider)
	}
	build := s.azureConnection
	if build == nil {
		build = s.factory.ForAzureConnection
	}
	p, err := build(scope, res.ConnectionRef, res.Entries)
	switch {
	case errors.Is(err, ai.ErrKeyNotConfigured):
		return nil, fmt.Errorf("%w: %w", errAzureNoCredentials, err)
	case errors.Is(err, storage.ErrSecretDecryptFailed):
		return nil, fmt.Errorf("%w: %w", errAzureUnreadableCredentials, err)
	}
	return p, err
}

// errProviderDisabled is the sentinel behind a turn refused because the
// settings disable the requested provider (see providerDisabledByPolicy).
// Match with errors.Is; the wrapping error's message is the client copy.
var errProviderDisabled = errors.New("AI provider disabled by policy")

// providerDisabledByPolicy reports whether the deployment settings disable
// providerID for every chat turn. AIProviderConfig.Enabled predates this
// check and no UI writes it, so it ships as a plain bool whose zero value —
// false — is what BOTH DefaultSettings and the frontend's store persist for
// every provider but the default active one (claude): a stored false is
// indistinguishable from an unset flag through the model. Treating every
// false as disabled would therefore cut off openai/gemini/... on every
// existing deployment the moment this shipped. The guard fires only on an
// unambiguous, explicit disable: an entry for a provider the defaults ENABLE
// whose stored entry says false. An absent entry, or one matching the
// shipped default, reads as enabled — exactly the pre-guard behavior.
func providerDisabledByPolicy(settings *models.AppSettings, providerID string) bool {
	if settings == nil {
		return false
	}
	pc, ok := settings.AI.Providers[providerID]
	if !ok || pc.Enabled {
		return false
	}
	return models.DefaultSettings().AI.Providers[providerID].Enabled
}

// prepareTurn resolves the provider, computes org attribution, enforces the
// daily AI budget (never on Azure AI Foundry — see enforceTurnBudget), and —
// if a knowledge base is configured for the flow's org — kicks off the
// per-turn RAG guidelines lookup concurrently so its embedding API call
// overlaps with the history reconstruction and context build that follow
// instead of gating first token (C-3). The lookup starts AFTER the policy and
// budget gates: it is a billed embedding call, and a refused turn must not
// spend. The returned ragDone channel (if non-nil)
// delivers exactly one value; the caller must receive from it (or abandon it
// on ctx cancellation) before finishing the turn.
//
// ctl is the stream's (already published) control: one maximum response is
// reserved on it BEFORE the budget check, with the check excluding ctl's own
// entry, so streams whose starts overlap see each other's reservation. The
// reservation used to be published only after the check (and after the
// history reconstruction's DB round trips), a window in which every
// simultaneously started stream passed the same store read.
//
// azure is the turn's model registry resolution (resolveAzureModel; nil off
// the registry): the provider is built for exactly its resource, and the
// returned turnSetup.model carries the resolved id every later reader uses.
func (s *ChatService) prepareTurn(ctx context.Context, scope string, doc *models.FlowDocument, req models.ChatRequest, azure *AzureChatResolution, ctl *streamCtl) (turnSetup, error) {
	// Org-scoped flows attribute usage and enforce the daily budget at the
	// org level; personal flows fall back to the per-user total. Resolved
	// before the provider because an org flow may also run on the org's
	// shared credential (G7) when the member has none of their own.
	//
	// The shared credential itself is only for the org's admins and members
	// (credentialOrg): a per-flow collaborator outside the org, a viewer or a
	// guest runs on their own key even on an org flow — whose usage is still
	// attributed to, and budgeted against, the flow's org (orgID).
	orgID := ""
	if doc != nil {
		orgID = doc.OrganizationID
	}
	// On the registry the credential is known before the provider is built
	// (the resolved resource is the organisation's or the caller's own), so
	// even a failed build — the organisation's resource lost its credential
	// — keeps the org "ask your admin" copy.
	shared := azure != nil && azure.ConnectionRef.OrgID != ""
	if ctl != nil && shared {
		ctl.sharedCredential.Store(true)
	}
	provider, err := s.turnProvider(ctx, scope, orgID, req, azure)
	if err != nil {
		return turnSetup{}, providerSetupError(scope, req.Provider, err)
	}
	if azure == nil {
		shared = ai.UsedOrgCredential(provider)
	}
	if ctl != nil {
		// Read by classifyFailure: a rejected shared credential is the org
		// admin's to fix, not the member's.
		ctl.sharedCredential.Store(shared)
	}
	model := newTurnModel(req, azure)
	// Policy gate on the resolved provider (its ID came from the factory's
	// own table, so it is safe to echo back): the same settings load
	// chat_context.go uses for ContextTokenBudget, consulted here so every
	// entry point that prepares a turn enforces it.
	var appSettings *models.AppSettings
	if s.settings != nil {
		appSettings = s.settings.Get()
	}
	if providerDisabledByPolicy(appSettings, provider.ID()) {
		return turnSetup{}, fmt.Errorf("%w: %s is disabled in this deployment's AI settings — pick another provider or ask your administrator to re-enable it", errProviderDisabled, provider.ID())
	}

	// Reserve one maximum response before the check so starts that overlap
	// see each other. req.MaxTokens is pre-clamp (normalizeChatParams runs in
	// buildStreamRequest): reserving slightly more is the safe direction —
	// the window backstop only ever lowers what is sent, and an unset cap
	// reserves the providers' common default (maxOutputTokens), the most
	// turnGenerationParams sets in its place on a small window. A reasoning
	// model's turn is sent with at least the reasoning floor
	// (turnGenerationParams), capped at the model's output ceiling, so that is
	// what it reserves. All three readers take the resolved id.
	if ctl != nil {
		maxOutput, served := modelOutputInfo(ctx, provider, model.resolved)
		reserveTokens := maxOutputTokens(ai.Request{MaxTokens: reasoningFloorTokens(req.MaxTokens, model.resolved, served, maxOutput)})
		ctl.setInflight(inFlightCost(inFlightPricing(ctx, provider, model.resolved), 0, reserveTokens))
	}
	if err := s.enforceTurnBudget(ctx, provider, scope, orgID, 0, ctl); err != nil {
		return turnSetup{}, err
	}

	var ragDone chan string
	if s.knowledge != nil && doc != nil && doc.OrganizationID != "" {
		ragDone = make(chan string, 1)
		ragCtx, ragCancel := context.WithTimeout(ctx, ragGuidelinesDeadline)
		go func() {
			defer ragCancel()
			defer func() {
				if r := recover(); r != nil {
					// A panic in the embedding/KB path (nil provider, malformed
					// vector, pgvector decode bug) must not crash the process.
					// Deliver an empty result so the awaiting turn consumer
					// doesn't block until ctx.Done() — the turn proceeds
					// without RAG guidelines.
					logger.Error("rag guidelines lookup panicked", "scope", scope, "panic", r)
					select {
					case ragDone <- "":
					default:
					}
				}
			}()
			ragDone <- s.ragGuidelines(ragCtx, scope, doc, req.UserMessage)
		}()
	}

	return turnSetup{provider: provider, model: model, orgID: orgID, ragDone: ragDone}, nil
}

// persistInitialUserTurn stores the user's turn at stream start (BUG-5) so
// closing the app mid-stream (or a crash before onDone) doesn't lose the typed
// message. Only called on the history-reconstruction path — a client that sent
// Messages explicitly (legacy client, or a resend that locally truncated
// history) owns persistence itself. Synchronous and before the provider dial;
// errors are non-fatal (a store hiccup just means the turn isn't pre-persisted,
// the turn itself still runs). Returns whether the turn was stored — false on
// a store error and when "Save conversation history" is off — which is what
// lets GET conversation report the stream to a reloading client (F-21).
func (s *ChatService) persistInitialUserTurn(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest) bool {
	if doc == nil || req.UserMessage == "" {
		return false
	}
	convKey := req.ConversationKey()
	userID := clientMessageID(req.UserMessageID)
	// A client id that already names a stored message (a resend reusing the
	// id) gets a fresh one rather than a duplicate id in the history.
	if slices.ContainsFunc(req.Messages, func(m models.ChatMessage) bool { return m.ID == userID }) {
		userID = uuid.NewString()
	}
	userMsg := models.ChatMessage{
		ID:             userID,
		Role:           "user",
		Content:        req.UserMessage,
		Timestamp:      time.Now(),
		ContextBlockID: req.ContextBlockID,
	}
	persisted := make([]models.ChatMessage, 0, len(req.Messages)+1)
	persisted = append(persisted, req.Messages...)
	persisted = append(persisted, userMsg)
	stored, err := s.saveConversation(ctx, doc, convKey, persisted)
	if err != nil {
		logger.Warn("chat: failed to persist user turn at stream start", "error", err)
	}
	return stored
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
	//
	// PERF-8: the guidelines depend on the turn's question, so they go into
	// the final user message (buildMessages), never into the system prompt:
	// Azure and OpenAI cache prompt prefixes automatically, and a system
	// prompt that changes per turn made the whole history prefill uncached
	// on every turn. sysPrompt stays core.sysPrompt — byte-stable across the
	// conversation.
	guidelines := ""
	if ts.ragDone != nil {
		select {
		case guidelines = <-ts.ragDone:
		case <-ctx.Done():
			return ai.Request{}, nil, false, nil
		}
	}
	scrubbedDoc = core.scrubbedDoc
	sysPrompt := scrubber.ScrubText(core.sysPrompt)
	contextText := scrubber.ScrubText(core.contextText)

	// The provider-level id (ts.model.resolved), never the request's: for
	// Azure AI Foundry on the registry the request carries "m/<entryId>",
	// which no deployment answers to.
	temperature, maxTokens := turnGenerationParams(ctx, ts.provider, req, ts.model)
	aiReq = ai.Request{
		SystemPrompt: sysPrompt,
		Messages:     buildMessages(req, contextText, scrubber.ScrubText(guidelines)),
		Model:        ts.model.resolved,
		Temperature:  temperature,
		// An explicit temperature (0 included) is sent as chosen; unset,
		// a zero Temperature keeps meaning "the provider's default".
		TemperatureSet: req.TemperatureSet,
		MaxTokens:      maxTokens,
		// The per-provider reasoning-depth choice (F-19): "" keeps the
		// provider default; validateReasoningEffort refused anything but
		// low/medium/high at stream start.
		ReasoningEffort: req.ReasoningEffort,
		OrgID:           ts.orgID,
	}

	// Enforce the model's context window before sending: truncate oldest
	// history if the prompt is too large, or fail cleanly with
	// ErrContextLimit if even the system prompt + current turn overflow.
	// Without this, an oversized prompt reaches the provider and surfaces as
	// a raw, confusing 400 ("context length exceeded") instead of a friendly
	// "conversation too long" message.
	//
	// Model-aware: the selected model's catalog window (e.g. 1M for Claude
	// Opus/Sonnet 4.6) overrides the smaller provider-wide default, so
	// truncation doesn't throw away history that would have fit. The current
	// turn is the last message buildMessages appended.
	if _, err := truncateForContextWindow(ctx, ts.provider, &aiReq, ai.ContextLimitFor(ctx, ts.provider, ts.model.resolved), len(aiReq.Messages)-1); err != nil {
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

// validateConversationID rejects a conversationId that is not a UUID. The
// value becomes a storage key (and, in local mode, a path segment), so it must
// be a client-generated thread id rather than free text.
func validateConversationID(req models.ChatRequest) error {
	if req.ConversationID == "" {
		return nil
	}
	if _, err := uuid.Parse(req.ConversationID); err != nil {
		// ErrInvalidInput so the HTTP layer answers 400 and the caller can read
		// why: unwrapped, this surfaced as a 500 whose message was masked to
		// "internal server error" — a malformed client field reported as a
		// server fault.
		return fmt.Errorf("%w: conversationId must be a UUID: %w", ErrInvalidInput, err)
	}
	return nil
}

// validReasoningEfforts are the ReasoningEffort values a chat request may
// carry (models.ChatRequest.ReasoningEffort, merged from the per-provider
// setting by the client's request builder): the provider default or one of
// the three levels every reasoning vendor shares. "minimal", "none", "xhigh"
// and "max" are model-dependent (F-19's verifier notes) and are refused
// rather than silently mapped; the send-side repair handles a level a
// deployment rejects.
func validReasoningEffort(effort string) bool {
	switch effort {
	case "", "low", "medium", "high":
		return true
	}
	return false
}

// validateReasoningEffort rejects a reasoning-effort value no provider can
// send, at stream start, so a hand-crafted request cannot smuggle a
// model-dependent level onto the wire.
func validateReasoningEffort(effort string) error {
	if !validReasoningEffort(effort) {
		return fmt.Errorf("%w: reasoningEffort must be one of low, medium or high (omit it for the provider default)", ErrInvalidInput)
	}
	return nil
}

func (s *ChatService) StreamChatMessage(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (streamID string, err error) {
	defer logger.Guard("App.StreamChatMessage", &err)

	// Per-flow chat context: when the handler didn't supply a report (it passes
	// nil because it doesn't hold AnalysisService), resolve the flow's cached
	// analysis here so the chat is grounded in the flow's latest findings
	// WITHOUT re-analyzing on every turn. Falls through to nil (no grounding)
	// when no analysis has been run yet — the agentic tool loop still works.
	if report == nil && s.analysisCache != nil && doc != nil {
		if cached, ok := s.analysisCache.CurrentReport(doc); ok {
			report = cached
		}
	}

	streamID, clientProvided, err := s.resolveStreamID(req)
	if err != nil {
		return "", err
	}
	if err := validateConversationID(req); err != nil {
		return "", err
	}
	if err := validateReasoningEffort(req.ReasoningEffort); err != nil {
		return "", err
	}
	// Azure AI Foundry on the model registry: the model id resolves here,
	// before any stream exists, so a model the caller can no longer use is
	// this request's answer (400 MODEL_NOT_AVAILABLE — the client puts the
	// text back in the composer) instead of a failed bubble. A registry or
	// keystore failure (azureErr, already the generic setup error) is
	// reported on the stream like every other provider-setup failure. The
	// resolution travels to the worker as a value; req itself is never
	// changed.
	azureRes, azureErr := s.resolveAzureModel(ctx, scope, doc, req)
	if errors.Is(azureErr, ErrModelNotAvailable) {
		return "", azureErr
	}

	// The stream deliberately outlives the HTTP request that created it (begin/
	// cancel/resume are separate requests; chunks are delivered over SSE), so it
	// must NOT inherit r.Context() — net/http cancels that the instant the create
	// handler returns, which would abort the provider call (notably Copilot's
	// session-token exchange on a cold cache) with "context canceled". Cancellation
	// is handled explicitly via ctl.cancel() (CancelStream) and the timeout below.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxChatStreamDuration)
	// orgID is set here, before the control is published: othersInFlight reads
	// it from other goroutines without a lock, so a later write would race.
	// flowID/convKey are immutable for the same reason (ActiveStreamFor reads
	// them from the conversation GET handler without a lock) and let the
	// handler report this stream as the conversation's active one (F-21).
	orgID, flowID := "", ""
	if doc != nil {
		orgID = doc.OrganizationID
		flowID = doc.ID
	}
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{}), settled: make(chan struct{}), ownerID: scope, orgID: orgID, streamID: streamID, flowID: flowID, convKey: req.ConversationKey()}
	ctl.touch() // pre-first-chunk work (context build, provider dial) counts as activity
	ctl.lastEmit.Store(time.Now().UnixNano())
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
		return "", fmt.Errorf("%w (max %d) — wait for one to finish or stop it", ErrTooManyChatStreams, maxConcurrentStreamsPerScope)
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
	// snapshots the growing buffer until the worker's cleanup closes
	// mirrorFinished — after the terminal state is recorded, so the last
	// snapshot published is the terminal one. No-op single-replica.
	var mirrorFinished chan struct{}
	if rs := s.resumeBackplane(); rs.enabled() {
		rs.Save(ctx, streamID, ctl.snapshot(), maxChatStreamDuration+resumeRetention)
		mirrorFinished = make(chan struct{})
		go s.mirrorStream(ctx, streamID, ctl, mirrorFinished)
	}

	go s.watchStream(ctx, streamID, scope, ctl)

	emit := func(eventType string, data map[string]interface{}) {
		// Journal first (under the ctl lock, ordered with the buffer): agentic
		// events survive a client disconnect via ResumeResult.Events / the
		// BeginStream replay. Runs on the coalescer's inner path too — chunk
		// events are simply not journaled.
		ctl.mu.Lock()
		ctl.recordJournal(eventType, data)
		ctl.mu.Unlock()
		ctl.lastEmit.Store(time.Now().UnixNano())
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
		// persistAnswer is set once the turn is known to be server-owned (the
		// client omitted Messages); the answer is stored from the cleanup
		// below, AFTER the stream has left the active set — the write is a
		// bounded, best-effort DB round trip that must not keep the stream's
		// concurrency slot and ledger entry alive.
		persistAnswer := false
		// turn is the turn's model once prepareTurn has set the turn up: the
		// answer is stored with its requested id and label, never the
		// provider-level id it was sent with.
		var turn turnModel
		// On exit, remove the stream from the active set (so cancel/begin and
		// leak-detection see it as gone) and move it to the finished set for a
		// short grace period, so a client that reconnects after completion can
		// still fetch the final buffer via ResumeStream instead of hanging.
		defer func() {
			// Every exit path must leave a terminal state behind (a chunk
			// dropped at the begin gate, a panic): record one when nothing
			// did, and tell a subscribed client. A stream that never began
			// gets it through BeginStream/ResumeStream instead.
			if code, msg, set := ctl.ensureTerminal(ctx); set {
				select {
				case <-ctl.started:
					// Never shared: this path only records cancelled or interrupted
					// streams — a credential failure stores its own state above.
					emit("error", errorEventData(code, msg, false))
				default:
				}
			}
			coalesce.flush() // deliver any batched-but-unsent tail before exit
			cancel()
			if mirrorFinished != nil {
				close(mirrorFinished) // done/error are recorded by now; the mirror publishes them last
			}
			// Finished set first: a conversation GET between the two calls
			// must still find the stream (ActiveStreamFor), or a reload in
			// that instant saw neither the stream nor the stored answer.
			s.finishedStreams.Store(streamID, ctl)
			s.activeStreams.Delete(streamID)
			time.AfterFunc(resumeRetention, func() { s.finishedStreams.Delete(streamID) })
			if persistAnswer {
				s.persistAssistantTurn(ctx, doc, req, ctl, turn.requested, turn.label)
			}
			close(ctl.settled)
		}()

		// fail stores the error in the stream buffer AND emits the SSE event so
		// both live SSE clients and reconnecting clients (via resumeStream) see it.
		// It deliberately does NOT wait for BeginStream: pre-stream failures must
		// surface immediately instead of parking the goroutine (and the client)
		// until begin or the 5-minute cap. The errMsg must be stored before the
		// emit — BeginStream returns the stored state when the emit predated the
		// client's SSE subscription.
		fail := func(code, msg string, shared bool) {
			msg = scrubber.ScrubText(msg) // provider strings can echo request content
			ctl.setFailure(code, msg, shared)
			emit("error", errorEventData(code, msg, shared))
		}

		// Client keepalive for silent phases (see heartbeatStream). Not joined
		// on exit: in a microsecond window a heartbeat can trail the terminal
		// event, and the client drops events for a stream it has already torn
		// down — the deferred close is enough.
		hbStop := make(chan struct{})
		defer close(hbStop)
		hbAfter, hbTick := s.heartbeatTiming()
		go heartbeatStream(ctx, ctl, emit, hbStop, hbAfter, hbTick)

		// The registry could not be read when the model id was resolved:
		// the generic setup error, never a fallback to another model.
		if azureErr != nil {
			fail(ctl.classifyFailureShared(ctx, azureErr))
			return
		}
		// C-3: provider resolution, budget checks (with this stream's
		// reservation published first) and the RAG lookup.
		ts, err := s.prepareTurn(ctx, scope, doc, req, azureRes, ctl)
		if err != nil {
			fail(ctl.classifyFailureShared(ctx, err))
			return
		}
		turn = ts.model

		// Server-side history reconstruction: when the client omits Messages
		// (the efficient path — it sends only the new userMessage), load the
		// prior conversation from the store so the LLM gets full context
		// without the client re-sending the whole history each turn. A
		// non-empty Messages slice (legacy client, or a resend that locally
		// truncated history) is used as-is.
		serverOwnsHistory := len(req.Messages) == 0
		if serverOwnsHistory {
			// The answer is stored when the stream finishes, for the same
			// reason the question is stored when it starts: the client saves
			// the conversation itself on done, so a tab that closes (or any
			// headless caller) left the question stored with no answer beside
			// it. Run from the cleanup so every exit path does it.
			persistAnswer = true
		}
		if len(req.Messages) == 0 {
			// Hold the per-conversation mutex across reconstruct + persist so two
			// concurrent streams on the same (flowID, convKey) can't both read
			// the same history and then overwrite each other's user-turn persist
			// (a lost update — SaveConversation replaces the whole file).
			// Only once the question is stored may a reloading client be sent
			// back to this stream (ActiveStreamFor).
			if s.reconstructAndPersistUserTurn(ctx, doc, &req, req.ConversationKey()) {
				ctl.userTurnStored.Store(true)
			}
		}

		aiReq, scrubbedDoc, ok, err := s.buildStreamRequest(ctx, ts, scope, doc, report, req)
		if err != nil {
			msg := "conversation is too long for this model's context window — start a new conversation or remove some history"
			fail(ChatErrContextLimit, msg, false) // a context limit is not the org's to fix
			return
		}
		if !ok {
			return // ctx cancelled while waiting on RAG
		}
		// The one-response reservation on the concurrent-stream ledger was
		// published by prepareTurn, ahead of its budget check; the loops below
		// refresh it per iteration with the actual spend so far plus one more
		// response.

		// When the caller opted into tools, run the agentic loop. Native
		// function-calling providers use runToolLoop; providers without it
		// (notably GitHub Copilot) fall back to runPromptToolLoop, which
		// teaches the model a <tool_call> marker format via the system prompt.
		// Both are self-contained (emit chunk/tool/done/error + update ctl),
		// so the normal streaming path below is skipped entirely.
		//
		// The ToolContext carries the scrubbed doc + the approval-gated fix
		// applier (bound to this stream's owner + emit), so apply_fix can
		// prompt the user through the same SSE channel. RealDoc is the
		// unscrubbed flow for analyses/fix resolution (A2), and analyses run
		// read-only so the tool loop never overwrites the UI's CurrentReport.
		// tctx is passed by POINTER: an approved apply swaps in the fresh
		// post-fix document mid-stream (A1) so follow-up tool calls in the
		// SAME stream see the fixed flow — the re-parse mints new block IDs,
		// and the pre-fix snapshot would mis-answer every later tool.
		// A Foundry agent (a registry agent entry, or the raw id
		// "agent:<name>") always runs through the tool loop regardless of the
		// per-thread toggle: the agent's own definition decides whether it
		// calls baki's flow tools, and a function call it does make must be
		// answered or the run stalls. Decided on the resolved model — the
		// request's "m/<entryId>" says nothing about the kind.
		if req.UseTools || ts.model.agent() {
			tctx := ai.ToolContext{
				Ctx:      ctx,
				Doc:      scrubbedDoc,
				RealDoc:  doc,
				Analysis: readOnlyAnalysis{s.analysisCache},
			}
			if ap := s.newFixApplier(scope, doc, streamID, emit, ctl.touch, func(fresh *models.FlowDocument) {
				tctx.RealDoc = fresh
				if sd, sErr := scrubber.ScrubDocument(fresh); sErr == nil {
					tctx.Doc = sd
				} else {
					logger.Error("Failed to scrub post-fix document; tools see the real doc", "error", sErr)
					tctx.Doc = fresh
				}
			}); ap != nil {
				tctx.Fixes = ap
			}
			if ts.provider.SupportsTools() {
				s.runToolLoop(ctx, ts.provider, aiReq, scope, &tctx, ctl, awaitStart, emit, coalesce.flushAndCount)
			} else {
				s.runPromptToolLoop(ctx, ts.provider, aiReq, scope, &tctx, ctl, awaitStart, emit, coalesce.flushAndCount)
			}
			return
		}

		// out masks secrets in the model's output (even across chunk splits),
		// maintains the resume buffer, and counts emitted chunk events; the done
		// event carries the count so the client can detect chunks dropped by a
		// saturated SSE buffer and recover the authoritative text via resume.
		out := newScrubbedEmitter(ctl, emit)
		// sawText: whether any answer text arrived (split-off reasoning and
		// keepalives are not answer text) — an empty answer cut at the token
		// cap is a failure, not a reply.
		sawText := false
		// lastServed: the model that answered, from any chunk that names it
		// (alias providers put it on content chunks and the terminal one).
		lastServed := ""
		ctl.beginProviderTurn()
		// failUsage is fail for the one failure whose billing is in hand (the
		// output-limit turn saw its Done usage): the error event carries the
		// same usage keys a done event would (usageEventFields) and the stored
		// failure keeps them, so the meter, ResumeStream and BeginStream
		// report a billed failed turn instead of 0/0 (F-15).
		failUsage := func(code, msg string, shared bool, tokensIn, tokensOut int, served string) {
			msg = scrubber.ScrubText(msg)
			ctl.setFailureUsage(code, msg, shared, tokensIn, tokensOut, served)
			data := errorEventData(code, msg, shared)
			for k, v := range usageEventFields(ctx, ts.provider, aiReq.Model, served, tokensIn, tokensOut) {
				data[k] = v
			}
			emit("error", data)
		}
		err = ts.provider.Stream(ctx, aiReq, func(chunk ai.Chunk) {
			ctl.touch()
			// Before the begin gate: the watchdog runs from stream start.
			ctl.sawOutput(chunk.Text != "", len(chunk.ToolCalls) > 0, chunk.Done)
			// A silent non-hosted phase (a Responses reasoning item with
			// summaries off — ai.Chunk.SilentStep) holds the hosted-step idle
			// limit while it runs, like a hosted tool step (F-08).
			if chunk.SilentStep != 0 {
				ctl.hostedStep(chunk.SilentStep > 0)
			}
			if chunk.ToolEvent != nil {
				ctl.hostedStep(!chunk.ToolEvent.Done)
			}
			if chunk.ServedModel != "" {
				lastServed = chunk.ServedModel
			}
			if !awaitStart() {
				return
			}
			if chunk.Error != nil {
				out.flush()
				// Same friendly mapping as a returned error: an in-stream
				// provider failure (Claude "overloaded", a content filter)
				// used to reach the user as the raw provider string.
				code, msg, shared := ctl.classifyFailureShared(ctx, chunk.Error)
				msg = scrubber.ScrubText(msg)
				ctl.setFailure(code, msg, shared)
				emit("error", errorEventData(code, msg, shared))
				return
			}
			if chunk.ToolEvent != nil {
				out.flush()
				emitToolEvent(emit, chunk.ToolEvent)
				return
			}
			if chunk.Notice != nil {
				// A provider status line ("reasoning off"): a trail row like
				// tools_unsupported, not a hosted step — the idle limit stays
				// the normal one. Once per turn (a retry repeats it).
				if ctl.firstNotice(chunk.Notice.Name) {
					out.flush()
					emit("tool", noticeEventData(chunk.Notice))
				}
				return
			}
			if chunk.Done {
				out.flush()
				if emptyOutputLimitTurn(sawText, len(chunk.ToolCalls), chunk.FinishReason) {
					// The provider still reported (and the audited wrapper
					// recorded) the usage; only the reply is missing.
					failUsage(ChatErrOutputLimit, outputLimitMsg, false, chunk.TokensIn, chunk.TokensOut, lastServed)
					return
				}
				ctl.markDone(chunk.TokensIn, chunk.TokensOut, lastServed)
				emit("done", doneEventData(ctx, ts.provider, aiReq.Model, lastServed, chunk.TokensIn, chunk.TokensOut, coalesce.flushAndCount()))
				return
			}

			// A ServerStep marker (hosted step without a trail entry), a
			// keepalive or split-off reasoning carries no answer text: nothing
			// to emit, the touch above is all it is for.
			if chunk.Text != "" {
				sawText = true
				out.text(chunk.Text)
			}
		})
		ctl.endProviderTurn()

		if err != nil {
			code, msg, shared := ctl.classifyFailureShared(ctx, err)
			msg = scrubber.ScrubText(msg)
			ctl.setFailure(code, msg, shared)
			if !awaitStart() {
				return
			}
			// Deliver whatever the scrubber was still holding before reporting
			// the failure, so the partial response isn't truncated client-side.
			out.flush()
			emit("error", errorEventData(code, msg, shared))
		}
	}()

	return streamID, nil
}

// runToolLoop drives the tool/agent loop: it offers the registered tools to
// the model, executes any tool calls against the (already scrubbed) doc +
// analysis cache, feeds results back, and repeats until the model returns a
// final text answer (or the iteration cap is hit). It is self-contained —
// emits "tool"/"chunk"/"done"/"error" events and updates ctl — so the caller
// does not run its own streaming/error handling for this path.
//
// scope is the caller's identity (budget re-checks per iteration); tctx is the
// prebuilt tool context (scrubbed doc + optional fix applier).
//
// Token consumption of ctl.started is gated through a single cached
// ensureStarted so it is consumed at most once (the channel only holds one
// token), regardless of how many turns/events the loop produces.
func (s *ChatService) runToolLoop(
	ctx context.Context,
	provider ai.Provider,
	base ai.Request,
	scope string,
	tctx *ai.ToolContext,
	ctl *streamCtl,
	awaitStart func() bool,
	emit func(string, map[string]interface{}),
	doneChunks func() int,
) {
	base.Tools = ai.ToolDefinitions()
	msgs := base.Messages
	// Model-aware window (computed once — the model doesn't change across
	// iterations): the catalog's per-model limit overrides the smaller
	// provider-wide default, so accumulating tool results aren't truncated
	// against a window the model doesn't actually have.
	ctxLimit := ai.ContextLimitFor(ctx, provider, base.Model)

	// replayItems: each assistant turn that issued tool calls carries the
	// provider's own record of it (ai.Message.ProviderItems — Claude's
	// thinking blocks with their signatures, a Responses run's encrypted
	// reasoning), which the provider replays verbatim on the next request so
	// the model keeps its reasoning across the tool step. A record is only
	// valid while the history stays APPEND-ONLY and the request's system
	// prompt, tools and provider do not change: Claude rejects (or drops) a
	// thinking block whose earlier history was edited. So the first rewrite —
	// truncation removing a message, the tools-unsupported fallback dropping
	// the tools, a record from another provider — strips every record from
	// the whole history and keeps them off for the rest of the loop (a record
	// put back after its removal is just as invalid). Records live only in
	// msgs: never persisted, never sent to the client.
	replayItems := true
	stopReplay := func() {
		if replayItems {
			replayItems = false
			msgs = withoutProviderItems(msgs)
		}
	}
	if hasForeignProviderItems(msgs, provider.ID()) {
		stopReplay()
	}

	started := false
	ensureStarted := func() bool {
		if !started && awaitStart() {
			started = true
		}
		return started
	}
	fail := func(code, msg string, shared bool) {
		msg = scrubber.ScrubText(msg) // provider strings can echo request content
		ctl.setFailure(code, msg, shared)
		if ensureStarted() {
			emit("error", errorEventData(code, msg, shared))
		}
	}
	// failUsage is fail for the loop's output-limit failures, whose token
	// totals are in hand: the error event carries the usage keys a done event
	// would (usageEventFields) and the stored failure keeps them, so the meter
	// and resume snapshots report a billed failed turn instead of 0/0 (F-15).
	failUsage := func(code, msg string, shared bool, tokensIn, tokensOut int, served string) {
		msg = scrubber.ScrubText(msg)
		ctl.setFailureUsage(code, msg, shared, tokensIn, tokensOut, served)
		if ensureStarted() {
			data := errorEventData(code, msg, shared)
			for k, v := range usageEventFields(ctx, provider, base.Model, served, tokensIn, tokensOut) {
				data[k] = v
			}
			emit("error", data)
		}
	}

	var totalIn, totalOut int
	// lastServed is the model that answered the most recent turn (alias
	// providers), carried on the done payload.
	lastServed := ""
	// out masks secrets in streamed turn text and counts emitted chunk events
	// across all turns; the count rides the done event so the client can detect
	// dropped SSE chunks (see StreamChatMessage). One scrubber spans the loop —
	// each turn's text is flushed before its tool events, so held text can't
	// bleed between turns.
	out := newScrubbedEmitter(ctl, emit)
	mem := newLoopToolMemory()
	price := inFlightPricing(ctx, provider, base.Model)
	reserve := inFlightCost(price, 0, maxOutputTokens(base))
	// Blended per-turn rates (F-30): a model-router deployment may answer each
	// iteration with a different served model, so the done/resume/persisted
	// rates accumulate each iteration's price × its tokens instead of pricing
	// the whole turn at the LAST served model's rate. unpriced keeps the plain
	// doneEventData payload for providers or models the pricer cannot price
	// (behaviour unchanged for them).
	var costIn, costOut float64
	unpriced := false
	pricer, canPrice := provider.(ai.UsagePricer)
	var lastPrice ai.Pricing
	// pin is the current turn's index in msgs (the last message the caller
	// built); truncation never drops it and reports where it moved to.
	pin := len(msgs) - 1
	for i := 0; i < maxToolIterations; i++ {
		metrics.RecordChatToolIteration()
		// Re-check the daily AI budget every iteration: a tool loop multiplies
		// the turn's spend up to maxToolIterations×, and the turn-start check
		// alone would let one approved request run past the cap mid-loop. The
		// in-flight term covers this stream's async-unwritten spend (see
		// inFlightCost).
		own := inFlightCost(price, totalIn, totalOut)
		ctl.setInflight(own + reserve)
		if err := s.enforceTurnBudget(ctx, provider, scope, base.OrgID, own, ctl); err != nil {
			code, msg, report := loopBudgetFailure(ctx, ctl, err)
			if report {
				// A mid-loop budget stop is the kind of error operators need
				// aggregated: the turn already burned budget before being cut off.
				errreport.CaptureError(ctx, "chat.tool_loop.budget", err, errreport.Attrs{"scope": scope, "provider": provider.ID()})
			}
			fail(code, msg, false) // a budget stop is never a shared-credential failure
			return
		}
		turn := base
		turn.Messages = msgs

		// Re-enforce the context window each turn: tool results + assistant
		// text accumulate and can overflow the model's context window by
		// iteration 4-6, surfacing as a raw provider 400. Truncating oldest
		// history (then the oldest tool exchange) here keeps the loop within
		// bounds; the current turn itself is never dropped.
		var err error
		if pin, err = truncateForContextWindow(ctx, provider, &turn, ctxLimit, pin); err != nil {
			fail(ChatErrContextLimit, "conversation is too long for this model's context window — too many tool results, try a simpler question", false)
			return
		}
		rewritten := !sameMessages(msgs, turn.Messages)
		msgs = turn.Messages
		if rewritten {
			// Earlier messages were dropped: the history is no longer the
			// one the recorded turns were produced after.
			stopReplay()
			turn.Messages = msgs
		}

		// Stream the turn: forward assistant text to the client as it arrives, and
		// capture the tool calls / token usage from the terminal (Done) chunk.
		var (
			turnText   strings.Builder
			turnCalls  []ai.ToolCall
			turnItems  json.RawMessage
			turnIn     int
			turnOut    int
			turnFinish string
			// turnServed is the model that answered THIS iteration (alias
			// providers), the unit the blended rates price (F-30).
			turnServed string
			chunkErr   error
		)
		ctl.beginProviderTurn()
		streamErr := provider.Stream(ctx, turn, func(c ai.Chunk) {
			ctl.touch()
			ctl.sawOutput(c.Text != "", len(c.ToolCalls) > 0, c.Done)
			// A silent non-hosted phase (a Responses reasoning item with
			// summaries off) holds the hosted-step idle limit while it runs
			// (F-08), before the begin gate like the hosted-step accounting.
			if c.SilentStep != 0 {
				ctl.hostedStep(c.SilentStep > 0)
			}
			if c.ServedModel != "" {
				lastServed = c.ServedModel
				turnServed = c.ServedModel
			}
			switch {
			case c.Error != nil:
				chunkErr = c.Error
			case c.ToolEvent != nil:
				// A tool the provider ran server-side (a Foundry agent's web
				// search, MCP call, …): account it for the idle watchdog
				// (before the begin gate — the watchdog runs from stream
				// start), then forward it to the trail in order.
				ctl.hostedStep(!c.ToolEvent.Done)
				if !ensureStarted() {
					return
				}
				out.flush()
				emitToolEvent(emit, c.ToolEvent)
			case c.Notice != nil:
				// A provider status line: the trail row the tools_unsupported
				// fallback emits, with no hosted-step accounting — once per
				// turn, not once per iteration or retried call.
				if !ensureStarted() || !ctl.firstNotice(c.Notice.Name) {
					return
				}
				out.flush()
				emit("tool", noticeEventData(c.Notice))
			case c.Done:
				turnIn = c.TokensIn
				turnOut = c.TokensOut
				turnCalls = c.ToolCalls
				turnFinish = c.FinishReason
				turnItems = c.ProviderItems
			case c.Text != "":
				if !ensureStarted() {
					return
				}
				// turnText keeps the raw text: it only feeds the next model turn
				// (server-side); everything client-bound goes through the scrubber.
				turnText.WriteString(c.Text)
				out.text(c.Text)
			}
			// Anything else (keepalive, split-off reasoning, ServerStep
			// marker) only feeds the idle watchdog through the touch above.
		})
		ctl.endProviderTurn()
		if started {
			// Release scrubber-held text before any error/tool/done event so the
			// turn's tail isn't withheld or attributed to the next turn.
			out.flush()
		}
		if streamErr != nil {
			// A model that can't use tools (provider family supports them,
			// this model doesn't — common on GitHub Models) rejects the
			// request outright. Degrade gracefully: retry the SAME iteration
			// once with tools dropped and a status notice, instead of failing
			// the whole turn with an opaque 400.
			if errors.Is(streamErr, ai.ErrToolsUnsupported) && len(base.Tools) > 0 {
				base.Tools = nil
				stopReplay() // the tools changed under the recorded turns
				if ensureStarted() {
					emit("tool", map[string]interface{}{"name": "tools_unsupported", "label": "This model can't use tools — answering without them"})
				}
				i-- // don't consume an iteration for the degradation retry
				continue
			}
			fail(ctl.classifyFailureShared(ctx, streamErr))
			return
		}
		if chunkErr != nil {
			// Same graceful degradation when the rejection arrives as an
			// in-stream error chunk (some providers stream the 400 body).
			if errors.Is(chunkErr, ai.ErrToolsUnsupported) && len(base.Tools) > 0 {
				base.Tools = nil
				stopReplay()
				if ensureStarted() {
					emit("tool", map[string]interface{}{"name": "tools_unsupported", "label": "This model can't use tools — answering without them"})
				}
				i--
				continue
			}
			fail(ctl.classifyFailureShared(ctx, chunkErr))
			return
		}
		totalIn += turnIn
		totalOut += turnOut
		if canPrice {
			if p, ok := pricer.UsagePricing(ctx, base.Model, turnServed); ok {
				lastPrice = p
				costIn += float64(turnIn) * p.InputCostPerM
				costOut += float64(turnOut) * p.OutputCostPerM
			} else {
				unpriced = true
			}
		} else {
			unpriced = true
		}

		// No tool calls → this turn is the final answer (its text already
		// streamed above, so there's nothing more to emit but the done event).
		if len(turnCalls) == 0 {
			if emptyOutputLimitTurn(turnText.Len() > 0, 0, turnFinish) {
				// Cut at the token cap before any answer (the usage is
				// already recorded by the audited wrapper).
				failUsage(ChatErrOutputLimit, outputLimitMsg, false, totalIn, totalOut, lastServed)
				return
			}
			if !ensureStarted() {
				return
			}
			ctl.markDone(totalIn, totalOut, lastServed)
			price, priced := blendedRates(lastPrice, costIn, totalIn, costOut, totalOut, unpriced)
			emit("done", doneEventDataRated(ctx, provider, base.Model, lastServed, totalIn, totalOut, doneChunks(), price, priced))
			return
		}

		if truncatedToolTurn(turnCalls, turnFinish) {
			// Cut off at the output limit mid-call: running a call with
			// incomplete arguments (a partial apply_fixes target list, a
			// truncated query) would act on something the model never finished
			// asking for. Fail like an exhausted budget; the audited wrapper
			// recorded the usage. (A NON-limit finish with malformed argument
			// JSON is not this case — ExecuteTool answers those calls with a
			// retryable error result so the model can re-issue them.)
			failUsage(ChatErrOutputLimit, outputLimitMsg, false, totalIn, totalOut, lastServed)
			return
		}

		// Record the assistant turn that issued the calls (including any streamed
		// preamble text and, while replay is on, the provider's own record of
		// the turn), then run each tool and append its result so the next turn
		// can see them.
		//
		// A non-limit turn may carry malformed argument JSON (open models emit
		// it with finish tool_calls; Microsoft: "the JSON response might not
		// always be valid"). ExecuteTool answers those calls with a retryable
		// error result, and the RECORDED copy must marshal on the next request
		// (claude.go json.Marshals arguments; responses.go forwards them), so
		// its invalid input becomes an empty object — the model sees its own
		// call echoed with {} beside the tool result naming the problem.
		recordCalls := turnCalls
		if slices.ContainsFunc(turnCalls, invalidToolInput) {
			recordCalls = slices.Clone(turnCalls)
			for j := range recordCalls {
				if invalidToolInput(recordCalls[j]) {
					recordCalls[j].Input = json.RawMessage("{}")
				}
			}
		}
		assistant := ai.Message{Role: "assistant", Content: turnText.String(), ToolCalls: recordCalls}
		if replayItems && len(turnItems) > 0 {
			assistant.ProviderItems = turnItems
			assistant.ProviderItemsFrom = provider.ID()
		}
		msgs = append(msgs, assistant)
		for _, r := range s.execTurnTools(emit, ctl, ensureStarted, mem, tctx, turnCalls) {
			msgs = append(msgs, ai.Message{Role: "tool", Content: r.result, ToolCallID: r.id})
		}
	}

	fail(ChatErrToolLoopLimit, fmt.Sprintf("tool loop exceeded %d iterations without a final answer", maxToolIterations), false)
}

// withoutProviderItems is msgs with every provider replay record removed
// (ai.Message.ProviderItems / ProviderItemsFrom). A fresh slice when any
// message carried one — msgs may share its backing array with the caller's
// request — else msgs itself.
func withoutProviderItems(msgs []ai.Message) []ai.Message {
	if !slices.ContainsFunc(msgs, func(m ai.Message) bool { return len(m.ProviderItems) > 0 || m.ProviderItemsFrom != "" }) {
		return msgs
	}
	out := make([]ai.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].ProviderItems = nil
		out[i].ProviderItemsFrom = ""
	}
	return out
}

// hasForeignProviderItems reports a replay record written by a provider
// other than providerID: the provider changed under the recorded turns.
func hasForeignProviderItems(msgs []ai.Message, providerID string) bool {
	return slices.ContainsFunc(msgs, func(m ai.Message) bool {
		return len(m.ProviderItems) > 0 && m.ProviderItemsFrom != providerID
	})
}

// sameMessages reports whether after is before unchanged — the same slice
// (truncateForContextWindow returns the request's own slice when nothing had
// to go, and a new one when it dropped messages).
func sameMessages(before, after []ai.Message) bool {
	if len(before) != len(after) {
		return false
	}
	return len(before) == 0 || &before[0] == &after[0]
}

// runPromptToolLoop is the prompt-based fallback for providers that don't
// support native function-calling (e.g. GitHub Copilot): the tool schemas are
// described in the system prompt and the model emits <tool_call> blocks in its
// text to invoke a tool. This mirrors runToolLoop's structure but parses tool
// calls from the streamed text instead of from native ToolCall fields.
//
// Because the markers arrive inline, the turn text is buffered whole (not
// streamed incrementally) so the raw tool JSON can be stripped before the client
// sees it. Copilot tool turns are therefore non-streaming; the final answer turn
// still emits normally once no tool_call is present.
func (s *ChatService) runPromptToolLoop(
	ctx context.Context,
	provider ai.Provider,
	base ai.Request,
	scope string,
	tctx *ai.ToolContext,
	ctl *streamCtl,
	awaitStart func() bool,
	emit func(string, map[string]interface{}),
	doneChunks func() int,
) {
	msgs := base.Messages
	// Teach the model the marker protocol by APPENDING to the system prompt —
	// NOT by injecting an extra system message. The context-window truncation
	// pins Messages[:1] as the user's question; a synthetic leading system
	// message used to take that pin, so under overflow the junction repair
	// dropped the USER'S TURN (context + question) while tool-instruction
	// boilerplate stayed pinned. SystemPrompt is counted by the truncation
	// estimate either way.
	toolInstr := ai.ToolPromptInstructions()
	if base.SystemPrompt != "" {
		turn := base
		turn.SystemPrompt = base.SystemPrompt + "\n\n" + toolInstr
		base = turn
	} else {
		base.SystemPrompt = toolInstr
	}
	ctxLimit := ai.ContextLimitFor(ctx, provider, base.Model)

	started := false
	ensureStarted := func() bool {
		if !started && awaitStart() {
			started = true
		}
		return started
	}
	fail := func(code, msg string, shared bool) {
		msg = scrubber.ScrubText(msg) // provider strings can echo request content
		ctl.setFailure(code, msg, shared)
		if ensureStarted() {
			emit("error", errorEventData(code, msg, shared))
		}
	}
	// failUsage is fail for the loop's output-limit failures, whose token
	// totals are in hand: the error event carries the usage keys a done event
	// would (usageEventFields) and the stored failure keeps them, so the meter
	// and resume snapshots report a billed failed turn instead of 0/0 (F-15).
	failUsage := func(code, msg string, shared bool, tokensIn, tokensOut int, served string) {
		msg = scrubber.ScrubText(msg)
		ctl.setFailureUsage(code, msg, shared, tokensIn, tokensOut, served)
		if ensureStarted() {
			data := errorEventData(code, msg, shared)
			for k, v := range usageEventFields(ctx, provider, base.Model, served, tokensIn, tokensOut) {
				data[k] = v
			}
			emit("error", data)
		}
	}

	var totalIn, totalOut int
	lastServed := ""
	out := newScrubbedEmitter(ctl, emit)
	mem := newLoopToolMemory()
	price := inFlightPricing(ctx, provider, base.Model)
	reserve := inFlightCost(price, 0, maxOutputTokens(base))
	// Blended per-turn rates (F-30): a model-router deployment may answer each
	// iteration with a different served model, so the done/resume/persisted
	// rates accumulate each iteration's price × its tokens instead of pricing
	// the whole turn at the LAST served model's rate. unpriced keeps the plain
	// doneEventData payload for providers or models the pricer cannot price
	// (behaviour unchanged for them).
	var costIn, costOut float64
	unpriced := false
	pricer, canPrice := provider.(ai.UsagePricer)
	var lastPrice ai.Pricing
	pin := len(msgs) - 1 // the current turn (see runToolLoop)
	for i := 0; i < maxToolIterations; i++ {
		metrics.RecordChatToolIteration()
		// Same per-iteration budget re-check as runToolLoop (in-flight term
		// included).
		own := inFlightCost(price, totalIn, totalOut)
		ctl.setInflight(own + reserve)
		if err := s.enforceTurnBudget(ctx, provider, scope, base.OrgID, own, ctl); err != nil {
			code, msg, report := loopBudgetFailure(ctx, ctl, err)
			if report {
				errreport.CaptureError(ctx, "chat.tool_loop.budget", err, errreport.Attrs{"scope": scope, "provider": provider.ID()})
			}
			fail(code, msg, false) // a budget stop is never a shared-credential failure
			return
		}
		turn := base
		turn.Messages = msgs
		var err error
		if pin, err = truncateForContextWindow(ctx, provider, &turn, ctxLimit, pin); err != nil {
			fail(ChatErrContextLimit, "conversation is too long for this model's context window — too many tool results, try a simpler question", false)
			return
		}
		msgs = turn.Messages

		// Buffer the full turn: the <tool_call> markers must be parsed + stripped
		// before anything reaches the client, so we can't forward text as it
		// streams.
		var (
			turnText   strings.Builder
			turnIn     int
			turnOut    int
			turnFinish string
			// turnServed is the model that answered THIS iteration — the unit
			// the blended rates price (F-30).
			turnServed string
			chunkErr   error
		)
		ctl.beginProviderTurn()
		streamErr := provider.Stream(ctx, turn, func(c ai.Chunk) {
			ctl.touch()
			ctl.sawOutput(c.Text != "", len(c.ToolCalls) > 0, c.Done)
			if c.ServedModel != "" {
				lastServed = c.ServedModel
				turnServed = c.ServedModel
			}
			switch {
			case c.Error != nil:
				chunkErr = c.Error
			case c.Notice != nil:
				// A provider status line, once per turn (see runToolLoop). The
				// turn's text is buffered, so nothing of it can be held back
				// by the row.
				if ensureStarted() && ctl.firstNotice(c.Notice.Name) {
					emit("tool", noticeEventData(c.Notice))
				}
			case c.Done:
				turnIn = c.TokensIn
				turnOut = c.TokensOut
				turnFinish = c.FinishReason
			case c.Text != "":
				turnText.WriteString(c.Text)
			}
		})
		if started {
			out.flush()
		}
		if streamErr != nil {
			fail(ctl.classifyFailureShared(ctx, streamErr))
			return
		}
		if chunkErr != nil {
			fail(ctl.classifyFailureShared(ctx, chunkErr))
			return
		}
		totalIn += turnIn
		totalOut += turnOut
		if canPrice {
			if p, ok := pricer.UsagePricing(ctx, base.Model, turnServed); ok {
				lastPrice = p
				costIn += float64(turnIn) * p.InputCostPerM
				costOut += float64(turnOut) * p.OutputCostPerM
			} else {
				unpriced = true
			}
		} else {
			unpriced = true
		}

		full := turnText.String()
		calls := ai.ParsePromptToolCalls(full)
		cleanText := ai.StripPromptToolCalls(full)

		// No tool calls → this turn is the final answer. Emit the cleaned text
		// (preamble the model wrote around the absent marker) and finish.
		if len(calls) == 0 {
			if emptyOutputLimitTurn(full != "", 0, turnFinish) {
				failUsage(ChatErrOutputLimit, outputLimitMsg, false, totalIn, totalOut, lastServed)
				return
			}
			if !ensureStarted() {
				return
			}
			if cleanText != "" {
				out.text(cleanText)
				out.flush()
			}
			ctl.markDone(totalIn, totalOut, lastServed)
			price, priced := blendedRates(lastPrice, costIn, totalIn, costOut, totalOut, unpriced)
			emit("done", doneEventDataRated(ctx, provider, base.Model, lastServed, totalIn, totalOut, doneChunks(), price, priced))
			return
		}

		// Emit the non-marker preamble (e.g. "Let me search for…"), then run each
		// tool and append its result for the next turn.
		if ensureStarted() && cleanText != "" {
			out.text(cleanText)
			out.flush()
		}
		msgs = append(msgs, ai.Message{Role: "assistant", Content: full})
		// Feed results back as a USER message with <tool_result> markers. A
		// bare role:"tool" message is schema-invalid on OpenAI-compatible
		// wires (tool messages must answer an assistant tool_calls turn this
		// protocol never emits) — Copilot rejected the second request of every
		// tool conversation with a 400. One user message per turn also keeps
		// consecutive-user-role sequences impossible.
		var results strings.Builder
		for _, tc := range calls {
			if !ensureStarted() {
				return
			}
			emit("tool", map[string]interface{}{"name": tc.Name, "label": ai.ToolLabel(tc.Name)})
			// See runToolLoop: keep the idle watchdog aware of in-flight tool
			// execution (no provider chunks flow while a tool runs).
			ctl.touch()
			started := time.Now()
			result := mem.exec(tc.Name, tc.Input, tctx)
			ctl.touch()
			emitToolResult(emit, tc.Name, result, time.Since(started))
			fmt.Fprintf(&results, "<tool_result name=%q>\n%s\n</tool_result>\n", tc.Name, result)
		}
		msgs = append(msgs, ai.Message{Role: "user", Content: results.String()})
	}

	fail(ChatErrToolLoopLimit, fmt.Sprintf("tool loop exceeded %d iterations without a final answer", maxToolIterations), false)
}

// toolSummary derives the one-line transparency summary for a tool_result
// event from the result string the model saw: first line, capped, so a
// multi-KB doc dump doesn't flood the event. Results derive from the
// scrubbed document (ToolContext carries scrubbed docs), so the summary
// inherits that scrubbing.
func toolSummary(result string) string {
	line := result
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len(line) > 120 {
		// Back off to a rune boundary (same rule as ai.truncateResult) so a
		// truncated multibyte char doesn't reach the client as invalid UTF-8.
		cut := 120
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	return line
}

// usageEventFields is the usage half of a done or error event payload: token
// counts, the model that answered (alias providers — omitted when the provider
// named none) and the per-million rates the turn was billed at, so the client
// shows model and cost without a listing refetch. The rate comes from the
// audited wrapper (ai.UsagePricer: the served model's catalogue price, else
// the provider rate; Azure AI Foundry is unpriced, so its turns carry zero
// rates); a bare provider (tests) carries
// no rate. Factored out of doneEventData so a billed FAILED turn (the
// output-limit failures, F-15) reaches the meter with the same keys a done
// event carries.
func usageEventFields(ctx context.Context, p ai.Provider, model, served string, tokensIn, tokensOut int) map[string]interface{} {
	data := map[string]interface{}{"tokensIn": tokensIn, "tokensOut": tokensOut}
	if served != "" {
		data["servedModel"] = served
	}
	if pr, ok := p.(ai.UsagePricer); ok {
		if price, ok := pr.UsagePricing(ctx, model, served); ok {
			data["inputCostPerM"] = price.InputCostPerM
			data["outputCostPerM"] = price.OutputCostPerM
		}
	}
	return data
}

// doneEventData is the done payload: usageEventFields plus the chunk count the
// client uses to detect SSE gaps.
func doneEventData(ctx context.Context, p ai.Provider, model, served string, tokensIn, tokensOut, chunks int) map[string]interface{} {
	data := usageEventFields(ctx, p, model, served, tokensIn, tokensOut)
	data["chunks"] = chunks
	return data
}

// doneEventDataRated is doneEventData with explicit rates: the tool loops
// blend the per-iteration prices of a turn whose served model varied (a
// model-router deployment, F-30) instead of pricing the whole token sum at
// the LAST served model's rate. priced=false keeps the plain lookup payload
// (doneEventData) for bare providers and models the pricer cannot price.
func doneEventDataRated(ctx context.Context, p ai.Provider, model, served string, tokensIn, tokensOut, chunks int, price ai.Pricing, priced bool) map[string]interface{} {
	if !priced {
		return doneEventData(ctx, p, model, served, tokensIn, tokensOut, chunks)
	}
	data := map[string]interface{}{"tokensIn": tokensIn, "tokensOut": tokensOut, "chunks": chunks}
	if served != "" {
		data["servedModel"] = served
	}
	data["inputCostPerM"] = price.InputCostPerM
	data["outputCostPerM"] = price.OutputCostPerM
	return data
}

// blendedRates derives the effective per-million rates of a multi-iteration
// tool turn from the accumulated per-iteration costs (F-30): the sum of what
// each iteration's served model charged divided by the total tokens. A zero
// token count keeps the last iteration's rate for that side (a 0 would read
// as "not reported" although the turn was priced). unpriced reports
// priced=false so the caller falls back to the plain payload.
func blendedRates(last ai.Pricing, costIn float64, tokensIn int, costOut float64, tokensOut int, unpriced bool) (ai.Pricing, bool) {
	if unpriced {
		return ai.Pricing{}, false
	}
	p := last
	if tokensIn > 0 {
		p.InputCostPerM = costIn / float64(tokensIn)
	}
	if tokensOut > 0 {
		p.OutputCostPerM = costOut / float64(tokensOut)
	}
	return p, true
}

// invalidToolInput reports a tool call whose arguments are non-empty but not
// valid JSON (F-11): the provider cut the argument stream or the model
// emitted malformed JSON with a non-limit finish. Empty input is the
// providers' "no arguments".
func invalidToolInput(tc ai.ToolCall) bool {
	in := bytes.TrimSpace(tc.Input)
	return len(in) > 0 && !json.Valid(in)
}

// emitToolEvent forwards a tool step the PROVIDER ran server-side (a Foundry
// agent's hosted web search, file search, MCP or code-interpreter call) as
// the same "tool" / "tool_result" events the local tool loop emits, so the
// client's trail shows hosted work without a new event type.
func emitToolEvent(emit func(string, map[string]interface{}), ev *ai.ToolEvent) {
	if ev == nil {
		return
	}
	if !ev.Done {
		emit("tool", map[string]interface{}{"name": ev.Name, "label": ev.Label})
		return
	}
	emit("tool_result", map[string]interface{}{
		"name":       ev.Name,
		"label":      ev.Label,
		"ok":         ev.OK,
		"durationMs": ev.DurationMs,
		"summary":    ev.Summary,
	})
}

// noticeEventData is the "tool" event payload of a provider status line
// (ai.Chunk.Notice, e.g. "reasoning_off"): a start-only trail row shaped like
// the tools_unsupported one. It is not a hosted step — nothing ends it, and
// the idle watchdog keeps the normal limit.
func noticeEventData(n *ai.Notice) map[string]interface{} {
	return map[string]interface{}{"name": n.Name, "label": n.Label}
}

// emitToolResult reports a finished tool execution on the stream so the client
// can accumulate the per-message tool trail. ok follows ExecuteTool's error
// contract ("error: …" prefix); the duration covers execution only.
func emitToolResult(emit func(string, map[string]interface{}), name, result string, took time.Duration) {
	emit("tool_result", map[string]interface{}{
		"name":       name,
		"label":      ai.ToolLabel(name),
		"ok":         !strings.HasPrefix(result, "error:"),
		"durationMs": took.Milliseconds(),
		"summary":    toolSummary(result),
	})
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
