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
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/eventbus"
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

// maxChatStreamDuration is the hard wall-clock backstop on a stream; the watchdog/idle timeout normally catches hangs first.
const maxChatStreamDuration = 10 * time.Minute

// maxConcurrentStreamsPerScope caps streams per caller, bounding one user's spend and goroutines.
const maxConcurrentStreamsPerScope = 3

// maxToolIterations bounds the read-only tool loop against a model that never stops requesting tools.
const maxToolIterations = 6

// inFlightCost prices not-yet-persisted spend; double-counting is the safe direction.
func inFlightCost(price ai.Pricing, tokensIn, tokensOut int) float64 {
	// Negative counts must not lower the ledger entry other streams read.
	tokensIn, tokensOut = max(tokensIn, 0), max(tokensOut, 0)
	if tokensIn == 0 && tokensOut == 0 {
		return 0
	}
	return float64(tokensIn)/1e6*price.InputCostPerM + float64(tokensOut)/1e6*price.OutputCostPerM
}

// inFlightPricing is the in-flight rate; alias providers (Foundry) are unpriced and reserve nothing.
func inFlightPricing(_ context.Context, p ai.Provider, _ string) ai.Pricing {
	if p == nil || ai.UsesModelAliases(p.ID()) {
		return ai.Pricing{}
	}
	return p.PricePerMillionTokens()
}

// self is the calling stream (nil before registration); its own ledger entry is excluded.
func (s *ChatService) enforceBudget(ctx context.Context, scope, orgID string, inflight float64, self *streamCtl) error {
	return s.enforceBudgetMemo(ctx, nil, scope, orgID, inflight, self)
}

// enforceBudgetMemo is enforceBudget with a per-loop usage memo; the in-flight ledger covers its staleness gap.
func (s *ChatService) enforceBudgetMemo(ctx context.Context, memo *turnBudgetUsage, scope, orgID string, inflight float64, self *streamCtl) error {
	var spend inFlightSpend
	if self != nil {
		spend = self
	}
	return s.aiBudget().check(ctx, memo, scope, orgID, inflight, spend)
}

// aiBudget is the budget gate: the one DI shares with the knowledge base, else (a bare struct in tests) one over
// the service's own settings and store.
func (s *ChatService) aiBudget() *AIBudget {
	s.budgetOnce.Do(func() {
		if s.budget == nil {
			s.budget = NewAIBudget(s.settings, s.backend)
		}
	})
	return s.budget
}

// enforceTurnBudget: an alias (Foundry) turn is never budget-refused — baki does not price Azure usage.
func (s *ChatService) enforceTurnBudget(ctx context.Context, provider ai.Provider, scope, orgID string, inflight float64, self *streamCtl) error {
	if provider != nil && ai.UsesModelAliases(provider.ID()) {
		return nil
	}
	return s.enforceBudget(ctx, scope, orgID, inflight, self)
}

func (s *ChatService) enforceTurnBudgetMemo(ctx context.Context, provider ai.Provider, memo *turnBudgetUsage, scope, orgID string, inflight float64, self *streamCtl) error {
	if provider != nil && ai.UsesModelAliases(provider.ID()) {
		return nil
	}
	return s.enforceBudgetMemo(ctx, memo, scope, orgID, inflight, self)
}

// maxOutputTokens is the reservation basis: the request's cap or the 4096 default.
func maxOutputTokens(req ai.Request) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return 4096
}

// modelOutputInfo reads model's listing entry: output ceiling (0 = unknown) and the served model behind an alias.
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

// reasoningFloorTokens raises a max-tokens budget below the reasoning floor, capped at maxOutput.
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

// turnGenerationParams sizes the turn's params by the resolved provider-level id: clamps, then the reasoning floor.
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

// errStreamIDInUse is the stream-ID collision sentinel; match with errors.Is.
var errStreamIDInUse = errors.New("clientStreamId already in use")

// ErrTooManyChatStreams is returned when the caller's concurrent-stream cap is exceeded; exported so the HTTP layer maps it to a distinct code.
var ErrTooManyChatStreams = errors.New("too many chat responses running at once")

// convMutexStripes: per-conversation locks are striped for bounded memory, not exact-keyed.
const convMutexStripes = 256

func (s *ChatService) convMutexFor(flowID, convKey string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(flowID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(convKey))
	return &s.convMutexes[h.Sum32()%convMutexStripes]
}

// reconstructAndPersistUserTurn holds the per-conversation mutex across both steps (SaveConversation is a full-file replace).
func (s *ChatService) reconstructAndPersistUserTurn(ctx context.Context, doc *models.FlowDocument, req *models.ChatRequest, convKey string) bool {
	mu := s.convMutexFor(doc.ID, convKey)
	mu.Lock()
	defer mu.Unlock()
	history, err := s.reconstructHistory(ctx, doc, *req)
	if err != nil {
		// Do NOT persist: SaveConversation is a full-file replace — a history we failed to read would be destroyed.
		logger.Warn("chat: history load failed; answering without prior context and "+
			"skipping persistence so the stored conversation is not overwritten", "error", err)
		req.Messages = nil
		return false
	}
	req.Messages = history
	return s.persistInitialUserTurn(ctx, doc, *req)
}

// tryStartStream checks the per-caller cap and reserves the stream ID atomically.
func (s *ChatService) tryStartStream(streamID, scope string, ctl *streamCtl) (bool, error) {
	s.streamCapMu.Lock()
	defer s.streamCapMu.Unlock()
	if _, dup := s.finishedStreams.Load(streamID); dup {
		return false, errStreamIDInUse
	}
	if _, loaded := s.activeStreams.LoadOrStore(streamID, ctl); loaded {
		return false, errStreamIDInUse
	}
	active := 0
	s.activeStreams.Range(func(_, v interface{}) bool {
		if v.(*streamCtl).ownerID == scope {
			active++
		}
		return true // count every entry: an early-out under-counts the just-stored one
	})
	// Undo and reject as cap-exceeded (not collision) so the caller reports the right reason.
	if active > maxConcurrentStreamsPerScope {
		s.activeStreams.Delete(streamID)
		return false, nil
	}
	// The budget's in-flight ledger holds exactly the active streams (released where activeStreams drops it).
	s.aiBudget().hold(ctl)
	return true, nil
}

