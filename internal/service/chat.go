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
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/ai/scrubber"
	"pad-core/logger"
	"pad-core/models"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// maxChatStreamDuration is the hard wall-clock backstop on a stream; the watchdog/idle timeout normally catches hangs first.
const maxChatStreamDuration = 10 * time.Minute

// maxConcurrentStreamsPerScope caps streams per caller, bounding one user's provider calls and goroutines.
const maxConcurrentStreamsPerScope = 3

// maxToolIterations bounds the read-only tool loop against a model that never stops requesting tools.
const maxToolIterations = 6

// maxOutputTokens is the request's output cap or the 4096 default.
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

// errStreamIDInUse is the stream-ID collision sentinel; match with errors.Is. A 409 STREAM_ID_IN_USE, not a 500: the
// id names a stream already started (on this replica or, claimed in Redis, another), which is what a resent start
// finds when its first send went through, so the client resumes that stream instead of failing the turn.
var errStreamIDInUse = apperr.Conflict("STREAM_ID_IN_USE", "clientStreamId already in use: a stream with this id was already started; resume it, or start with a new id")

// ErrTooManyChatStreams is returned when the caller's concurrent-stream cap is exceeded; exported so the HTTP layer maps it to a distinct code.
var ErrTooManyChatStreams = errors.New("too many chat responses running at once")

// ErrServerRestarting refuses a stream asked for once the server's stop has begun ending them (Shutdown).
var ErrServerRestarting = errors.New("the server is restarting — try again in a moment")

// convMutexStripes: per-conversation locks are striped for bounded memory, not exact-keyed.
const convMutexStripes = 256

func (s *ChatService) convMutexFor(flowID, convKey string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(flowID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(convKey))
	return &s.convMutexes[h.Sum32()%convMutexStripes]
}

// reconstructAndPersistUserTurn holds the per-conversation mutex across both steps (a file save is a full replace, and
// in cloud mode the lock queues this replica's writers while the version check settles the other replicas').
func (s *ChatService) reconstructAndPersistUserTurn(ctx context.Context, doc *models.FlowDocument, req *models.ChatRequest, convKey string) (stored bool, write *convWrite) {
	mu := s.convMutexFor(doc.ID, convKey)
	mu.Lock()
	defer mu.Unlock()
	if s.useBackendConversations() {
		return s.persistUserTurnVersioned(ctx, doc, req, convKey)
	}
	history, err := s.reconstructHistory(ctx, doc, *req)
	if err != nil {
		// Do NOT persist: SaveConversation is a full-file replace — a history we failed to read would be destroyed.
		logger.Warn("chat: history load failed; answering without prior context and "+
			"skipping persistence so the stored conversation is not overwritten", "error", err)
		req.Messages = nil
		return false, nil
	}
	req.Messages = history
	return s.persistInitialUserTurn(ctx, doc, *req), nil
}

