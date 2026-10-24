package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"weak"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-core/ai/scrubber"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"
)

// maxChatContextCache bounds the cached per-turn context entries (one per scope/flow/generation/block/prompt/provider/model/report/sources combination).
const maxChatContextCache = 32

// maxScrubbedDocCache bounds the scrubbed clones the entries share, keyed by (scope, flow, generation) — one per flow.
const maxScrubbedDocCache = 4

// maxCachedScrubbedBlocks is the largest clone the doc cache holds; a bigger flow is re-scrubbed per turn.
const maxCachedScrubbedBlocks = 40_000

// ragGuidelinesDeadline caps the streaming path's wait for RAG guidelines; on timeout the turn proceeds without.
const ragGuidelinesDeadline = 800 * time.Millisecond

// ragGuidelinesTokenCap bounds the guidelines block (tail-truncated, keeping the header and top chunks).
const ragGuidelinesTokenCap = 1200

// contextReserve is the tokens kept free for the prompt/context when clamping MaxTokens; scaled down on a small window.
const contextReserve = 4000

// outputBackstop is the most max tokens a request may ask of a ctxLimit window: window less min(contextReserve, half the window).
func outputBackstop(ctxLimit int) int {
	return ctxLimit - min(contextReserve, ctxLimit/2)
}

// truncationReserve is the room truncateForContextWindow keeps free beside the prompt.
func truncationReserve(ctxLimit, maxTokens int) int {
	if maxTokens <= 0 {
		return contextReserve
	}
	return max(min(contextReserve, ctxLimit/4), maxTokens)
}

// exactCountBandPercent: above this share of the window, the estimate (+ reserve) is re-checked by the provider's tokenizer.
const exactCountBandPercent = 70

// normalizeChatParams clamps generation params; maxTokens bounded by maxOutput, with outputBackstop when the ceiling is unknown.
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
	// Backstop: never let MaxTokens approach the input window when the ceiling is unknown.
	if ctxLimit > 0 {
		if cap := outputBackstop(ctxLimit); maxTokens > cap {
			maxTokens = cap
		}
	}
	return temperature, maxTokens
}

// The chat's context caches (ADR 0017): a turn's scrubbed context texts and the scrubbed clones of the flow they
// point into, per caller (the scope), flow and edit generation (chatCtxGen); the texts also by every other input.
var (
	chatContexts = cache.Declare("chat.context",
		cache.Keys(cache.KindTenant, cache.KindResource, cache.KindVersion,
			cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam,
			cache.KindParam),
		cache.ReplicaLocal("a turn is served by the replica its stream runs on; another rebuilds the context on a miss"))
	scrubbedDocs = cache.Declare("chat.scrubbed-docs",
		cache.Keys(cache.KindTenant, cache.KindResource, cache.KindVersion),
		cache.ReplicaLocal("a turn is served by the replica its stream runs on; another re-scrubs the flow on a miss"))
)

// chatContextCache is the two-level memo: per-turn text entries plus the scrubbed clones they point into.
type chatContextCache struct {
	texts *cache.Typed[chatContextEntry]
	docs  *cache.Typed[scrubbedDocEntry]
}

// newChatContextCache builds the bounded LRUs (one unit per entry).
func newChatContextCache() *chatContextCache {
	return &chatContextCache{
		texts: cache.New[chatContextEntry](chatContexts, maxChatContextCache),
		docs:  cache.New[scrubbedDocEntry](scrubbedDocs, maxScrubbedDocCache),
	}
}

// InvalidateChatContext drops the flow's cached scrubbed context (FlowCacheSubscribers' "chat-context-cache"):
// its generation moves on, so the keys built from the old one are never asked for again, and a fill that read
// before the bump is refused.
func (s *ChatService) InvalidateChatContext(flowID string) {
	s.chatCtxGen.Bump(flowID)
}

// chatContextKeyAt keys a turn's context texts for a generation the caller already read; the zero Key (uncached)
// for a flow without an id.
func (s *ChatService) chatContextKeyAt(scope, flowID string, gen uint64, req models.ChatRequest, providerID, model string, report *models.AnalysisReport, sourceFP string, tokenBudget int, access ai.ToolAccess) cache.Key {
	if flowID == "" {
		return cache.Key{}
	}
	reportFP := ""
	if report != nil {
		reportFP = fmt.Sprintf("%d-%d", report.GeneratedAt.UnixNano(), len(report.Findings))
	}
	// sourceFP keys a source selection AND any on-disk change to it; ExcludeContext MUST be in the key.
	// req.SystemPrompt holds the MERGED suffix (settings + request), so editing the setting cannot serve a stale
	// context; the caller's token budget bounds the text, and the tool access picks the system prompt, so both
	// are in the key for the same reason.
	k, _ := chatContexts.Key(callerTenant(scope), cache.Resource(flowID), cache.Version(strconv.FormatUint(gen, 10)),
		cache.Param("block", req.ContextBlockID), cache.Param("prompt", req.SystemPrompt), cache.Param("provider", providerID),
		cache.Param("model", model), cache.Param("report", reportFP), cache.Param("sources", sourceFP),
		cache.Param("exclude", strconv.FormatBool(req.ExcludeContext)), cache.Param("budget", strconv.Itoa(tokenBudget)),
		cache.Param("tools", strconv.Itoa(int(access))))
	return k
}