// chatStore is the narrow storage slice ChatService uses: conversations, usage, the caller's own settings and
// their role in an organisation.
type chatStore interface {
	storageif.ConversationStore
	storageif.UsageStore
	LoadUserSettings(ctx context.Context, userID string) (*storageif.AppSettings, error)
	OrgMemberRole(ctx context.Context, orgID, userID string) (role auth.Role, member bool, err error)
}

// readOnlyAnalysis adapts AnalysisService: tool-driven analyses never record history or emit progress.
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
	// mode: ModeLocal persists on disk (desktop); cloud via Postgres + RLS.
	mode      config.DeploymentMode
	knowledge *rag.KnowledgeService
	// budget is the daily AI budget gate and in-flight ledger, shared with the knowledge base; nil (a bare
	// struct in tests) builds one over settings and backend on first use (aiBudget).
	budget     *AIBudget
	budgetOnce sync.Once
	// watchdogInterval / idleTimeout / hostedIdleTimeout: test overrides; 0 ⇒ defaults.
	watchdogInterval  time.Duration
	idleTimeout       time.Duration
	hostedIdleTimeout time.Duration
	// heartbeatAfter / heartbeatTick: client-keepalive test overrides; 0 ⇒ defaults.
	heartbeatAfter time.Duration
	heartbeatTick  time.Duration
	activeStreams  sync.Map   // map[streamID]*streamCtl — in-flight streams
	streamCapMu    sync.Mutex // serializes the per-caller concurrency check + store
	// finishedStreams keeps completed streams for the resumeRetention grace period.
	finishedStreams sync.Map // map[streamID]*streamCtl
	// convMutexes stripes the reconstruct/persist critical section (bounded lock set).
	convMutexes [convMutexStripes]sync.Mutex
	// resume mirrors stream state for cross-replica reconnect resume; no-op single-replica.
	resume resumeStore

	// pendingFixDecisions holds apply_fix proposals awaiting approve/decline, keyed by ID.
	pendingFixDecisions sync.Map
	// applyFixFunc overrides the fix mutation in tests; nil ⇒ FlowService.ApplyFix.
	applyFixFunc    applyFixFunc
	fixDecisionWait time.Duration

	// chatCtxCache memoises the scrubbed doc + built context; keys embed the flow's generation in chatCtxGen,
	// which InvalidateChatContext bumps and which fences the fills (chat_context.go).
	chatCtxCache cache.Cache
	chatCtxGen   eventbus.Gen[string]

	// azure resolves foundry model ids before the stream starts; nil keeps single-document behaviour.
	azure *AzureModelService
	// azureConnection builds a resource's provider; nil ⇒ the factory default. Tests script it.
	azureConnection func(scope string, ref ai.AzureConnectionRef, entries []ai.AzureRegisteredModel) (ai.Provider, error)
}

// ChatOptions are ChatService's optional collaborators; the zero value chats without the knowledge base, the
// Azure AI Foundry model registry or the Redis resume backplane, on a budget gate of its own.
type ChatOptions struct {
	// Knowledge answers retrieval for org-scoped turns; nil = no RAG.
	Knowledge *rag.KnowledgeService
	// Budget is the daily AI budget gate the knowledge base consults too; nil builds a private one.
	Budget *AIBudget
	// Azure resolves azure-foundry model ids on the model registry before the stream starts; nil keeps the
	// single-document behaviour.
	Azure *AzureModelService
	// Resume enables cross-replica stream resume; nil (PAD_REDIS_URL unset) keeps the single-replica default.
	Resume *redis.Client
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
	opts ChatOptions,
) *ChatService {
	s := &ChatService{
		notifier:      notifier,
		configDir:     configDir,
		flowCache:     flowCache,
		analysisCache: analysisCache,
		settings:      settings,
		factory:       factory,
		backend:       backend,
		mode:          mode,
		knowledge:     opts.Knowledge,
		budget:        opts.Budget,
		azure:         opts.Azure,
		resume:        noopResumeStore{},
		chatCtxCache:  newChatContextCache(),
	}
	if opts.Resume != nil {
		s.resume = redisResumeStore{c: opts.Resume}
	}
	return s
}

// resumeBackplane returns the resume store, or a no-op when unset (bare struct literal in tests).
func (s *ChatService) resumeBackplane() resumeStore {
	if s.resume == nil {
		return noopResumeStore{}
	}
	return s.resume
}

// GetAuthorizedFlow resolves a flow the stream's actor (carried by ctx) may perform action on: FlowService.GetAuthorized.
func (s *ChatService) GetAuthorizedFlow(ctx context.Context, flowID string, action Action) (*models.FlowDocument, error) {
	return s.flowCache.GetAuthorized(ctx, flowID, action)
}

// turnSetup is the resolved provider + model + attribution + RAG lookup for one turn; ragDone nil when RAG isn't wired.
type turnSetup struct {
	provider ai.Provider
	// model is what the turn runs on; read from here, never from the request.
	model   turnModel
	orgID   string
	ragDone <-chan string
}

// turnModel is the model a turn runs on, fixed pre-stream and carried in turnSetup — never via the request.
type turnModel struct {
	// resolved is the provider-level id every reader sends by (request's model or registry entry's deployment id).
	resolved string
	// requested is the id the answer persists with (request's model, or "m/<entryId>" for a registry default).
	requested string
	// label is the registry entry's display name stored with the answer; "" off the registry.
	label string
	// kind is the registry entry's kind — chat, claude or agent; "" off the
	// registry.
	kind string
	// maxOutput is the registry's output-token override (0 = none).
	maxOutput int
}