// tryStartStream checks the per-caller cap and reserves the stream ID atomically, counting the stream's worker for
// Shutdown; a server that is shutting down refuses with ErrServerRestarting.
func (s *ChatService) tryStartStream(streamID, scope string, ctl *streamCtl) (bool, error) {
	s.streamCapMu.Lock()
	if s.closing {
		s.streamCapMu.Unlock()
		return false, ErrServerRestarting
	}
	if _, dup := s.finishedStreams.Load(streamID); dup {
		s.streamCapMu.Unlock()
		return false, errStreamIDInUse
	}
	if _, loaded := s.activeStreams.LoadOrStore(streamID, ctl); loaded {
		s.streamCapMu.Unlock()
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
		s.streamCapMu.Unlock()
		return false, nil
	}
	s.streamCapMu.Unlock()

	// The same two checks across the replicas (chat_replicas.go; a no-op with one replica), outside the lock.
	hold, dup, full := s.share.reserve(ctl)
	if dup || full {
		s.activeStreams.Delete(streamID)
		if dup {
			return false, errStreamIDInUse
		}
		return false, nil
	}
	ctl.shareHold = hold

	// The worker is counted under the lock that Shutdown sets closing under, so Shutdown's Wait cannot miss it; a stop
	// that began during the cross-replica reservation refuses the start and gives the entry back.
	s.streamCapMu.Lock()
	if s.closing {
		s.streamCapMu.Unlock()
		s.activeStreams.Delete(streamID)
		s.share.abort(ctl, hold)
		return false, ErrServerRestarting
	}
	s.workers.Add(1) // the worker StreamChatMessage starts next; it calls Done as its very last step
	s.streamCapMu.Unlock()
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
	// watchdogInterval / idleTimeout / hostedIdleTimeout: test overrides; 0 ⇒ defaults.
	// flowEditCheck decides whether the stream's caller may edit its flow (the fix tools' gate); nil asks the flow
	// service (canEditFlow). A test override.
	flowEditCheck     func(ctx context.Context, flowID string) error
	watchdogInterval  time.Duration
	idleTimeout       time.Duration
	hostedIdleTimeout time.Duration
	// heartbeatAfter / heartbeatTick: client-keepalive test overrides; 0 ⇒ defaults.
	heartbeatAfter time.Duration
	heartbeatTick  time.Duration
	activeStreams  sync.Map   // map[streamID]*streamCtl — in-flight streams
	streamCapMu    sync.Mutex // serializes the per-caller concurrency check + store
	// workers counts each stream's worker and resume mirror, so Shutdown waits for a stream's last emit, final
	// snapshot and answer persist. closing refuses new streams once Shutdown began; it is read and set under
	// streamCapMu, which so orders every workers.Add before Shutdown's Wait.
	workers sync.WaitGroup
	closing bool
	// shutdownBudget: test override of chatShutdownBudget; 0 ⇒ the default.
	shutdownBudget time.Duration
	// finishedStreams keeps completed streams for the resumeRetention grace period.
	finishedStreams sync.Map // map[streamID]*streamCtl
	// convMutexes stripes the reconstruct/persist critical section (bounded lock set).
	convMutexes [convMutexStripes]sync.Mutex
	// resume mirrors stream state for cross-replica reconnect resume; no-op single-replica.
	resume resumeStore
	// cluster links this replica to the others (stream commands, the approval and active-stream keys); nil without Redis.
	cluster *chatCluster
	// share makes the stream cap and the clientStreamId check hold across the replicas (chat_replicas.go); nil with one
	// replica or without Redis.
	share *streamShare

	// pendingFixDecisions holds apply_fix proposals awaiting approve/decline, keyed by ID.
	pendingFixDecisions sync.Map
	// applyFixFunc overrides the fix mutation in tests; nil ⇒ FlowService.ApplyFix.
	applyFixFunc    applyFixFunc
	fixDecisionWait time.Duration

	// chatCtxCache memoises the scrubbed doc + built context; keys embed the flow's generation in chatCtxGen,
	// which InvalidateChatContext bumps and which fences the fills (chat_context.go).
	chatCtxCache *chatContextCache
	chatCtxGen   eventbus.Gen[string]

	// azure resolves foundry model ids before the stream starts; nil keeps single-document behaviour.
	azure *AzureModelService
	// azureConnection builds a resource's provider; nil ⇒ the factory default. Tests script it.
	azureConnection func(ctx context.Context, scope string, ref ai.AzureConnectionRef, entries []ai.AzureRegisteredModel) (ai.Provider, error)
}