// sourceFilesFingerprint reduces the selected-files list to a stable key fragment:
// name folded with size+mtime (names alone under-keyed it). Stat failure degrades to the name.
func sourceFilesFingerprint(dir string, files []string) string {
	if len(files) == 0 {
		return ""
	}
	cp := make([]string, len(files))
	copy(cp, files)
	sort.Strings(cp)
	for i, name := range cp {
		// Names are RELATIVE to the flow's directory (ReadSourceFiles joins them there); stat against the same base.
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			cp[i] = fmt.Sprintf("%s:%d:%d", name, info.Size(), info.ModTime().UnixNano())
		}
	}
	return strings.Join(cp, ";")
}

// contextScrub is the scrub applied to a cached context's system prompt and context text (tests count it).
var contextScrub = scrubber.ScrubText

// scrubDocument is the redacting deep clone of the flow (tests count it, and make it fail).
var scrubDocument = scrubber.ScrubDocument

// errContextScrub: the flow's redacting clone could not be made. The turn or the preview fails rather than carry
// the document itself — nothing the model sees may bypass the scrubber (ADR 0034).
var errContextScrub = errors.New("the flow could not be prepared for the assistant")

// scrubSourceText is the AST scrub of a selected source file (tests measure what it is handed).
var scrubSourceText = scrubber.ScrubSourceText

// chatContextValue is what a turn gets: the redacted clone (read-only downstream, safe to share) plus the pre-scrubbed texts.
type chatContextValue struct {
	scrubbedDoc *models.FlowDocument
	sysPrompt   string
	contextText string
	tokenBudget int // the budget the context was built to (the preview shows it)
}

// chatContextEntry is the cached texts only — a strong pointer here let 32 entries pin 32 clones. src is the
// document they were built from: the generation is read after the caller resolved its document, so an entry
// built from a document resolved before an edit committed may carry the new generation; a turn holding the
// edited document (another object) misses it instead of being served the old flow.
type chatContextEntry struct {
	src         weak.Pointer[models.FlowDocument]
	sysPrompt   string
	contextText string
}

// scrubbedDocEntry is a cached clone with the document it was scrubbed from (see chatContextEntry.src).
type scrubbedDocEntry struct {
	src weak.Pointer[models.FlowDocument]
	doc *models.FlowDocument
}

// scrubbedDocKey keys the doc cache: scope, flow and edit generation; the zero Key (uncached) without a flow id.
func scrubbedDocKey(scope, flowID string, gen uint64) cache.Key {
	if flowID == "" {
		return cache.Key{}
	}
	k, _ := scrubbedDocs.Key(callerTenant(scope), cache.Resource(flowID), cache.Version(strconv.FormatUint(gen, 10)))
	return k
}

// scrubbedDocFor returns the flow's scrubbed clone for gen, cached when within maxCachedScrubbedBlocks and the
// flow was not invalidated since gen was read.
func (s *ChatService) scrubbedDocFor(ctx context.Context, scope string, doc *models.FlowDocument, gen uint64) (*models.FlowDocument, error) {
	cc := s.chatCtxCache
	key := scrubbedDocKey(scope, doc.ID, gen)
	src := weak.Make(doc)
	if cc != nil {
		if e, ok := cc.docs.Get(key); ok && e.src == src {
			return e.doc, nil
		}
	}
	sd, err := scrubDocument(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errContextScrub, err)
	}
	if cc != nil && len(sd.BlocksByID) <= maxCachedScrubbedBlocks && s.chatCtxGen.FillOK(ctx, doc.ID, gen) {
		cc.docs.Set(key, scrubbedDocEntry{src: src, doc: sd}, 1, 0)
	}
	return sd, nil
}

// ragGuidelines returns the turn's KB guidelines or "": this RLS-exempt path gates org knowledge on MEMBERSHIP, fail closed.
func (s *ChatService) ragGuidelines(ctx context.Context, scope string, doc *models.FlowDocument, userMessage string) string {
	if s.knowledge == nil || doc == nil || doc.OrganizationID == "" {
		return ""
	}
	if !s.isOrgMember(ctx, doc.OrganizationID, scope) {
		recordRAGLookup("skipped")
		return ""
	}
	guidelines, err := s.knowledge.Search(ctx, scope, doc.OrganizationID, userMessage)
	guidelines = ai.TruncateToTokenLimit(guidelines, ragGuidelinesTokenCap)
	switch {
	case err != nil:
		if ctx.Err() == nil { // deadline skips are expected under load, not errors
			recordRAGLookup("error")
		} else {
			recordRAGLookup("skipped")
		}
	case guidelines == "":
		recordRAGLookup("miss")
	default:
		recordRAGLookup("hit")
	}
	if err != nil || guidelines == "" {
		return ""
	}
	return guidelines
}

// isOrgMember reports org membership; "" and store errors fail closed — a missing proof must never widen knowledge access.
func (s *ChatService) isOrgMember(ctx context.Context, orgID, userID string) bool {
	_, ok, err := s.orgRoleCached(ctx, orgID, userID)
	if err != nil {
		logger.Warn("rag guidelines: org membership lookup failed; skipping guidelines", "error", err)
		return false
	}
	return ok
}

