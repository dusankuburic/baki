package service

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"weak"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-core/ai/scrubber"
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
			cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam, cache.KindParam),
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
func (s *ChatService) chatContextKeyAt(scope, flowID string, gen uint64, req models.ChatRequest, providerID, model string, report *models.AnalysisReport, sourceFP string) cache.Key {
	if flowID == "" {
		return cache.Key{}
	}
	reportFP := ""
	if report != nil {
		reportFP = fmt.Sprintf("%d-%d", report.GeneratedAt.UnixNano(), len(report.Findings))
	}
	// sourceFP keys a source selection AND any on-disk change to it; ExcludeContext MUST be in the key.
	// req.SystemPrompt holds the MERGED suffix (settings + request), so editing the setting cannot serve a stale context.
	k, _ := chatContexts.Key(callerTenant(scope), cache.Resource(flowID), cache.Version(strconv.FormatUint(gen, 10)),
		cache.Param("block", req.ContextBlockID), cache.Param("prompt", req.SystemPrompt), cache.Param("provider", providerID),
		cache.Param("model", model), cache.Param("report", reportFP), cache.Param("sources", sourceFP),
		cache.Param("exclude", strconv.FormatBool(req.ExcludeContext)))
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

// scrubDocument is the redacting deep clone of the flow (tests count it).
var scrubDocument = scrubber.ScrubDocument

// scrubSourceText is the AST scrub of a selected source file (tests measure what it is handed).
var scrubSourceText = scrubber.ScrubSourceText

// chatContextValue is what a turn gets: the redacted clone (read-only downstream, safe to share) plus the pre-scrubbed texts.
type chatContextValue struct {
	scrubbedDoc *models.FlowDocument
	sysPrompt   string
	contextText string
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
func (s *ChatService) scrubbedDocFor(ctx context.Context, scope string, doc *models.FlowDocument, gen uint64) *models.FlowDocument {
	cc := s.chatCtxCache
	key := scrubbedDocKey(scope, doc.ID, gen)
	src := weak.Make(doc)
	if cc != nil {
		if e, ok := cc.docs.Get(key); ok && e.src == src {
			return e.doc
		}
	}
	sd := scrubOrSelf(doc)
	if cc != nil && len(sd.BlocksByID) <= maxCachedScrubbedBlocks && s.chatCtxGen.FillOK(ctx, doc.ID, gen) {
		cc.docs.Set(key, scrubbedDocEntry{src: src, doc: sd}, 1, 0)
	}
	return sd
}

// scrubOrSelf folds scrubDocument's failure into the unscrubbed document (per-string ScrubText still redacts output).
func scrubOrSelf(doc *models.FlowDocument) *models.FlowDocument {
	sd, err := scrubDocument(doc)
	if err != nil {
		logger.Error("Failed to scrub document", "error", err)
		return doc
	}
	return sd
}

// buildScrubbedContext is the SYNCHRONOUS path (shared with PreviewContext); guidelines belong to the user message, never the byte-stable system prompt.
func (s *ChatService) buildScrubbedContext(ctx context.Context, scope string, provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (scrubbedDoc *models.FlowDocument, sysPrompt, contextText, guidelines string) {
	cv := s.cachedContextCore(ctx, scope, provider, doc, report, req)
	guidelines = s.ragGuidelines(ctx, scope, doc, req.UserMessage)
	return cv.scrubbedDoc, cv.sysPrompt, cv.contextText, scrubber.ScrubText(guidelines)
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

// cachedContextCore returns the memoised scrub+BuildContext result; bypassed with no document or no cache.
func (s *ChatService) cachedContextCore(ctx context.Context, scope string, provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) chatContextValue {
	// Resolve the caller's custom instructions once, here where the caller is known.
	req.SystemPrompt = s.systemPromptSuffix(ctx, req)
	if s.chatCtxCache == nil || doc == nil {
		return s.computeContextCore(provider, doc, report, req)
	}
	// One generation read for both keys, before the build: the entry and the clone it points into must agree,
	// and an invalidation during the build refuses the fill.
	gen := s.chatCtxGen.Load(doc.ID)
	src := weak.Make(doc)
	key := s.chatContextKeyAt(scope, doc.ID, gen, req, provider.ID(), req.Model, report,
		sourceFilesFingerprint(filepath.Dir(doc.FilePath), req.SelectedSourceFiles))
	// The texts are memoised; the clone comes from the doc cache, never the entry.
	if e, ok := s.chatCtxCache.texts.Get(key); ok && e.src == src {
		return chatContextValue{scrubbedDoc: s.scrubbedDocFor(ctx, scope, doc, gen), sysPrompt: e.sysPrompt, contextText: e.contextText}
	}
	sd := s.scrubbedDocFor(ctx, scope, doc, gen)
	cv := s.computeContextCoreWith(provider, doc, sd, report, req)
	if s.chatCtxGen.FillOK(ctx, doc.ID, gen) {
		s.chatCtxCache.texts.Set(key, chatContextEntry{src: src, sysPrompt: cv.sysPrompt, contextText: cv.contextText}, 1, 0)
	}
	return cv
}

// computeContextCore is the uncached scrub+BuildContext path — expensive per-block work identical across turns.
func (s *ChatService) computeContextCore(provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) chatContextValue {
	var scrubbedDoc *models.FlowDocument
	if doc != nil {
		scrubbedDoc = scrubOrSelf(doc)
	}
	return s.computeContextCoreWith(provider, doc, scrubbedDoc, report, req)
}

// computeContextCoreWith is computeContextCore for a clone already scrubbed (shared via the doc cache).
func (s *ChatService) computeContextCoreWith(provider ai.Provider, doc, scrubbedDoc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) chatContextValue {
	// Per-provider context token budget from settings, falling back to 4000 (matches DefaultSettings).
	tokenBudget := 4000
	if s.settings != nil {
		if appSettings := s.settings.Get(); appSettings != nil {
			if pc, ok := appSettings.AI.Providers[provider.ID()]; ok && pc.ContextTokenBudget > 0 {
				tokenBudget = pc.ContextTokenBudget
			}
		}
	}

	ctxReq := ai.ContextRequest{
		Flow:               scrubbedDoc,
		TokenBudget:        tokenBudget,
		Provider:           provider,
		SystemPromptSuffix: req.SystemPrompt,
	}
	if report != nil {
		ctxReq.Findings = report.Findings
	}
	// Injection is gated on !ExcludeContext; ContextBlockID is ALSO the history key, so it stays present.
	if !req.ExcludeContext && req.ContextBlockID != "" && scrubbedDoc != nil && scrubbedDoc.BlocksByID != nil {
		ctxReq.SelectedBlock = scrubbedDoc.BlocksByID[req.ContextBlockID]
		ctxReq.SelectedSubflow = scrubbedDoc.BlockSubflow[req.ContextBlockID]
	}
	// Selected source files (desktop only). AST-scrubbed — the regex pass misses PAD password syntax — and byte-bounded.
	if !req.ExcludeContext && len(req.SelectedSourceFiles) > 0 && s.flowCache != nil && doc != nil && doc.FilePath != "" {
		if sources, sErr := s.flowCache.ReadSourceFiles(doc, req.SelectedSourceFiles); sErr == nil && len(sources) > 0 {
			for name, content := range sources {
				sources[name] = scrubSourceText(ai.BoundSourceText(content, tokenBudget))
			}
			ctxReq.RawSourceFiles = sources
		}
	}

	sysPrompt, contextText := ai.BuildContext(ctxReq)
	// Scrubbed here, once per cache entry: every reusing turn used to scrub both texts again.
	return chatContextValue{scrubbedDoc: scrubbedDoc, sysPrompt: contextScrub(sysPrompt), contextText: contextScrub(contextText)}
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
	// The RAG guidelines ride in the user turn so the prompt cache covers the byte-identical prefix.
	if guidelines != "" {
		userContent = guidelines + "\n\n" + userContent
	}
	if !req.ExcludeContext && contextText != "" {
		userContent = "Context:\n" + contextText + "\n\n" + userContent
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

// PreviewContext returns what would be sent for a turn without a chat call.
func (s *ChatService) PreviewContext(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (res *models.ContextPreview, err error) {
	defer logger.Guard("App.PreviewContext", &err)
	ctx = withTurnMemo(ctx) // one org-role and settings read, as a turn makes

	// Same resolution as prepareTurn: the preview must accept exactly what the turn would.
	orgID := ""
	if doc != nil {
		orgID = doc.OrganizationID
	}
	ctx = ai.WithBudgetScope(ctx, scope, orgID) // the flow's org for the policy, as the worker seeds it
	azure, err := s.resolveAzureModel(ctx, scope, doc, req)
	if err != nil {
		return nil, err
	}
	provider, err := s.turnProvider(ctx, scope, orgID, req, azure)
	if err != nil {
		return nil, err
	}
	model := newTurnModel(req, azure)

	_, sysPrompt, contextText, guidelines := s.buildScrubbedContext(ctx, scope, provider, doc, report, req)
	scrubbedUserMsg := scrubber.ScrubText(req.UserMessage)
	if guidelines != "" {
		// Shown where they are sent: in the user turn, not the system prompt.
		scrubbedUserMsg = guidelines + "\n\n" + scrubbedUserMsg
	}

	total := provider.EstimateTokens(sysPrompt + contextText + scrubbedUserMsg)

	return &models.ContextPreview{
		SystemPrompt:    sysPrompt,
		ContextText:     contextText,
		UserMessage:     scrubbedUserMsg,
		EstimatedTokens: total,
		ContextLimit:    ai.ContextLimitFor(ctx, provider, model.resolved),
	}, nil
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