// newTurnModel is the turn model of req: the registry resolution's, else the request's own.
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

// agent reports a Foundry agent turn: a registry agent entry or a raw "agent:<name>" id.
func (m turnModel) agent() bool {
	return m.kind == ai.AzureModelKindAgent || ai.IsAgentModel(m.resolved)
}

// resolveAzureModel resolves a foundry model id pre-stream; unusable → ErrModelNotAvailable, other failures → a setup error.
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
		// Not the caller's own and org entries unreadable: "not available" would be a guess.
		return nil, providerSetupError(scope, req.Provider, roleErr)
	case errors.Is(err, ErrModelNotAvailable):
		return nil, ErrModelNotAvailable
	default:
		return nil, providerSetupError(scope, req.Provider, err)
	}
}

// turnProvider builds the turn's provider as exactly the resolved resource's credential; a gone key is a setup error.
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

// errProviderDisabled is the policy-disable sentinel; match with errors.Is.
var errProviderDisabled = errors.New("AI provider disabled by policy")

// providerDisabledByPolicy fires only on an unambiguous disable; absent entries read as enabled.
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

// prepareTurn resolves the provider, enforces the budget (one response reserved first) and starts RAG after the gates — no spend on refusal.
func (s *ChatService) prepareTurn(ctx context.Context, scope string, doc *models.FlowDocument, req models.ChatRequest, azure *AzureChatResolution, ctl *streamCtl) (turnSetup, error) {
	// Org flows bill the org even for outsiders; the shared credential is for its admins and members.
	orgID := ""
	if doc != nil {
		orgID = doc.OrganizationID
	}
	// Registry credential known pre-build: even a failed build keeps the org "ask your admin" copy.
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
		// Read by classifyFailure: a rejected shared credential is the org admin's to fix.
		ctl.sharedCredential.Store(shared)
	}
	model := newTurnModel(req, azure)
	// Policy gate on the resolved provider; every turn entry point enforces it here.
	var appSettings *models.AppSettings
	if s.settings != nil {
		appSettings = s.settings.Get()
	}
	if providerDisabledByPolicy(appSettings, provider.ID()) {
		return turnSetup{}, fmt.Errorf("%w: %s is disabled in this deployment's AI settings — pick another provider or ask your administrator to re-enable it", errProviderDisabled, provider.ID())
	}

	// Reserve one max response pre-check so overlapping starts see each other; pre-clamp MaxTokens makes over-reserving safe.
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
					// A panic here must not crash the process: deliver empty RAG guidelines.
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

// persistInitialUserTurn stores the user's turn at stream start (a closed tab must not lose it); false on error or history-off.
func (s *ChatService) persistInitialUserTurn(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest) bool {
	if doc == nil || req.UserMessage == "" {
		return false
	}
	convKey := req.ConversationKey()
	userID := clientMessageID(req.UserMessageID)
	// A resend reusing a stored message id gets a fresh one, not a duplicate in the history.
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
	stored, err := s.saveConversation(ctx, doc.ID, convKey, persisted)
	if err != nil {
		logger.Warn("chat: failed to persist user turn at stream start", "error", err)
	}
	return stored
}