// orgRole returns userID's role in orgID; ok is false outside the org, in local mode and on a store error.
func (s *ChatService) orgRole(ctx context.Context, orgID, userID string) (role auth.Role, ok bool) {
	role, ok, err := s.orgRoleCached(ctx, orgID, userID)
	if err != nil {
		logger.Warn("chat: org role lookup failed; not using the org's shared credential", "error", err)
		return "", false
	}
	return role, ok
}

// credentialOrg is the org whose shared credential the caller may fall back to: admin/member only.
func (s *ChatService) credentialOrg(ctx context.Context, orgID, scope string) string {
	if orgID == "" {
		return ""
	}
	if role, ok := s.orgRole(ctx, orgID, scope); ok && OrgRoleUsesSharedCredential(role) {
		return orgID
	}
	return ""
}

// registryCredentialOrg is credentialOrg for registry resolution, where a store error is returned, not folded into "".
func (s *ChatService) registryCredentialOrg(ctx context.Context, orgID, scope string) (string, error) {
	if orgID == "" {
		return "", nil
	}
	role, ok, err := s.orgRoleCached(ctx, orgID, scope)
	if err != nil {
		return "", fmt.Errorf("look up the caller's role in the flow's organisation: %w", err)
	}
	if ok && OrgRoleUsesSharedCredential(role) {
		return orgID, nil
	}
	return "", nil
}

// cachedContextCore returns the memoised scrub+BuildContext result; bypassed with no document or no cache. A
// document whose redacting clone cannot be made is an error (errContextScrub), never a context built on the
// document itself.
func (s *ChatService) cachedContextCore(ctx context.Context, scope string, provider ai.Provider, model string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest, access ai.ToolAccess) (chatContextValue, error) {
	// Resolve the caller's custom instructions and context budget once, here where the caller is known.
	req.SystemPrompt = s.systemPromptSuffix(ctx, req)
	tokenBudget := s.contextTokenBudget(ctx, provider, model)
	if s.chatCtxCache == nil || doc == nil {
		sd, err := scrubbedOrNil(doc)
		if err != nil {
			return chatContextValue{}, err
		}
		return s.computeContextCoreWith(doc, sd, report, req, tokenBudget, access), nil
	}
	// One generation read for both keys, before the build: the entry and the clone it points into must agree,
	// and an invalidation during the build refuses the fill.
	gen := s.chatCtxGen.Load(doc.ID)
	src := weak.Make(doc)
	key := s.chatContextKeyAt(scope, doc.ID, gen, req, provider.ID(), req.Model, report,
		sourceFilesFingerprint(filepath.Dir(doc.FilePath), req.SelectedSourceFiles), tokenBudget, access)
	// The texts are memoised; the clone comes from the doc cache, never the entry.
	if e, ok := s.chatCtxCache.texts.Get(key); ok && e.src == src {
		sd, err := s.scrubbedDocFor(ctx, scope, doc, gen)
		if err != nil {
			return chatContextValue{}, err
		}
		return chatContextValue{scrubbedDoc: sd, sysPrompt: e.sysPrompt, contextText: e.contextText}, nil
	}
	sd, err := s.scrubbedDocFor(ctx, scope, doc, gen)
	if err != nil {
		return chatContextValue{}, err
	}
	cv := s.computeContextCoreWith(doc, sd, report, req, tokenBudget, access)
	if s.chatCtxGen.FillOK(ctx, doc.ID, gen) {
		s.chatCtxCache.texts.Set(key, chatContextEntry{src: src, sysPrompt: cv.sysPrompt, contextText: cv.contextText}, 1, 0)
	}
	return cv, nil
}

// What resolveContextBlock found of the request's context block (ContextPreview.ContextBlock).
const (
	contextBlockResolved = "resolved"
	contextBlockMissing  = "missing"
)

// resolveContextBlock finds the request's context block in doc by id, else by its anchor: an applied fix or an
// edit re-parses the flow and re-mints every block id. ContextBlockID becomes the id found, so the context, its
// cache key and the fix applier all use the live one. The status is "" when the turn asks for no block context
// (or the document has no index to look in), contextBlockMissing when the block is no longer in the flow.
func resolveContextBlock(doc *models.FlowDocument, req models.ChatRequest) (models.ChatRequest, string) {
	if req.ExcludeContext || req.ContextBlockID == "" || doc == nil || doc.BlocksByID == nil {
		return req, ""
	}
	if doc.BlocksByID[req.ContextBlockID] != nil {
		return req, contextBlockResolved
	}
	if req.ContextBlockAnchor != nil {
		if id := doc.ResolveAnchor(*req.ContextBlockAnchor); id != "" {
			req.ContextBlockID = id
			return req, contextBlockResolved
		}
	}
	return req, contextBlockMissing
}

// contextBlockMissingNotice is the stream's row for a context block that is no longer in the flow.
func contextBlockMissingNotice() map[string]interface{} {
	return map[string]interface{}{"name": "context_block_missing", "label": "The block this chat was about is no longer in the flow — answering about the whole flow"}
}

// defaultContextTokenBudget is the per-provider context budget when the caller's settings set none (matches
// DefaultSettings).
const defaultContextTokenBudget = 4000