// ChatOptions are ChatService's optional collaborators; the zero value chats without the knowledge base, the
// Azure AI Foundry model registry or the Redis resume backplane.
type ChatOptions struct {
	// Knowledge answers retrieval for org-scoped turns; nil = no RAG.
	Knowledge *rag.KnowledgeService
	// Azure resolves azure-foundry model ids on the model registry before the stream starts; nil keeps the
	// single-document behaviour.
	Azure *AzureModelService
	// Resume enables cross-replica stream resume; nil (PAD_REDIS_URL unset) keeps the single-replica default.
	Resume *redis.Client
	// Instance names this replica to the others (config Runtime.InstanceID); with a boot suffix it is stamped in every
	// resume snapshot and is the address of the replica's command channel. Used with Resume only.
	Instance string
	// BusKey signs the stream commands between replicas (redisx.DeriveBusKey of the deployment's shared secret); empty
	// leaves them unsigned. Used with Resume only.
	BusKey []byte
	// MultiReplica: the deployment can run more than one replica (PAD_MAX_REPLICAS > 1). With Resume, the caller's
	// stream cap and the clientStreamId check then also hold across the replicas, and the replica keeps a liveness key
	// so a stop for a dead replica's stream is answered as an ended one (chat_replicas.go). Without it they stay per
	// replica, so a Redis maintenance window costs a single replica nothing.
	MultiReplica bool
	// Health is Resume's health (nil: never degraded); with it a degraded Redis is not asked at a stream start.
	Health *redisx.Health
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
		azure:         opts.Azure,
		resume:        noopResumeStore{},
		chatCtxCache:  newChatContextCache(),
	}
	if opts.Resume != nil {
		s.attachCluster(opts.Resume, opts.Instance, opts.BusKey)
		if opts.MultiReplica {
			s.shareAcrossReplicas(opts.Resume, opts.Health)
		}
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

// canEditFlow reports whether the stream's caller may edit doc (FlowEdit, without loading its content): the fix
// tools are offered only then. The apply step checks FlowEdit again on a fresh document (applyApprovedFix).
func (s *ChatService) canEditFlow(ctx context.Context, doc *models.FlowDocument) bool {
	if doc == nil {
		return false
	}
	if s.flowEditCheck != nil {
		return s.flowEditCheck(ctx, doc.ID) == nil
	}
	if s.flowCache == nil {
		return false
	}
	err := s.flowCache.CheckFlowPermission(ctx, doc.ID, FlowEdit)
	if err != nil && !errors.Is(err, ErrPermissionDenied) {
		// Not a denial but a failed check (a store error): the turn still runs, without the fix tools.
		logger.Warn("chat: the flow edit permission could not be checked; the fix tools are not offered", "flow_id", doc.ID, "error", err)
	}
	return err == nil
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

// resolveAzureModel resolves a foundry model id pre-stream; unusable → ErrModelNotAvailable, other failures → a
// setup error. It is also where a turn's provider id meets the registry first: an id no spec describes is
// ErrProviderUnknown (the client's mistake, 400 before any stream), never a setup failure.
func (s *ChatService) resolveAzureModel(ctx context.Context, scope string, doc *models.FlowDocument, req models.ChatRequest) (*AzureChatResolution, error) {
	if !ai.KnownProvider(req.Provider) {
		return nil, fmt.Errorf("%w: %s", ErrProviderUnknown, req.Provider)
	}
	if !registryFamily(s.azure, req.Provider) {
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
		return s.factory.ForOrg(ctx, scope, s.credentialOrg(ctx, orgID, scope), req.Provider)
	}
	build := s.azureConnection
	if build == nil {
		build = s.factory.ForAzureConnection
	}
	p, err := build(ctx, scope, res.ConnectionRef, res.Entries)
	switch {
	case errors.Is(err, ai.ErrKeyNotConfigured):
		return nil, fmt.Errorf("%w: %w", errAzureNoCredentials, err)
	case errors.Is(err, storage.ErrSecretDecryptFailed):
		return nil, fmt.Errorf("%w: %w", errAzureUnreadableCredentials, err)
	}
	return p, err
}

// prepareTurn resolves the provider (the factory's policy refuses a provider the deployment or the org does
// not allow) and starts RAG after that gate — no provider call on refusal.
func (s *ChatService) prepareTurn(ctx context.Context, scope string, doc *models.FlowDocument, req models.ChatRequest, azure *AzureChatResolution, ctl *streamCtl) (turnSetup, error) {
	// Org flows record the org's usage even for outsiders; the shared credential is for its admins and members.
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
	userMsg := newUserTurn(req, req.Messages)
	persisted := make([]models.ChatMessage, 0, len(req.Messages)+1)
	persisted = append(persisted, req.Messages...)
	persisted = append(persisted, userMsg)
	stored, err := s.saveConversation(ctx, doc.ID, convKey, persisted)
	if err != nil {
		logger.Warn("chat: failed to persist user turn at stream start", "error", err)
	}
	return stored
}

// buildStreamRequest assembles the ai.Request (the shared assembly: scrubbed context, RAG to deadline, clamped
// params, the gate's tools) and enforces the window; dropped is how many history messages went to fit it.
// RAG-wait ctx.Done() → ok=false.
func (s *ChatService) buildStreamRequest(ctx context.Context, ts turnSetup, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest, gate ai.ToolGate, useTools bool) (aiReq ai.Request, scrubbedDoc *models.FlowDocument, dropped int, ok bool, err error) {
	// RAG timeout → "". Guidelines go in the final user message, never the system prompt — Azure/OpenAI cache
	// prefixes. Awaited after the context build, which runs while the lookup does.
	guidelines := func() (string, bool) {
		if ts.ragDone == nil {
			return "", true
		}
		select {
		case g := <-ts.ragDone:
			return g, true
		case <-ctx.Done():
			return "", false
		}
	}
	a, ok, err := s.assembleTurnRequest(ctx, scope, ts, doc, report, req, guidelines, gate, useTools)
	if err != nil || !ok {
		return ai.Request{}, nil, 0, false, err
	}
	aiReq = a.req

	// Enforce the window before sending: truncate or fail with ErrContextLimit, never a raw provider 400. The pin
	// is the current turn's index; where it moved to is how many history messages were dropped (R-streaming-7).
	pin := len(aiReq.Messages) - 1
	newPin, err := truncateForContextWindow(ctx, ts.provider, &aiReq, ai.ContextLimitFor(ctx, ts.provider, ts.model.resolved), pin)
	if err != nil {
		return ai.Request{}, nil, 0, false, err
	}
	return aiReq, a.scrubbedDoc, pin - newPin, true, nil
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
	report = s.currentReport(doc, report)

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
	if errors.Is(azureErr, ErrModelNotAvailable) || errors.Is(azureErr, ErrProviderUnknown) {
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
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{}), settled: make(chan struct{}), ownerID: scope, streamID: streamID, flowID: flowID, convKey: req.ConversationKey()}
	// Every provider call of the turn (the RAG query embed included) is recorded for the flow's organisation, and
	// the provider policy sees it.
	ctx = ai.WithUsageScope(ctx, scope, orgID)
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
		if rs.Save(ctx, streamID, snap, maxChatStreamDuration+resumeRetention) {
			ctl.published.Store(true)
		} else {
			mark = unpublishedMark // the mirror's first tick retries
		}
		mirrorFinished = make(chan struct{})
		// Counted: its final snapshot, written after the worker exits, must land before Shutdown returns.
		s.workers.Go(func() { s.mirrorStream(ctx, streamID, ctl, mirrorFinished, mark) })
	}

	go s.watchStream(ctx, streamID, scope, ctl)

	// storeAnswer is set by the worker once it owns the turn's history: the answer must be in the store before
	// "done" leaves, and the event names the version it was stored at (ADR 0031, addendum).
	var storeAnswer func()
	emit := func(eventType string, data map[string]interface{}) {
		// One terminal event per stream, claimed before the answer is stored: after Shutdown ended the stream in a
		// stuck worker's place, the worker's late done or error is dropped, and so is its answer.
		if (eventType == "done" || eventType == "error") && !ctl.claimTerminalEvent(eventType) {
			return
		}
		if eventType == "done" && storeAnswer != nil {
			storeAnswer()
		}
		if eventType == "done" || eventType == "error" {
			ctl.stampConversationVersion(data)
		}
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
		// First deferred, so it runs last: after the terminal emit, the mirror hand-off and the answer persist.
		defer s.workers.Done()
		chatStreamStart()
		defer chatStreamEnd()
		defer func() {
			if r := recover(); r != nil {
				logger.Error("chat stream goroutine panicked", "streamID", streamID, "panic", r)
			}
		}()
		// persistAnswer: server-owned turns store the answer before "done" (storeAnswer), else from cleanup, after
		// leaving the active set — the write must not hold the slot. answerAttempted: that write ran; cleanup skips it.
		persistAnswer, answerAttempted := false, false
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
			s.share.finish(ctl, ctl.shareHold)
			time.AfterFunc(resumeRetention, func() { s.finishedStreams.Delete(streamID) })
			if persistAnswer && !answerAttempted {
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
			stored, write := s.reconstructAndPersistUserTurn(ctx, doc, &req, req.ConversationKey())
			if stored {
				ctl.userTurnStored.Store(true)
			}
			ctl.convWrite.Store(write)
			storeAnswer = func() {
				answerAttempted = true
				// Read settings afresh: history-saving may have been switched off mid-stream.
				ans := s.persistAssistantTurn(withoutTurnMemo(ctx), doc, req, ctl, turn.requested, turn.label)
				// Kept only when contiguous with the question's write; else someone else's message is in the version.
				if q := ctl.convWrite.Load(); q != nil && ans.Written && ans.Loaded == q.Version {
					ctl.convWrite.Store(&convWrite{Base: q.Base, Version: ans.Version})
				} else {
					ctl.convWrite.Store(nil)
				}
			}
		}

		// The context block found again by its anchor after a re-parse, or a row saying it is gone. A copy: req's
		// ContextBlockID stays the conversation's (legacy) history key and what its messages record.
		ctxReq, blockStatus := resolveContextBlock(doc, req)
		if blockStatus == contextBlockMissing {
			emit("tool", contextBlockMissingNotice())
		}
		// The session's gate: what this actor may run decides what the model is offered — the request carries
		// the tools, or the marker-mode instructions, from its assembly — and ExecuteTool asks it again before
		// any handler (ADR 0026). Approvals are wired whenever a fix applier can be (newFixApplier); the fix tools
		// need the caller's edit permission, resolved here once, before anything is offered; a Foundry agent
		// always runs the loop.
		useTools := req.UseTools || ts.model.agent()
		gate := ai.NewToolGate(auth.ActorFromContext(ctx), s.flowCache != nil, s.canEditFlow(ctx, doc))
		aiReq, scrubbedDoc, dropped, ok, err := s.buildStreamRequest(ctx, ts, scope, doc, report, ctxReq, gate, useTools)
		if err != nil {
			if errors.Is(err, errContextScrub) {
				// Fail closed: nothing was sent, and the document itself never stands in for its clone.
				logger.Error("chat: the flow's redacting clone could not be made; the turn was not sent", "error", err, "flow_id", doc.ID)
				fail(ChatErrContextUnavailable, errContextScrub.Error()+" — nothing was sent; try again", false)
				return
			}
			msg := "conversation is too long for this model's context window — start a new conversation or remove some history"
			fail(ChatErrContextLimit, msg, false) // a context limit is not the org's to fix
			return
		}
		if !ok {
			return // ctx cancelled while waiting on RAG
		}
		if dropped > 0 {
			// Said once, before the first chunk, and journaled: the user sees that earlier turns were left out.
			ctl.setDroppedHistory(dropped)
			emit("notice", historyTruncatedNotice(dropped, dropped))
		}
		// The loops below refresh the reservation per iteration with actual
		// spend plus one more response.

		// Tools: tctx by POINTER so an approved apply_fix swaps the fresh doc mid-stream.
		if useTools {
			tctx := ai.ToolContext{
				Ctx:      ctx,
				Doc:      scrubbedDoc,
				RealDoc:  doc,
				Analysis: readOnlyAnalysis{s.analysisCache},
			}
			if ap := s.newFixApplier(doc, streamID, emit, ctl.touch, toolDocSwap(&tctx)); ap != nil {
				if fa, ok := ap.(*chatFixApplier); ok && blockStatus == contextBlockResolved {
					fa.groundOn(doc, ctxReq.ContextBlockID)
				}
				tctx.Fixes = ap
			}
			tctx.Allow = gate
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
			failUsageStream(ctl, emit, nil, code, msg, shared, tokensIn, tokensOut, served)
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
				emit("done", doneEventData(lastServed, chunk.TokensIn, chunk.TokensOut, coalesce.flushAndCount()))
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

// toolLoopState is the machinery both tool-loop drivers share: begin gate, per-iteration context re-trim, token totals.
type toolLoopState struct {
	s        *ChatService
	ctx      context.Context
	provider ai.Provider
	// model is the loop's provider-level id.
	model string
	scope string
	ctl   *streamCtl
	emit  func(string, map[string]interface{})
	// awaitStart gates emission on the client's begin; doneChunks counts emitted chunk events.
	awaitStart func() bool
	doneChunks func() int

	// started is the consumed-once record of the begin gate (ctl.started holds one token).
	started bool

	// tokens memoises truncation estimates; ctxLimit is the model-aware window (catalog overrides provider default).
	tokens   *tokenMemo
	ctxLimit int
	// dropped is how many history messages the turn has left out so far (the request build's, then each
	// iteration's): the keptFrom of the next history_truncated notice.
	dropped int

	// The turn's token totals across iterations and the model that answered last.
	totalIn    int
	totalOut   int
	lastServed string
}

// newToolLoopState snapshots the loop's fixed wiring; only base's Model is read.
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
	return &toolLoopState{
		s:          s,
		ctx:        ctx,
		provider:   provider,
		model:      base.Model,
		scope:      scope,
		ctl:        ctl,
		emit:       emit,
		awaitStart: awaitStart,
		doneChunks: doneChunks,
		tokens:     newTokenMemo(),
		ctxLimit:   ai.ContextLimitFor(ctx, provider, base.Model),
		dropped:    ctl.droppedHistoryCount(),
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
	failUsageStream(st.ctl, st.emit, st.ensureStarted, code, msg, shared, tokensIn, tokensOut, served)
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
	if dropped := pin - newPin; dropped > 0 && st.ensureStarted() {
		// Tool results grew the prompt past the window: more of the history went, and the user is told again.
		st.dropped += dropped
		st.ctl.setDroppedHistory(st.dropped)
		st.emit("notice", historyTruncatedNotice(dropped, st.dropped))
	}
	return turn, newPin, true
}

// addTurnUsage folds one iteration's tokens into the turn's totals.
func (st *toolLoopState) addTurnUsage(turnIn, turnOut int) {
	st.totalIn += turnIn
	st.totalOut += turnOut
}

// emitDone is the success exit: mark done and emit the turn's totals.
func (st *toolLoopState) emitDone() {
	st.ctl.markDone(st.totalIn, st.totalOut, st.lastServed)
	st.emit("done", doneEventData(st.lastServed, st.totalIn, st.totalOut, st.doneChunks()))
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
	// A stream's request carries the gate's tools from its assembly (assembleTurnRequest, so the first window
	// check counted them); a caller that built its own request gets them here.
	if base.Tools == nil {
		base.Tools = ai.ToolDefinitionsFor(tctx.Allow)
	}
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
	refunds := 0
	// i == maxToolIterations is the one extra turn after the last step: told tools are over, the model turns
	// what the steps gathered into an answer instead of the turn failing with everything it paid for.
	for i := 0; i <= maxToolIterations; i++ {
		final := i == maxToolIterations
		// The note goes on this pass's copy of the request: a tools-unsupported retry re-enters the same pass.
		stepBase := base
		if final {
			stepBase.SystemPrompt = withStepLimitNote(base.SystemPrompt)
		}
		recordChatToolIteration()
		turn, newPin, ok := st.trimmedTurn(stepBase, msgs, pin)
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
		st.addTurnUsage(turnIn, turnOut)

		// No tool calls → this turn is the final answer (its text already streamed; only the done event remains).
		if len(turnCalls) == 0 {
			if emptyOutputLimitTurn(turnText.Len() > 0, 0, turnFinish) {
				// Cut at the token cap before any answer (usage already recorded by the audited wrapper).
				st.failUsage(ChatErrOutputLimit, outputLimitMsg, false, st.totalIn, st.totalOut, st.lastServed)
				return
			}
			if final && strings.TrimSpace(turnText.String()) == "" {
				// Told not to call tools, a model that meant only to call one may say nothing: a done with
				// nothing after the tool trail would read as an answer.
				st.failUsage(ChatErrToolLoopLimit, stepLimitNoAnswerMessage, false, st.totalIn, st.totalOut, st.lastServed)
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
		if final {
			// Still calling tools after the step limit: nothing more runs. Text written alongside the calls is
			// the answer the note asked for (it already streamed), so it stands and the trail says what was skipped.
			if strings.TrimSpace(turnText.String()) == "" {
				st.failUsage(ChatErrToolLoopLimit, stepLimitMessage, false, st.totalIn, st.totalOut, st.lastServed)
				return
			}
			if !st.ensureStarted() {
				return
			}
			emit("tool", stepLimitSkippedNotice(len(turnCalls)))
			st.emitDone()
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
		docBefore := currentDoc(tctx)
		for _, r := range s.execTurnTools(emit, ctl, st.ensureStarted, mem, tctx, turnCalls) {
			msgs = append(msgs, ai.Message{Role: "tool", Content: r.result, ToolCallID: r.id})
		}
		if currentDoc(tctx) != docBefore && refunds < maxAppliedStepRefunds {
			// An approved fix landed (the document was replaced): that step was the user's decision, and the
			// model has to list again before referencing any block, so neither may use up the answer's steps.
			refunds++
			i--
		}
	}

	st.failUsage(ChatErrToolLoopLimit, stepLimitMessage, false, st.totalIn, st.totalOut, st.lastServed)
}

// maxAppliedStepRefunds bounds how many steps approved fixes give back in one turn; spend stays bounded by the
// budget re-check each step, and every apply waited on a human.
const maxAppliedStepRefunds = 10

// stepLimitNote is added to the system prompt of the turn that follows the last tool step.
const stepLimitNote = "The tool step limit for this turn is reached: do not call any more tools. Answer the user now from the results you already have, and say plainly what is left undone."

// stepLimitMessage ends a turn whose model kept calling tools, and wrote nothing else, after stepLimitNote;
// stepLimitNoAnswerMessage one whose model obeyed it but answered nothing.
var (
	stepLimitMessage         = fmt.Sprintf("the step limit ended the turn: the model was still calling tools after %d tool steps", maxToolIterations)
	stepLimitNoAnswerMessage = fmt.Sprintf("the step limit ended the turn: the model gave no answer after %d tool steps", maxToolIterations)
)

func withStepLimitNote(system string) string {
	if system == "" {
		return stepLimitNote
	}
	return system + "\n\n" + stepLimitNote
}

// stepLimitSkippedNotice is the trail row for the tool calls a final turn made alongside its answer: the answer
// stands, the calls do not run.
func stepLimitSkippedNotice(calls int) map[string]interface{} {
	label := fmt.Sprintf("The tool step limit is reached — %d more tool calls skipped", calls)
	if calls == 1 {
		label = "The tool step limit is reached — one more tool call skipped"
	}
	return map[string]interface{}{"name": "step_limit_reached", "label": label}
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
	// The tool instructions are APPENDED to the system prompt, never injected as a leading system message
	// (truncation pins Messages[:1]); a stream's request carries them from its assembly (assembleTurnRequest), a
	// caller that built its own request gets them here.
	if toolInstr := ai.ToolPromptInstructionsFor(tctx.Allow); !strings.Contains(base.SystemPrompt, toolInstr) {
		base.SystemPrompt = withSuffix(base.SystemPrompt, toolInstr)
	}
	st := newToolLoopState(s, ctx, provider, base, scope, ctl, awaitStart, emit, doneChunks)
	out := newScrubbedEmitter(ctl, emit)
	mem := newLoopToolMemory()
	pin := len(msgs) - 1 // the current turn (see runToolLoop)
	refunds := 0
	for i := 0; i <= maxToolIterations; i++ { // the last pass is the final answer (see runToolLoop)
		final := i == maxToolIterations
		stepBase := base
		if final {
			stepBase.SystemPrompt = withStepLimitNote(base.SystemPrompt)
		}
		recordChatToolIteration()
		turn, newPin, ok := st.trimmedTurn(stepBase, msgs, pin)
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
			chunkErr   error
		)
		ctl.beginProviderTurn()
		streamErr := provider.Stream(ctx, turn, func(c ai.Chunk) {
			ctl.touch()
			ctl.sawOutput(c.Text != "", len(c.ToolCalls) > 0, c.Done)
			if c.ServedModel != "" {
				st.lastServed = c.ServedModel
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
		st.addTurnUsage(turnIn, turnOut)

		full := turnText.String()
		calls := ai.ParsePromptToolCalls(full)
		cleanText := ai.StripPromptToolCalls(full)
		if isOutputLimitFinish(turnFinish) && (len(calls) > 0 || ai.HasUnterminatedToolCall(full)) {
			// Cut at the output cap with a call in it: the call may be incomplete and must not run, nor its JSON
			// reach the client — the native loop's truncatedToolTurn rule.
			st.failUsage(ChatErrOutputLimit, outputLimitMsg, false, st.totalIn, st.totalOut, st.lastServed)
			return
		}

		// No tool calls → this turn is the final answer. Emit the cleaned text
		// (preamble the model wrote around the absent marker) and finish.
		if len(calls) == 0 {
			if emptyOutputLimitTurn(full != "", 0, turnFinish) {
				st.failUsage(ChatErrOutputLimit, outputLimitMsg, false, st.totalIn, st.totalOut, st.lastServed)
				return
			}
			if final && strings.TrimSpace(cleanText) == "" {
				// An empty final answer after the note (see runToolLoop).
				st.failUsage(ChatErrToolLoopLimit, stepLimitNoAnswerMessage, false, st.totalIn, st.totalOut, st.lastServed)
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
		if final {
			// Still emitting tool calls after the step limit: nothing more runs. The text around the markers is
			// the answer the note asked for (see runToolLoop); unlike there, it has not streamed yet.
			if strings.TrimSpace(cleanText) == "" {
				st.failUsage(ChatErrToolLoopLimit, stepLimitMessage, false, st.totalIn, st.totalOut, st.lastServed)
				return
			}
			if !st.ensureStarted() {
				return
			}
			out.text(cleanText)
			out.flush()
			emit("tool", stepLimitSkippedNotice(len(calls)))
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
		docBefore := currentDoc(tctx)
		for _, tc := range calls {
			if !st.ensureStarted() {
				return
			}
			emit("tool", map[string]interface{}{"name": tc.Name, "label": ai.ToolLabel(tc.Name)})
			// Keep the idle watchdog aware of in-flight tool execution.
			ctl.touch()
			started := time.Now()
			result := guardedToolRun(ai.ToolCall{Name: tc.Name}, func() toolExecResult {
				return toolExecResult{name: tc.Name, result: mem.exec(tc.Name, tc.Input, tctx)}
			}).result
			ctl.touch()
			emitToolResult(emit, tc.Name, result, time.Since(started))
			// In its data frame, escaped: a block name holding </tool_result> cannot close it (ADR 0036).
			fmt.Fprintf(&results, "%s\n", ai.ToolResultFrame(tc.Name, result))
		}
		msgs = append(msgs, ai.Message{Role: "user", Content: results.String()})
		if currentDoc(tctx) != docBefore && refunds < maxAppliedStepRefunds {
			refunds++ // an approved fix landed: see runToolLoop
			i--
		}
	}

	st.failUsage(ChatErrToolLoopLimit, stepLimitMessage, false, st.totalIn, st.totalOut, st.lastServed)
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

// usageEventFields is the usage half of a done/error payload, so a FAILED turn that used tokens carries the same keys.
func usageEventFields(served string, tokensIn, tokensOut int) map[string]interface{} {
	data := map[string]interface{}{"tokensIn": tokensIn, "tokensOut": tokensOut}
	if served != "" {
		data["servedModel"] = served
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

// failUsageStream is failStream for a failure that used tokens: the error carries the usage keys so the meter reports it.
func failUsageStream(ctl *streamCtl, emit func(string, map[string]interface{}), gate func() bool, code, msg string, shared bool, tokensIn, tokensOut int, served string) {
	msg = scrubber.ScrubText(msg)
	ctl.setFailureUsage(code, msg, shared, tokensIn, tokensOut, served)
	if gate == nil || gate() {
		data := errorEventData(code, msg, shared)
		for k, v := range usageEventFields(served, tokensIn, tokensOut) {
			data[k] = v
		}
		emit("error", data)
	}
}

// doneEventData is the done payload: usageEventFields plus the chunk count the client uses to detect SSE gaps.
func doneEventData(served string, tokensIn, tokensOut, chunks int) map[string]interface{} {
	data := usageEventFields(served, tokensIn, tokensOut)
	data["chunks"] = chunks
	return data
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

// noticeHistoryTruncated is the kind of the journaled notice event a turn emits when it leaves earlier history
// out to fit the window (R-streaming-7): dropped is how many messages went this time, keptFrom how many have
// gone in all — the index in the stored history of the first message the model still sees.
const noticeHistoryTruncated = "history_truncated"

func historyTruncatedNotice(dropped, keptFrom int) map[string]interface{} {
	return map[string]interface{}{"kind": noticeHistoryTruncated, "dropped": dropped, "keptFrom": keptFrom}
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

// GetSuggestedPrompts returns the caller's own prompt lists (their document in cloud, the deployment's on the
// desktop); a list they have not set falls back to the default one, never to nothing.
func (s *ChatService) GetSuggestedPrompts(ctx context.Context, hasBlock, hasFindings bool) (prompts []string, err error) {
	defer logger.Guard("App.GetSuggestedPrompts", &err)

	settings := s.callerPreferences(ctx)
	if settings == nil {
		return nil, fmt.Errorf("settings not found")
	}

	pick := func(p models.AIPromptsConfig) []string {
		switch {
		case hasBlock && hasFindings:
			return p.BlockWithFindings
		case hasFindings:
			return p.Finding
		case hasBlock:
			return p.Block
		default:
			return p.Flow
		}
	}
	prompts = pick(settings.AI.Prompts)
	if len(prompts) == 0 {
		prompts = pick(models.DefaultSettings().AI.Prompts)
	}
	return append([]string(nil), prompts...), nil
}