// buildStreamRequest assembles the ai.Request (scrubbed context, RAG to deadline, clamped params); RAG-wait ctx.Done() → ok=false.
func (s *ChatService) buildStreamRequest(ctx context.Context, ts turnSetup, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (aiReq ai.Request, scrubbedDoc *models.FlowDocument, ok bool, err error) {
	core := s.cachedContextCore(ctx, scope, ts.provider, doc, report, req)

	// RAG timeout → "". Guidelines go in the final user message, never the system prompt — Azure/OpenAI cache prefixes.
	guidelines := ""
	if ts.ragDone != nil {
		select {
		case guidelines = <-ts.ragDone:
		case <-ctx.Done():
			return ai.Request{}, nil, false, nil
		}
	}
	scrubbedDoc = core.scrubbedDoc
	// Scrubbed once when the cached context was built (computeContextCore).
	sysPrompt := core.sysPrompt
	contextText := core.contextText

	// The provider-level id (ts.model.resolved) — a registry "m/<entryId>" answers to no deployment.
	temperature, maxTokens := turnGenerationParams(ctx, ts.provider, req, ts.model)
	aiReq = ai.Request{
		SystemPrompt: sysPrompt,
		Messages:     buildScopedMessages(scope, req, contextText, scrubber.ScrubText(guidelines)),
		Model:        ts.model.resolved,
		Temperature:  temperature,
		// An explicit temperature (0 included) is sent as chosen; unset keeps "provider default".
		TemperatureSet: req.TemperatureSet,
		MaxTokens:      maxTokens,
		// Per-provider reasoning depth; "" keeps the provider default.
		ReasoningEffort: req.ReasoningEffort,
		OrgID:           ts.orgID,
	}

	// Enforce the window before sending: truncate or fail with ErrContextLimit, never a raw provider 400.
	if _, err := truncateForContextWindow(ctx, ts.provider, &aiReq, ai.ContextLimitFor(ctx, ts.provider, ts.model.resolved), len(aiReq.Messages)-1); err != nil {
		return ai.Request{}, nil, false, err
	}

	return aiReq, scrubbedDoc, true, nil
}

// resolveStreamID assigns the stream identity: a client UUID (SSE subscribes before creating the stream) or a backend id (legacy begin).
func (s *ChatService) resolveStreamID(req models.ChatRequest) (streamID string, clientProvided bool, err error) {
	if req.ClientStreamID == "" {
		return uuid.NewString(), false, nil
	}
	if _, parseErr := uuid.Parse(req.ClientStreamID); parseErr != nil {
		return "", false, fmt.Errorf("clientStreamId must be a UUID: %w", parseErr)
	}
	return req.ClientStreamID, true, nil
}

// validateConversationID rejects a non-UUID — it becomes a storage key and a local-mode path segment.
func validateConversationID(req models.ChatRequest) error {
	if req.ConversationID == "" {
		return nil
	}
	if _, err := uuid.Parse(req.ConversationID); err != nil {
		// ErrInvalidInput so the HTTP layer answers 400, not a masked 500.
		return fmt.Errorf("%w: conversationId must be a UUID: %w", ErrInvalidInput, err)
	}
	return nil
}

// validReasoningEffort admits the provider default or low/medium/high; model-dependent levels are refused, not silently mapped.
func validReasoningEffort(effort string) bool {
	switch effort {
	case "", "low", "medium", "high":
		return true
	}
	return false
}

func validateReasoningEffort(effort string) error {
	if !validReasoningEffort(effort) {
		return fmt.Errorf("%w: reasoningEffort must be one of low, medium or high (omit it for the provider default)", ErrInvalidInput)
	}
	return nil
}

func (s *ChatService) StreamChatMessage(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (streamID string, err error) {
	defer logger.Guard("App.StreamChatMessage", &err)
	// The turn's repeated reads (settings row, org role) are made once and shared (turnMemo).
	ctx = withTurnMemo(ctx)

	// Ground the chat in cached findings without re-analyzing per turn; nil when no analysis has run.
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
	// Registry resolution before any stream exists: unusable → 400 now, other failures report on the stream; req is never changed.
	azureRes, azureErr := s.resolveAzureModel(ctx, scope, doc, req)
	if errors.Is(azureErr, ErrModelNotAvailable) {
		return "", azureErr
	}

	// The stream outlives its HTTP request: do NOT inherit the request context (cancelled when the handler returns).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxChatStreamDuration)
	// orgID/flowID/convKey are read lock-free elsewhere once set — a later write would race.
	orgID, flowID := "", ""
	if doc != nil {
		orgID = doc.OrganizationID
		flowID = doc.ID
	}
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{}), settled: make(chan struct{}), ownerID: scope, orgID: orgID, streamID: streamID, flowID: flowID, convKey: req.ConversationKey()}
	ctl.touch() // pre-first-chunk work (context build, provider dial) counts as activity
	ctl.lastEmit.Store(time.Now().UnixNano())
	// Per-caller cap + stream-ID reservation, atomically (tryStartStream).
	reserved, resErr := s.tryStartStream(streamID, scope, ctl)
	if resErr != nil {
		cancel()
		return "", resErr
	}
	if !reserved {
		cancel()
		return "", fmt.Errorf("%w (max %d) — wait for one to finish or stop it", ErrTooManyChatStreams, maxConcurrentStreamsPerScope)
	}

	// Client-supplied stream ID ⇒ the SSE listener is already up: unblock emission (idempotent); legacy leaves it to BeginStream.
	if clientProvided {
		ctl.startOnce.Do(func() { close(ctl.started) })
	}

	// Multi-replica: mirror the stream so a reconnect to another replica can resume.
	var mirrorFinished chan struct{}
	if rs := s.resumeBackplane(); rs.enabled() {
		snap, mark := ctl.snapshotMarked()
		if !rs.Save(ctx, streamID, snap, maxChatStreamDuration+resumeRetention) {
			mark = unpublishedMark // the mirror's first tick retries
		}
		mirrorFinished = make(chan struct{})
		go s.mirrorStream(ctx, streamID, ctl, mirrorFinished, mark)
	}

	go s.watchStream(ctx, streamID, scope, ctl)

	emit := func(eventType string, data map[string]interface{}) {
		// Journal first (under the ctl lock): agentic events survive a disconnect via replay.
		ctl.mu.Lock()
		ctl.recordJournal(eventType, data)
		ctl.mu.Unlock()
		ctl.lastEmit.Store(time.Now().UnixNano())
		s.notifier.EmitTo(scope, "chat:event",
			map[string]interface{}{"streamId": streamID, "type": eventType, "data": data})
	}
	// Coalesce consecutive chunk deltas into one SSE event; wrap() flushes before non-chunk events, keeping order.
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
		// persistAnswer: server-owned turns store the answer from cleanup, after leaving the active set — the write must not hold the slot.
		persistAnswer := false
		// turn is the turn's model once prepareTurn ran: the answer stores its requested id and label, never the provider-level id.
		var turn turnModel
		defer func() {
			// Every exit path must leave a terminal state; a stream that never began gets it via BeginStream/ResumeStream.
			if code, msg, set := ctl.ensureTerminal(ctx); set {
				select {
				case <-ctl.started:
					// Never shared: this path only records cancelled/interrupted streams.
					emit("error", errorEventData(code, msg, false))
				default:
				}
			}
			coalesce.flush() // deliver any batched-but-unsent tail before exit
			cancel()
			if mirrorFinished != nil {
				close(mirrorFinished) // done/error are recorded by now; the mirror publishes them last
			}
			// Finished set first: a conversation GET between the two calls must still find the stream.
			s.finishedStreams.Store(streamID, ctl)
			s.activeStreams.Delete(streamID)
			s.aiBudget().release(ctl)
			time.AfterFunc(resumeRetention, func() { s.finishedStreams.Delete(streamID) })
			if persistAnswer {
				// Read settings afresh: history-saving may have been switched off mid-stream.
				s.persistAssistantTurn(withoutTurnMemo(ctx), doc, req, ctl, turn.requested, turn.label)
			}
			close(ctl.settled)
		}()

		fail := func(code, msg string, shared bool) { failStream(ctl, emit, nil, code, msg, shared) }

		// Client keepalive for silent phases; not joined on exit — the deferred close is enough.
		hbStop := make(chan struct{})
		defer close(hbStop)
		hbAfter, hbTick := s.heartbeatTiming()
		go heartbeatStream(ctx, ctl, emit, hbStop, hbAfter, hbTick)

		// Registry unreadable at resolution: the generic setup error, never a fallback model.
		if azureErr != nil {
			fail(ctl.classifyFailureShared(ctx, azureErr))
			return
		}
		// The settings row is needed right after setup; read it while setup runs.
		s.prefetchCallerPreferences(ctx)
		// Provider resolution, budget checks (reservation first) and the RAG lookup.
		ts, err := s.prepareTurn(ctx, scope, doc, req, azureRes, ctl)
		if err != nil {
			fail(ctl.classifyFailureShared(ctx, err))
			return
		}
		turn = ts.model

		// Server-side reconstruction: empty Messages loads the prior conversation; non-empty (legacy/resend) is used as-is.
		serverOwnsHistory := len(req.Messages) == 0
		if serverOwnsHistory {
			// Store the answer at finish (a closed tab must not lose it); from cleanup so every exit path does it.
			persistAnswer = true
			// Under the per-conversation mutex (SaveConversation replaces the whole file); only then may a reload be sent back.
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
		// The loops below refresh the reservation per iteration with actual
		// spend plus one more response.

		// Tools: tctx by POINTER so an approved apply_fix swaps the fresh doc mid-stream; a Foundry agent always runs the loop.
		if req.UseTools || ts.model.agent() {
			tctx := ai.ToolContext{
				Ctx:      ctx,
				Doc:      scrubbedDoc,
				RealDoc:  doc,
				Analysis: readOnlyAnalysis{s.analysisCache},
			}
			if ap := s.newFixApplier(doc, streamID, emit, ctl.touch, func(fresh *models.FlowDocument) {
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

		// out masks secrets, maintains the resume buffer and counts chunk events — the done count detects SSE gaps.
		out := newScrubbedEmitter(ctl, emit)
		// sawText: whether answer text arrived (reasoning and keepalives are not) — an empty answer cut at the cap is a failure.
		sawText := false
		// lastServed: the model that answered, from any chunk naming it (alias providers).
		lastServed := ""
		ctl.beginProviderTurn()
		failUsage := func(code, msg string, shared bool, tokensIn, tokensOut int, served string) {
			failUsageStream(ctx, ctl, emit, nil, ts.provider, aiReq.Model, code, msg, shared, tokensIn, tokensOut, served)
		}
		err = ts.provider.Stream(ctx, aiReq, func(chunk ai.Chunk) {
			ctl.touch()
			ctl.sawOutput(chunk.Text != "", len(chunk.ToolCalls) > 0, chunk.Done)
			// A silent non-hosted phase (ai.Chunk.SilentStep) holds the hosted-step idle limit.
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
				// Same friendly mapping as a returned error.
				code, msg, shared := ctl.classifyFailureShared(ctx, chunk.Error)
				failStream(ctl, emit, nil, code, msg, shared)
				return
			}
			if chunk.ToolEvent != nil {
				out.flush()
				emitToolEvent(emit, chunk.ToolEvent)
				return
			}
			if chunk.Notice != nil {
				// A provider status line: a trail row, not a hosted step; once per turn (a retry repeats it).
				if ctl.firstNotice(chunk.Notice.Name) {
					out.flush()
					emit("tool", noticeEventData(chunk.Notice))
				}
				return
			}
			if chunk.Done {
				out.flush()
				if emptyOutputLimitTurn(sawText, len(chunk.ToolCalls), chunk.FinishReason) {
					// The provider still reported the usage; only the reply is missing.
					failUsage(ChatErrOutputLimit, outputLimitMsg, false, chunk.TokensIn, chunk.TokensOut, lastServed)
					return
				}
				ctl.markDone(chunk.TokensIn, chunk.TokensOut, lastServed)
				emit("done", doneEventData(ctx, ts.provider, aiReq.Model, lastServed, chunk.TokensIn, chunk.TokensOut, coalesce.flushAndCount()))
				return
			}

			// ServerStep markers, keepalives and split-off reasoning carry no answer text.
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
			// Flush scrubber-held text before the failure so the partial response isn't truncated client-side.
			out.flush()
			emit("error", errorEventData(code, msg, shared))
		}
	}()

	return streamID, nil
}

// toolLoopState is the machinery both tool-loop drivers share: begin gate, per-iteration budget re-check/context re-trim, token/cost ledger.
type toolLoopState struct {
	s        *ChatService
	ctx      context.Context
	provider ai.Provider
	// model is the loop's provider-level id; orgID the attribution its budget re-check enforces against.
	model string
	orgID string
	scope string
	ctl   *streamCtl
	emit  func(string, map[string]interface{})
	// awaitStart gates emission on the client's begin; doneChunks counts emitted chunk events.
	awaitStart func() bool
	doneChunks func() int

	// started is the consumed-once record of the begin gate (ctl.started holds one token).
	started bool

	// budgetUsage memoizes the loop's daily-usage reads across iterations.
	budgetUsage *turnBudgetUsage

	// tokens memoises truncation estimates; ctxLimit is the model-aware window (catalog overrides provider default).
	tokens   *tokenMemo
	ctxLimit int

	// The spend ledger: done rates blend per-iteration price × tokens (a router may serve each iteration differently).
	price      ai.Pricing
	reserve    float64
	pricer     ai.UsagePricer
	canPrice   bool
	totalIn    int
	totalOut   int
	costIn     float64
	costOut    float64
	unpriced   bool
	lastPrice  ai.Pricing
	lastServed string
}

// newToolLoopState snapshots the loop's fixed wiring and opens the ledger; only base's Model, OrgID and MaxTokens are read.
func newToolLoopState(
	s *ChatService,
	ctx context.Context,
	provider ai.Provider,
	base ai.Request,
	scope string,
	ctl *streamCtl,
	awaitStart func() bool,
	emit func(string, map[string]interface{}),
	doneChunks func() int,
) *toolLoopState {
	price := inFlightPricing(ctx, provider, base.Model)
	pricer, canPrice := provider.(ai.UsagePricer)
	return &toolLoopState{
		s:          s,
		ctx:        ctx,
		provider:   provider,
		model:      base.Model,
		orgID:      base.OrgID,
		scope:      scope,
		ctl:        ctl,
		emit:       emit,
		awaitStart: awaitStart,
		doneChunks: doneChunks,
		tokens:     newTokenMemo(),
		ctxLimit:   ai.ContextLimitFor(ctx, provider, base.Model),
		price:      price,
		reserve:    inFlightCost(price, 0, maxOutputTokens(base)),
		pricer:     pricer,
		canPrice:   canPrice,
	}
}

// ensureStarted consumes the begin gate at most once; emissions are held back until the client began.
func (st *toolLoopState) ensureStarted() bool {
	if !st.started && st.awaitStart() {
		st.started = true
	}
	return st.started
}

// fail is failStream through the begin gate: terminal state first, then emitted once the client began.
func (st *toolLoopState) fail(code, msg string, shared bool) {
	failStream(st.ctl, st.emit, st.ensureStarted, code, msg, shared)
}

// failUsage is failUsageStream through the begin gate, for a billed failure with totals in hand.
func (st *toolLoopState) failUsage(code, msg string, shared bool, tokensIn, tokensOut int, served string) {
	failUsageStream(st.ctx, st.ctl, st.emit, st.ensureStarted, st.provider, st.model, code, msg, shared, tokensIn, tokensOut, served)
}

// recheckBudget re-checks the daily budget every iteration (the loop multiplies spend up to maxToolIterations×); false = stop, already reported.
func (st *toolLoopState) recheckBudget() bool {
	own := inFlightCost(st.price, st.totalIn, st.totalOut)
	st.ctl.setInflight(own + st.reserve)
	if st.budgetUsage == nil {
		st.budgetUsage = &turnBudgetUsage{}
	}
	if err := st.s.enforceTurnBudgetMemo(st.ctx, st.provider, st.budgetUsage, st.scope, st.orgID, own, st.ctl); err != nil {
		code, msg, report := loopBudgetFailure(st.ctx, st.ctl, err)
		if report {
			// A mid-loop budget stop is the kind operators need aggregated: the turn already burned budget.
			errreport.CaptureError(st.ctx, "chat.tool_loop.budget", err, errreport.Attrs{"scope": st.scope, "provider": st.provider.ID()})
		}
		st.fail(code, msg, false) // a budget stop is never a shared-credential failure
		return false
	}
	return true
}

// trimmedTurn builds the iteration's request and re-enforces the window (tool results can overflow it by iteration 4-6).
func (st *toolLoopState) trimmedTurn(base ai.Request, msgs []ai.Message, pin int) (ai.Request, int, bool) {
	turn := base
	turn.Messages = msgs
	newPin, err := truncateForContextWindowMemo(st.ctx, st.provider, &turn, st.ctxLimit, pin, st.tokens)
	if err != nil {
		st.fail(ChatErrContextLimit, "conversation is too long for this model's context window — too many tool results, try a simpler question", false)
		return turn, pin, false
	}
	return turn, newPin, true
}

// addTurnUsage folds one iteration's usage into the ledger, priced per served model; unpriceable marks the turn unpriced.
func (st *toolLoopState) addTurnUsage(turnIn, turnOut int, turnServed string) {
	st.totalIn += turnIn
	st.totalOut += turnOut
	if st.canPrice {
		if p, ok := st.pricer.UsagePricing(st.ctx, st.model, turnServed); ok {
			st.lastPrice = p
			st.costIn += float64(turnIn) * p.InputCostPerM
			st.costOut += float64(turnOut) * p.OutputCostPerM
		} else {
			st.unpriced = true
		}
	} else {
		st.unpriced = true
	}
}

// emitDone is the success exit: mark done and emit the ledger's blended rates.
func (st *toolLoopState) emitDone() {
	st.ctl.markDone(st.totalIn, st.totalOut, st.lastServed)
	price, priced := blendedRates(st.lastPrice, st.costIn, st.totalIn, st.costOut, st.totalOut, st.unpriced)
	st.emit("done", doneEventDataRated(st.ctx, st.provider, st.model, st.lastServed, st.totalIn, st.totalOut, st.doneChunks(), price, priced))
}

// runToolLoop drives the native function-calling tool loop; self-contained — emits tool/chunk/done/error and updates ctl itself.
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
	st := newToolLoopState(s, ctx, provider, base, scope, ctl, awaitStart, emit, doneChunks)

	// replayItems: provider reasoning records replay verbatim; valid only while history stays APPEND-ONLY.
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

	// One scrubber spans the loop: each turn's text is flushed before its tool events, so held text can't bleed between turns.
	out := newScrubbedEmitter(ctl, emit)
	mem := newLoopToolMemory()
	// pin is the current turn's index in msgs; truncation never drops it and reports where it moved.
	pin := len(msgs) - 1
	for i := 0; i < maxToolIterations; i++ {
		metrics.RecordChatToolIteration()
		if !st.recheckBudget() {
			return
		}
		turn, newPin, ok := st.trimmedTurn(base, msgs, pin)
		if !ok {
			return
		}
		pin = newPin
		rewritten := !sameMessages(msgs, turn.Messages)
		msgs = turn.Messages
		if rewritten {
			// History rewritten by truncation: recorded turns are no longer valid to replay.
			stopReplay()
			turn.Messages = msgs
		}

		// Stream the turn: forward assistant text, capture tool calls/usage from the terminal chunk.
		var (
			turnText   strings.Builder
			turnCalls  []ai.ToolCall
			turnItems  json.RawMessage
			turnIn     int
			turnOut    int
			turnFinish string
			// turnServed is the model that answered THIS iteration — the unit the blended rates price.
			turnServed string
			chunkErr   error
		)
		ctl.beginProviderTurn()
		streamErr := provider.Stream(ctx, turn, func(c ai.Chunk) {
			ctl.touch()
			ctl.sawOutput(c.Text != "", len(c.ToolCalls) > 0, c.Done)
			// A silent phase holds the hosted-step idle limit before the begin gate.
			if c.SilentStep != 0 {
				ctl.hostedStep(c.SilentStep > 0)
			}
			if c.ServedModel != "" {
				st.lastServed = c.ServedModel
				turnServed = c.ServedModel
			}
			switch {
			case c.Error != nil:
				chunkErr = c.Error
			case c.ToolEvent != nil:
				// A server-side tool (agent web search, MCP): account for the idle watchdog, then forward to the trail.
				ctl.hostedStep(!c.ToolEvent.Done)
				if !st.ensureStarted() {
					return
				}
				out.flush()
				emitToolEvent(emit, c.ToolEvent)
			case c.Notice != nil:
				// A provider status line: a trail row, once per turn, no
				// hosted-step accounting.
				if !st.ensureStarted() || !ctl.firstNotice(c.Notice.Name) {
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
				if !st.ensureStarted() {
					return
				}
				// turnText keeps the raw text for the next model turn; client-bound text goes through the scrubber.
				turnText.WriteString(c.Text)
				out.text(c.Text)
			}
			// Anything else (keepalive, split-off reasoning, ServerStep) only feeds the idle watchdog.
		})
		ctl.endProviderTurn()
		if st.started {
			// Release scrubber-held text before any error/tool/done event so the tail isn't withheld or mis-attributed.
			out.flush()
		}
		if streamErr != nil {
			// A model that can't use tools: retry the SAME iteration once with tools dropped plus a status notice.
			if errors.Is(streamErr, ai.ErrToolsUnsupported) && len(base.Tools) > 0 {
				base.Tools = nil
				stopReplay() // the tools changed under the recorded turns
				if st.ensureStarted() {
					emit("tool", map[string]interface{}{"name": "tools_unsupported", "label": "This model can't use tools — answering without them"})
				}
				i-- // don't consume an iteration for the degradation retry
				continue
			}
			st.fail(ctl.classifyFailureShared(ctx, streamErr))
			return
		}
		if chunkErr != nil {
			// Same degradation when the rejection arrives as an in-stream error chunk.
			if errors.Is(chunkErr, ai.ErrToolsUnsupported) && len(base.Tools) > 0 {
				base.Tools = nil
				stopReplay()
				if st.ensureStarted() {
					emit("tool", map[string]interface{}{"name": "tools_unsupported", "label": "This model can't use tools — answering without them"})
				}
				i--
				continue
			}
			st.fail(ctl.classifyFailureShared(ctx, chunkErr))
			return
		}
		st.addTurnUsage(turnIn, turnOut, turnServed)

		// No tool calls → this turn is the final answer (its text already streamed; only the done event remains).
		if len(turnCalls) == 0 {
			if emptyOutputLimitTurn(turnText.Len() > 0, 0, turnFinish) {
				// Cut at the token cap before any answer (usage already recorded by the audited wrapper).
				st.failUsage(ChatErrOutputLimit, outputLimitMsg, false, st.totalIn, st.totalOut, st.lastServed)
				return
			}
			if !st.ensureStarted() {
				return
			}
			st.emitDone()
			return
		}

		if truncatedToolTurn(turnCalls, turnFinish) {
			// Cut mid-call at the output limit: running a call with incomplete arguments acts on something unfinished.
			st.failUsage(ChatErrOutputLimit, outputLimitMsg, false, st.totalIn, st.totalOut, st.lastServed)
			return
		}

		// Record the assistant turn, run the tools, append results; invalid JSON input records as {}.
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
		for _, r := range s.execTurnTools(emit, ctl, st.ensureStarted, mem, tctx, turnCalls) {
			msgs = append(msgs, ai.Message{Role: "tool", Content: r.result, ToolCallID: r.id})
		}
	}

	st.fail(ChatErrToolLoopLimit, fmt.Sprintf("tool loop exceeded %d iterations without a final answer", maxToolIterations), false)
}

// withoutProviderItems returns msgs with replay records removed (a fresh slice when any carried one, else msgs itself).
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

// hasForeignProviderItems reports a replay record from a different provider: the provider changed under the recorded turns.
func hasForeignProviderItems(msgs []ai.Message, providerID string) bool {
	return slices.ContainsFunc(msgs, func(m ai.Message) bool {
		return len(m.ProviderItems) > 0 && m.ProviderItemsFrom != providerID
	})
}

// sameMessages reports whether after is before unchanged — the same slice when truncation dropped nothing.
func sameMessages(before, after []ai.Message) bool {
	if len(before) != len(after) {
		return false
	}
	return len(before) == 0 || &before[0] == &after[0]
}

// runPromptToolLoop is the prompt-based fallback for providers without native function-calling: schemas in the prompt, <tool_call> blocks in the text.
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
	// APPEND to the system prompt, never inject a leading system message: truncation pins Messages[:1].
	toolInstr := ai.ToolPromptInstructions()
	if base.SystemPrompt != "" {
		turn := base
		turn.SystemPrompt = base.SystemPrompt + "\n\n" + toolInstr
		base = turn
	} else {
		base.SystemPrompt = toolInstr
	}
	st := newToolLoopState(s, ctx, provider, base, scope, ctl, awaitStart, emit, doneChunks)
	out := newScrubbedEmitter(ctl, emit)
	mem := newLoopToolMemory()
	pin := len(msgs) - 1 // the current turn (see runToolLoop)
	for i := 0; i < maxToolIterations; i++ {
		metrics.RecordChatToolIteration()
		if !st.recheckBudget() {
			return
		}
		turn, newPin, ok := st.trimmedTurn(base, msgs, pin)
		if !ok {
			return
		}
		pin = newPin
		msgs = turn.Messages

		// Buffer the full turn: <tool_call> markers must be parsed and stripped before anything reaches the client.
		var (
			turnText   strings.Builder
			turnIn     int
			turnOut    int
			turnFinish string
			// turnServed is the model that answered THIS iteration — the unit
			// the blended rates price.
			turnServed string
			chunkErr   error
		)
		ctl.beginProviderTurn()
		streamErr := provider.Stream(ctx, turn, func(c ai.Chunk) {
			ctl.touch()
			ctl.sawOutput(c.Text != "", len(c.ToolCalls) > 0, c.Done)
			if c.ServedModel != "" {
				st.lastServed = c.ServedModel
				turnServed = c.ServedModel
			}
			switch {
			case c.Error != nil:
				chunkErr = c.Error
			case c.Notice != nil:
				// A provider status line, once per turn (see runToolLoop).
				if st.ensureStarted() && ctl.firstNotice(c.Notice.Name) {
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
		if st.started {
			out.flush()
		}
		if streamErr != nil {
			st.fail(ctl.classifyFailureShared(ctx, streamErr))
			return
		}
		if chunkErr != nil {
			st.fail(ctl.classifyFailureShared(ctx, chunkErr))
			return
		}
		st.addTurnUsage(turnIn, turnOut, turnServed)

		full := turnText.String()
		calls := ai.ParsePromptToolCalls(full)
		cleanText := ai.StripPromptToolCalls(full)

		// No tool calls → this turn is the final answer. Emit the cleaned text
		// (preamble the model wrote around the absent marker) and finish.
		if len(calls) == 0 {
			if emptyOutputLimitTurn(full != "", 0, turnFinish) {
				st.failUsage(ChatErrOutputLimit, outputLimitMsg, false, st.totalIn, st.totalOut, st.lastServed)
				return
			}
			if !st.ensureStarted() {
				return
			}
			if cleanText != "" {
				out.text(cleanText)
				out.flush()
			}
			st.emitDone()
			return
		}

		// Emit the non-marker preamble (e.g. "Let me search for…"), then run each
		// tool and append its result for the next turn.
		if st.ensureStarted() && cleanText != "" {
			out.text(cleanText)
			out.flush()
		}
		msgs = append(msgs, ai.Message{Role: "assistant", Content: full})
		// Results go back as a USER message with <tool_result> markers: a bare role:"tool" is schema-invalid here.
		var results strings.Builder
		for _, tc := range calls {
			if !st.ensureStarted() {
				return
			}
			emit("tool", map[string]interface{}{"name": tc.Name, "label": ai.ToolLabel(tc.Name)})
			// Keep the idle watchdog aware of in-flight tool execution.
			ctl.touch()
			started := time.Now()
			result := mem.exec(tc.Name, tc.Input, tctx)
			ctl.touch()
			emitToolResult(emit, tc.Name, result, time.Since(started))
			fmt.Fprintf(&results, "<tool_result name=%q>\n%s\n</tool_result>\n", tc.Name, result)
		}
		msgs = append(msgs, ai.Message{Role: "user", Content: results.String()})
	}

	st.fail(ChatErrToolLoopLimit, fmt.Sprintf("tool loop exceeded %d iterations without a final answer", maxToolIterations), false)
}

// toolSummary derives the one-line summary of a tool_result; it inherits the scrubbed document's scrubbing.
func toolSummary(result string) string {
	line := result
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len(line) > 120 {
		// Back off to a rune boundary (same rule as ai.truncateResult) so a truncated multibyte char isn't sent as invalid UTF-8.
		cut := 120
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	return line
}

// usageEventFields is the usage half of a done/error payload, so a billed FAILED turn carries the same keys.
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

// failStream scrubs, stores the terminal state AND emits the error — state first, so replay sees it; gate (nil: at once) holds emission.
func failStream(ctl *streamCtl, emit func(string, map[string]interface{}), gate func() bool, code, msg string, shared bool) {
	msg = scrubber.ScrubText(msg)
	ctl.setFailure(code, msg, shared)
	if gate == nil || gate() {
		emit("error", errorEventData(code, msg, shared))
	}
}

// failUsageStream is failStream for a billed failure: the error carries the usage keys so the meter reports it.
func failUsageStream(ctx context.Context, ctl *streamCtl, emit func(string, map[string]interface{}), gate func() bool, p ai.Provider, model, code, msg string, shared bool, tokensIn, tokensOut int, served string) {
	msg = scrubber.ScrubText(msg)
	ctl.setFailureUsage(code, msg, shared, tokensIn, tokensOut, served)
	if gate == nil || gate() {
		data := errorEventData(code, msg, shared)
		for k, v := range usageEventFields(ctx, p, model, served, tokensIn, tokensOut) {
			data[k] = v
		}
		emit("error", data)
	}
}

// doneEventData is the done payload: usageEventFields plus the chunk count the client uses to detect SSE gaps.
func doneEventData(ctx context.Context, p ai.Provider, model, served string, tokensIn, tokensOut, chunks int) map[string]interface{} {
	data := usageEventFields(ctx, p, model, served, tokensIn, tokensOut)
	data["chunks"] = chunks
	return data
}

// doneEventDataRated is doneEventData with explicit blended rates; priced=false falls back to the plain payload.
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

// blendedRates derives a multi-iteration turn's effective rates (sum charged / total tokens); zero tokens keep the last rate.
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

// invalidToolInput reports a tool call whose arguments are non-empty but not valid JSON (empty = "no arguments").
func invalidToolInput(tc ai.ToolCall) bool {
	in := bytes.TrimSpace(tc.Input)
	return len(in) > 0 && !json.Valid(in)
}

// emitToolEvent forwards a provider-run server-side tool as the same events the local loop emits.
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

// noticeEventData is the payload of a provider status line: a trail row, not a hosted step.
func noticeEventData(n *ai.Notice) map[string]interface{} {
	return map[string]interface{}{"name": n.Name, "label": n.Label}
}

// emitToolResult reports a finished tool execution; ok follows ExecuteTool's "error: …" prefix.
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