// MaxContextTokenBudget caps the per-provider context budget a caller's settings can set, the same maximum the
// Settings UI's field stops at (CONTEXT_TOKEN_BUDGET_MAX in frontend/src/lib/constraints.ts): the settings write
// validates only the profile section, so without it an API client could have every turn build a context of any
// size.
const MaxContextTokenBudget = 128000

// contextTokenBudget is the caller's context token budget for a turn on provider and model: their own document
// in cloud (a user's setting is theirs, not the deployment's), s.settings on the desktop, at most
// MaxContextTokenBudget; without a setting, the budget the model's window affords (derivedContextBudget).
func (s *ChatService) contextTokenBudget(ctx context.Context, provider ai.Provider, model string) int {
	if prefs := s.callerPreferences(ctx); prefs != nil {
		if pc, ok := prefs.AI.Providers[provider.ID()]; ok && pc.ContextTokenBudget > 0 {
			return min(pc.ContextTokenBudget, MaxContextTokenBudget)
		}
	}
	return derivedContextBudget(ai.ContextLimitFor(ctx, provider, model))
}

// derivedContextBudget is the context budget a model's window affords when the caller set none (R-config-9): a
// quarter of the window less the answer's reserve, so the history, the tool results and the answer have the
// rest — never under the old flat default (an 8k window gets what it always got) and never over
// MaxContextTokenBudget (the retrieval tools give the model the rest of a large flow). The budget is in the
// context cache key, so the preview and the send derive the same one.
func derivedContextBudget(window int) int {
	if window <= 0 {
		return defaultContextTokenBudget
	}
	budget := max(defaultContextTokenBudget, min(MaxContextTokenBudget, (window-contextReserve)/4))
	// A window under twice the floor cannot hold the floor beside an answer: half of it, then.
	return min(budget, max(window/2, 1))
}

// maxSourceTokenBudget bounds what a selected source file is cut to before the AST scrub and the tokenizer see
// it (BoundSourceText: 16 bytes per token, 256 KB here), whatever the context budget: the scrub runs per cache
// miss at ~0.5 s/MB, and the context's final cut keeps the first budget tokens anyway.
const maxSourceTokenBudget = 16_000

// computeContextCore is the uncached scrub+BuildContext path — expensive per-block work identical across turns.
// Without a caller (no ctx) the budget is the deployment's default one.
func (s *ChatService) computeContextCore(provider ai.Provider, model string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest, access ai.ToolAccess) (chatContextValue, error) {
	sd, err := scrubbedOrNil(doc)
	if err != nil {
		return chatContextValue{}, err
	}
	return s.computeContextCoreWith(doc, sd, report, req, s.contextTokenBudget(context.Background(), provider, model), access), nil
}

// scrubbedOrNil is the redacting clone of a document that may be absent; a clone that cannot be made is
// errContextScrub, never the document itself.
func scrubbedOrNil(doc *models.FlowDocument) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, nil
	}
	sd, err := scrubDocument(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errContextScrub, err)
	}
	return sd, nil
}

// computeContextCoreWith is computeContextCore for a clone already scrubbed (shared via the doc cache) and a
// budget already resolved (it is in the cache key).
func (s *ChatService) computeContextCoreWith(doc, scrubbedDoc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest, tokenBudget int, access ai.ToolAccess) chatContextValue {
	ctxReq := ai.ContextRequest{
		Flow:               scrubbedDoc,
		TokenBudget:        tokenBudget,
		SystemPromptSuffix: req.SystemPrompt,
		ToolAccess:         access,
	}
	if report != nil {
		ctxReq.Findings = report.Findings
	}
	// Injection is gated on !ExcludeContext; ContextBlockID is ALSO the history key, so it stays present.
	if !req.ExcludeContext && req.ContextBlockID != "" && scrubbedDoc != nil && scrubbedDoc.BlocksByID != nil {
		ctxReq.SelectedBlock = scrubbedDoc.BlocksByID[req.ContextBlockID]
		ctxReq.SelectedSubflow = scrubbedDoc.BlockSubflow[req.ContextBlockID]
		// Not even by its anchor (resolveContextBlock ran first): the model is told, not left without the section.
		ctxReq.SelectedBlockMissing = ctxReq.SelectedBlock == nil
		if ctxReq.SelectedBlock != nil {
			ctxReq.VariableEvents = selectedBlockLineage(scrubbedDoc, ctxReq.SelectedBlock)
		}
	}
	// Selected source files (desktop only). AST-scrubbed — the regex pass misses PAD password syntax — and byte-bounded.
	if !req.ExcludeContext && len(req.SelectedSourceFiles) > 0 && s.flowCache != nil && doc != nil && doc.FilePath != "" {
		if sources, sErr := s.flowCache.ReadSourceFiles(doc, req.SelectedSourceFiles); sErr == nil && len(sources) > 0 {
			for name, content := range sources {
				sources[name] = scrubSourceText(ai.BoundSourceText(content, min(tokenBudget, maxSourceTokenBudget)))
			}
			ctxReq.RawSourceFiles = sources
		}
	}

	sysPrompt, contextText := ai.BuildContext(ctxReq)
	// Scrubbed here, once per cache entry: every reusing turn used to scrub both texts again.
	return chatContextValue{scrubbedDoc: scrubbedDoc, sysPrompt: contextScrub(sysPrompt), contextText: contextScrub(contextText), tokenBudget: tokenBudget}
}

// maxLineageVariables bounds the variables whose lineage the selected block's Variable History shows: its
// output variable, then the variables it references, in the block's order (BuildContext bounds the events).
const maxLineageVariables = 5

// selectedBlockLineage is the Variable History of the selected block (R-analyzer-boundary-4), computed on the
// redacted clone (an event names a block and a line, never a value); nil when the block sets and reads nothing.
func selectedBlockLineage(doc *models.FlowDocument, block *models.Block) map[string][]models.VariableEvent {
	var names []string
	if out := strings.TrimSpace(block.Properties["_output"]); out != "" {
		names = append(names, out)
	}
	for _, v := range block.Variables {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(names, v) {
			names = append(names, v)
		}
	}
	if len(names) > maxLineageVariables {
		names = names[:maxLineageVariables]
	}
	var events map[string][]models.VariableEvent
	for _, name := range names {
		if h := analyzer.BuildVariableLineage(doc, name); h != nil && len(h.Events) > 0 {
			if events == nil {
				events = make(map[string][]models.VariableEvent, len(names))
			}
			events[name] = h.Events
		}
	}
	return events
}

// buildScopedMessages assembles scrubbed history plus ONE user turn; only user/assistant roles pass.
func buildScopedMessages(scope string, req models.ChatRequest, contextText, guidelines string) []ai.Message {
	messages := make([]ai.Message, 0, len(req.Messages)+1)
	for i, m := range req.Messages {
		role, ok := historyRole(m.Role)
		if !ok {
			logger.Debug("chat: dropping a history message with a role history may not carry", "role", oneLineRole(m.Role), "index", i)
			continue
		}
		// Memoised (historyScrubs): the same stored messages come back every turn.
		content := historyScrubs.scrubText(scope, m.Content)
		if strings.TrimSpace(content) == "" {
			logger.Debug("chat: dropping an empty history message", "role", role, "index", i)
			continue
		}
		messages = append(messages, ai.Message{Role: role, Content: content})
	}
	// Through the memo too: this question is the next turn's history.
	userContent := historyScrubs.scrubText(scope, req.UserMessage)
	// The RAG guidelines ride in the user turn so the prompt cache covers the byte-identical prefix. They and the
	// flow context sit in their data frames (ADR 0036): the system prompt says what is inside is not instructions.
	if guidelines != "" {
		userContent = ai.Frame(ai.FrameGuidelines, guidelines) + "\n\n" + userContent
	}
	if !req.ExcludeContext && contextText != "" {
		userContent = ai.Frame(ai.FrameFlowContext, contextText) + "\n\n" + userContent
	}
	return append(messages, ai.Message{Role: "user", Content: userContent})
}

// historyRole canonicalises a history role: "user"/"assistant" (any case, trimmed) pass; others are refused.
func historyRole(role string) (string, bool) {
	switch r := strings.ToLower(strings.TrimSpace(role)); r {
	case "user", "assistant":
		return r, true
	}
	return "", false
}

// oneLineRole bounds a refused role for the log: it is client input.
func oneLineRole(role string) string {
	const maxRoleLog = 32
	role = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, role)
	if r := []rune(role); len(r) > maxRoleLog {
		return string(r[:maxRoleLog]) + "…"
	}
	return role
}

// promptTokenCeiling is a tokenizer-free upper bound (estimators yield ≤ 1 token per byte).
func promptTokenCeiling(req *ai.Request) int {
	n := len(req.SystemPrompt)
	for _, td := range req.Tools {
		n += len(td.InputSchema) + 8
	}
	for _, m := range req.Messages {
		c := len(m.Content)
		for _, tc := range m.ToolCalls {
			c += len(tc.Input)
		}
		n += max(c, len(m.ProviderItems)) + 4
	}
	return n
}

// truncateForContextWindow drops oldest turns until the prompt fits the window; the current turn (pin) is never dropped.
func truncateForContextWindow(ctx context.Context, provider ai.Provider, req *ai.Request, ctxLimit int, pin int) (int, error) {
	return truncateForContextWindowMemo(ctx, provider, req, ctxLimit, pin, nil)
}

// tokenMemo remembers EstimateTokens results across one turn's tool-loop iterations; nil consults tokenCounts.
type tokenMemo struct {
	m map[string]int
}

func newTokenMemo() *tokenMemo { return &tokenMemo{m: make(map[string]int)} }

// estimate is provider.EstimateTokens(text), memoised.
func (t *tokenMemo) estimate(provider ai.Provider, text string) int {
	if t == nil {
		return tokenCounts.estimate(provider, text)
	}
	if n, ok := t.m[text]; ok {
		return n
	}
	n := tokenCounts.estimate(provider, text)
	t.m[text] = n
	return n
}

// estimateBytes is estimate for a byte slice; a hit converts without a copy.
func (t *tokenMemo) estimateBytes(provider ai.Provider, b []byte) int {
	if t != nil {
		if n, ok := t.m[string(b)]; ok {
			return n
		}
	}
	return t.estimate(provider, string(b))
}

// truncateForContextWindowMemo is truncateForContextWindow with memoised estimates (nil: none).
func truncateForContextWindowMemo(ctx context.Context, provider ai.Provider, req *ai.Request, ctxLimit int, pin int, memo *tokenMemo) (int, error) {
	if ctxLimit <= 0 || len(req.Messages) == 0 {
		return pin, nil // unknown limit — can't enforce; nothing to truncate
	}
	msgs := req.Messages
	if pin < 0 || pin >= len(msgs) {
		pin = len(msgs) - 1 // the current turn is the last message the caller built
	}
	// One reserve for byte bound, estimate and count — the fast paths stay as sound as the drop loop.
	reserve := truncationReserve(ctxLimit, req.MaxTokens)
	ceiling := promptTokenCeiling(req)
	if ceiling+reserve <= (ctxLimit*4)/5 {
		return pin, nil // fits in bytes with 20% margin, so it fits in tokens — no tiktoken pass, no count call
	}
	// Past the byte bound but within the window, every estimator yields ≤ 1 token per byte — only the provider's tokenizer overrules.
	_, canCount := provider.(ai.TokenCounter)
	if ceiling+reserve <= ctxLimit && !canCount {
		return pin, nil
	}
	nearWindow := func(estimate int) bool {
		return canCount && (estimate+reserve)*100 > ctxLimit*exactCountBandPercent
	}

	// Fixed component (system prompt + tool schemas) — constant across the drop loop.
	fixed := memo.estimate(provider, req.SystemPrompt)
	for _, td := range req.Tools {
		fixed += memo.estimateBytes(provider, td.InputSchema) + 8
	}
	// Per-message costs + suffix sums (the old loop was O(n²)); a replay record is bounded by the MAX of content and record estimate.
	msgCost := func(m ai.Message) int {
		// Per-message framing (~4 tokens) + each tool_use's argument JSON verbatim.
		c := memo.estimate(provider, m.Content) + 4
		for _, tc := range m.ToolCalls {
			c += memo.estimateBytes(provider, tc.Input)
		}
		if len(m.ProviderItems) > 0 {
			c = max(c, memo.estimateBytes(provider, m.ProviderItems))
		}
		return c
	}
	suffix := make([]int, len(msgs)+1)
	for i := len(msgs) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + msgCost(msgs[i])
	}
	curCost := suffix[pin] - suffix[pin+1]
	// keptEstimate is the unscaled estimate of the request that keeps history from b and post-turn exchanges from a.
	keptEstimate := func(b, a int) int {
		return fixed + (suffix[b] - suffix[pin]) + curCost + suffix[a]
	}
	// keep sets req.Messages to that kept set; nothing dropped leaves the caller's slice in place (identity-compared).
	keep := func(b, a int) {
		if b == 0 && a == pin+1 {
			req.Messages = msgs
			return
		}
		kept := make([]ai.Message, 0, (pin-b)+1+(len(msgs)-a))
		kept = append(kept, msgs[b:pin]...)
		kept = append(kept, msgs[pin])
		kept = append(kept, msgs[a:]...)
		req.Messages = kept
	}
	// drop runs the drop loop against the estimate's costs × scale (rounded up so it never under-counts).
	drop := func(scale float64) (b, a int, ok bool) {
		s, f := suffix, fixed
		if scale > 1 {
			s = make([]int, len(suffix))
			for i, v := range suffix {
				s[i] = int(math.Ceil(float64(v) * scale))
			}
			f = int(math.Ceil(float64(fixed) * scale))
		}
		b, a = 0, pin+1
		fits := func() bool {
			return f+(s[b]-s[pin])+(s[pin]-s[pin+1])+s[a]+reserve <= ctxLimit
		}
		for !fits() && b < pin { // oldest history turns; kept prefix starts with a user turn
			b++
			for b < pin && msgs[b].Role != "user" {
				b++
			}
		}
		for !fits() && a < len(msgs) { // oldest exchange after the current turn (assistant + its results)
			a++
			for a < len(msgs) && msgs[a].Role != "assistant" {
				a++
			}
		}
		return b, a, fits()
	}
	b, a, ok := drop(1)
	if !ok {
		// Even the current turn (+ system prompt and tools) exceeds the window.
		return pin, ai.ErrContextLimit
	}
	keep(b, a)
	if est := keptEstimate(b, a); est > 0 && nearWindow(est) {
		// The estimate is inside its error band: the provider's tokenizer gets the last word (over → scale and drop again).
		if n, has := countPromptTokens(ctx, provider, req); has && n+reserve > ctxLimit {
			scale := float64(n) / float64(est)
			if b, a, ok = drop(scale); !ok {
				return pin, ai.ErrContextLimit
			}
			keep(b, a)
			// Verify once more; still over → tighten the scale to the kept set and drop again, without further counting.
			if n, has := countPromptTokens(ctx, provider, req); has && n+reserve > ctxLimit {
				if est := keptEstimate(b, a); est > 0 {
					if b2, a2, ok2 := drop(max(scale, float64(n)/float64(est))); ok2 && (b2 != b || a2 != a) {
						b, a = b2, a2
						keep(b, a)
					}
				}
			}
		}
	}
	return pin - b, nil
}

// countPromptTokens asks the provider's tokenizer; a counting problem must never fail a turn.
func countPromptTokens(ctx context.Context, provider ai.Provider, req *ai.Request) (tokens int, ok bool) {
	counter, canCount := provider.(ai.TokenCounter)
	if !canCount {
		return 0, false
	}
	n, err := counter.CountTokens(ctx, *req)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// currentReport is the turn's analysis report: the caller's, else the cached current one (the chat is grounded
// in cached findings without re-analysing per turn); nil when no analysis has run. The preview resolves it the
// same way, so it shows the Analysis Summary the turn sends first and shares the turn's cache entry.
func (s *ChatService) currentReport(doc *models.FlowDocument, report *models.AnalysisReport) *models.AnalysisReport {
	if report == nil && s.analysisCache != nil && doc != nil {
		if cached, ok := s.analysisCache.CurrentReport(doc); ok {
			return cached
		}
	}
	return report
}

// guidelineSource hands assembleTurnRequest the turn's knowledge-base guidelines once the context is built (the
// worker's lookup runs meanwhile): ok is false when the turn was cancelled while waiting.
type guidelineSource func() (guidelines string, ok bool)

// fixedGuidelines is a guidelineSource with the text in hand.
func fixedGuidelines(text string) guidelineSource {
	return func() (string, bool) { return text, true }
}

// turnAssembly is a turn's request as the first provider call sees it, before the window check, with the parts
// the preview shows: the context's own system prompt and text (unframed), the guidelines, the budget the context
// was built to and how many history messages were forwarded.
type turnAssembly struct {
	req         ai.Request
	scrubbedDoc *models.FlowDocument
	sysPrompt   string
	contextText string
	guidelines  string
	tokenBudget int
	history     int
}

// assembleTurnRequest builds the request of a turn: the memoised scrubbed context, the scrubbed history and the
// question with the flow context and the guidelines in their frames, the generation parameters and — when the
// turn uses tools — the gate's tool definitions (a provider with function calling) or the marker-mode tool
// instructions appended to the system prompt. buildStreamRequest and PreviewContext share it, so the preview
// shows what the worker sends (R-analyzer-boundary-3) and the request snapshot golden pins it (R-testing-8).
func (s *ChatService) assembleTurnRequest(ctx context.Context, scope string, ts turnSetup, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest, guidelines guidelineSource, gate ai.ToolGate, useTools bool) (a turnAssembly, ok bool, err error) {
	core, err := s.cachedContextCore(ctx, scope, ts.provider, ts.model.resolved, doc, report, req, ai.ToolAccessOf(useTools, gate))
	if err != nil {
		return turnAssembly{}, false, err
	}
	text := ""
	if guidelines != nil {
		if text, ok = guidelines(); !ok {
			return turnAssembly{}, false, nil
		}
	}
	text = scrubber.ScrubText(text)
	messages := buildScopedMessages(scope, req, core.contextText, text)
	// The provider-level id (ts.model.resolved) — a registry "m/<entryId>" answers to no deployment.
	temperature, maxTokens := turnGenerationParams(ctx, ts.provider, req, ts.model)
	aiReq := ai.Request{
		SystemPrompt: core.sysPrompt,
		Messages:     messages,
		Model:        ts.model.resolved,
		Temperature:  temperature,
		// An explicit temperature (0 included) is sent as chosen; unset keeps "provider default".
		TemperatureSet: req.TemperatureSet,
		MaxTokens:      maxTokens,
		// Per-provider reasoning depth; "" keeps the provider default.
		ReasoningEffort: req.ReasoningEffort,
		OrgID:           ts.orgID,
	}
	if useTools {
		if ts.provider.SupportsTools() {
			aiReq.Tools = ai.ToolDefinitionsFor(gate)
		} else {
			// Marker mode: the instructions are APPENDED to the system prompt, never injected as a leading system
			// message (truncation pins Messages[:1]).
			aiReq.SystemPrompt = withSuffix(aiReq.SystemPrompt, ai.ToolPromptInstructionsFor(gate))
		}
	}
	return turnAssembly{req: aiReq, scrubbedDoc: core.scrubbedDoc, sysPrompt: core.sysPrompt, contextText: core.contextText,
		guidelines: text, tokenBudget: core.tokenBudget, history: len(messages) - 1}, true, nil
}

// withSuffix appends suffix to text after a blank line, or is suffix when text is empty.
func withSuffix(text, suffix string) string {
	if text == "" {
		return suffix
	}
	return text + "\n\n" + suffix
}

// requestTokenEstimate is the prompt estimate the window check works with — the system prompt, every tool schema
// (+8 framing) and every message (+4 framing), by the provider's estimator — by part: the last message split
// into the flow-context frame and the rest of the turn.
func requestTokenEstimate(provider ai.Provider, req *ai.Request, contextFrame string) (int, models.ContextPreviewBreakdown) {
	est := provider.EstimateTokens
	var b models.ContextPreviewBreakdown
	b.System = est(req.SystemPrompt)
	for _, td := range req.Tools {
		b.Tools += est(string(td.InputSchema)) + 8
	}
	n := len(req.Messages)
	for _, m := range req.Messages[:max(n-1, 0)] {
		b.History += est(m.Content) + 4
	}
	if n > 0 {
		last := est(req.Messages[n-1].Content) + 4
		if contextFrame != "" {
			b.Context = est(contextFrame)
		}
		b.User = max(last-b.Context, 0)
	}
	return b.System + b.Tools + b.History + b.Context + b.User, b
}

// PreviewContext returns what a turn would send, without a chat call: the turn's own request assembly
// (assembleTurnRequest) over the cached report, the stored history and the tool gate, as StreamChatMessage builds
// it, so the preview and the send agree and share one chat.context entry (R-analyzer-boundary-3).
func (s *ChatService) PreviewContext(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (res *models.ContextPreview, err error) {
	defer logger.Guard("App.PreviewContext", &err)
	ctx = withTurnMemo(ctx) // one org-role and settings read, as a turn makes
	report = s.currentReport(doc, report)

	// Same resolution as prepareTurn: the preview must accept exactly what the turn would.
	orgID := ""
	if doc != nil {
		orgID = doc.OrganizationID
	}
	ctx = ai.WithUsageScope(ctx, scope, orgID) // the flow's org for the policy, as the worker seeds it
	azure, err := s.resolveAzureModel(ctx, scope, doc, req)
	if err != nil {
		return nil, err
	}
	provider, err := s.turnProvider(ctx, scope, orgID, req, azure)
	if err != nil {
		return nil, err
	}
	model := newTurnModel(req, azure)

	// The stored conversation, as the send rebuilds it when the request carries no history (in cloud mode the
	// model's context ends before a question already stored under its id, as persistUserTurnVersioned cuts it);
	// a history that cannot be read previews without one, as the turn then answers.
	if len(req.Messages) == 0 && doc != nil {
		if history, hErr := s.reconstructHistory(ctx, doc, req); hErr == nil {
			if i := storedUserTurn(req, history); i >= 0 && s.useBackendConversations() {
				history = history[:i]
			}
			req.Messages = history
		} else {
			logger.Warn("chat preview: history load failed; previewing without prior context", "error", hErr)
		}
	}
	asked := req.ContextBlockID
	req, blockStatus := resolveContextBlock(doc, req)
	useTools := req.UseTools || model.agent()
	gate := ai.NewToolGate(auth.ActorFromContext(ctx), s.flowCache != nil, s.canEditFlow(ctx, doc))
	ts := turnSetup{provider: provider, model: model, orgID: orgID}
	// The knowledge base under the stream's deadline: a slow lookup previews without guidelines, as the turn runs.
	guidelines := func() (string, bool) {
		ragCtx, cancel := context.WithTimeout(ctx, ragGuidelinesDeadline)
		defer cancel()
		return s.ragGuidelines(ragCtx, scope, doc, req.UserMessage), true
	}
	a, _, err := s.assembleTurnRequest(ctx, scope, ts, doc, report, req, guidelines, gate, useTools)
	if err != nil {
		return nil, err
	}

	ctxLimit := ai.ContextLimitFor(ctx, provider, model.resolved)
	// The window check the send makes, on a copy: how many history messages it would leave out.
	probe := a.req
	dropped := 0
	if pin := len(probe.Messages) - 1; pin >= 0 {
		if newPin, tErr := truncateForContextWindow(ctx, provider, &probe, ctxLimit, pin); tErr == nil {
			dropped = pin - newPin
		} else {
			dropped = pin // not even the turn alone fits: the history would all go, and the turn still fail
		}
	}
	contextText, contextFrame := "", ""
	if !req.ExcludeContext && a.contextText != "" {
		contextText = a.contextText
		contextFrame = ai.Frame(ai.FrameFlowContext, a.contextText) + "\n\n"
	}
	total, breakdown := requestTokenEstimate(provider, &a.req, contextFrame)
	var tools []string
	if useTools {
		for _, td := range ai.ToolDefinitionsFor(gate) {
			tools = append(tools, td.Name)
		}
	}

	res = &models.ContextPreview{
		SystemPrompt: a.req.SystemPrompt,
		ContextText:  contextText,
		// The turn as sent, less the flow-context frame shown above: the guidelines in their frame, the question.
		UserMessage:     strings.TrimPrefix(a.req.Messages[len(a.req.Messages)-1].Content, contextFrame),
		EstimatedTokens: total,
		ContextLimit:    ctxLimit,
		Breakdown:       breakdown,
		HistoryMessages: a.history,
		HistoryDropped:  dropped,
		ToolsOffered:    useTools,
		Tools:           tools,
		ContextBudget:   a.tokenBudget,
		ContextBlock:    blockStatus,
	}
	if req.ContextBlockID != asked {
		res.ContextBlockID = req.ContextBlockID
	}
	return res, nil
}

// systemPromptSuffix merges the caller's custom-instructions setting with the per-request override (server-side, tamper-proof).
func (s *ChatService) systemPromptSuffix(ctx context.Context, req models.ChatRequest) string {
	var configured string
	if app := s.callerPreferences(ctx); app != nil {
		configured = strings.TrimSpace(app.AI.SystemPromptSuffix)
	}
	perRequest := strings.TrimSpace(req.SystemPrompt)
	switch {
	case configured == "":
		return perRequest
	case perRequest == "":
		return configured
	default:
		return configured + "\n\n" + perRequest
	}
}
