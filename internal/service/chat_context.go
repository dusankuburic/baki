package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/ai/scrubber"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"
)

// maxChatContextCache bounds the cached scrubbed-context entries. Each entry
// holds a scrubbed (redacted) flow clone plus the built system/context text;
// the cost is dominated by the deep clone + per-block scrub walk + token
// estimation in BuildContext, none of which change across consecutive turns in
// the same thread. A small per-scope bound covers a few concurrent threads.
const maxChatContextCache = 32

// maxChatCtxGen bounds the per-flow generation-counter cache (see chatCtxGen).
// Larger than maxChatContextCache: counters are a few bytes each (vs. a whole
// scrubbed document+context), so keeping more of them further reduces the
// already-negligible chance that a generation number gets reused while a
// stale chatCtxCache entry keyed with that same number is still resident.
const maxChatCtxGen = 256

// ragGuidelinesDeadline caps how long the streaming chat path waits for RAG
// knowledge-base guidelines before first token. RAG issues a synchronous
// embedding API call + vector query; on a slow embedding provider this would
// otherwise gate the whole turn. On timeout the turn proceeds without
// guidelines (skip-on-timeout) rather than stalling the user.
const ragGuidelinesDeadline = 800 * time.Millisecond

// ragGuidelinesTokenCap bounds the guidelines block appended to the system
// prompt: the assembly emits up to 5 retrieved chunks (~1.5k tokens) and a
// verbose knowledge base could crowd the prompt's own budget. Truncation cuts
// from the tail (whole guidelines first, then per-chunk), keeping the header
// and the highest-ranked chunks intact.
const ragGuidelinesTokenCap = 1200

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

// newChatContextCache builds the bounded LRU for scrubbed-context memoisation.
// size > 0 ⇒ the constructor error is impossible, so it is swallowed.
func newChatContextCache() cache.Cache {
	c, _ := cache.NewLRUCache(maxChatContextCache)
	return c
}

// newChatCtxGenCache builds the bounded LRU for the per-flow generation
// counters. size > 0 ⇒ the constructor error is impossible, so it is swallowed.
func newChatCtxGenCache() cache.Cache {
	c, _ := cache.NewLRUCache(maxChatCtxGen)
	return c
}

// chatCtxGenCache lazily initializes chatCtxGen on first use, so a ChatService
// built as a bare struct literal (common in tests) still gets a working bounded
// cache instead of a nil interface.
func (s *ChatService) chatCtxGenCache() cache.Cache {
	s.chatCtxGenOnce.Do(func() {
		if s.chatCtxGen == nil {
			s.chatCtxGen = newChatCtxGenCache()
		}
	})
	return s.chatCtxGen
}

// InvalidateChatContext drops any cached scrubbed context for a flow. Call on
// in-place content updates (mirrors FlowService.InvalidateSearchIndex). It
// bumps a per-flow generation counter rather than enumerating keys; stale
// entries become unreachable (different key) and age out via the LRU bound.
func (s *ChatService) InvalidateChatContext(flowID string) {
	if s.chatCtxCache == nil {
		return
	}
	genCache := s.chatCtxGenCache()
	ctx := context.Background()
	// Serialize the read-modify-write so two concurrent invalidations can't
	// both read gen=N and both write N+1 (losing an increment → stale context).
	s.chatCtxGenMu.Lock()
	defer s.chatCtxGenMu.Unlock()
	gen := uint64(0)
	if v, ok := genCache.Get(ctx, flowID); ok {
		gen = v.(uint64)
	}
	genCache.Set(ctx, flowID, gen+1, 0)
}

// chatContextKey builds the cache key for a turn's scrubbed context. It mixes
// every input to the turn-invariant core of buildScrubbedContext: the owning
// scope (authz isolation), the flow's edit-generation, the selected block /
// system-prompt suffix, the provider+model (token math differs), and a cheap
// fingerprint of the analysis report (regenerated → GeneratedAt moves).
func (s *ChatService) chatContextKey(scope, flowID string, req models.ChatRequest, providerID, model string, report *models.AnalysisReport, sourceFP string) string {
	gen := uint64(0)
	if v, ok := s.chatCtxGenCache().Get(context.Background(), flowID); ok {
		gen = v.(uint64)
	}
	reportFP := ""
	if report != nil {
		reportFP = fmt.Sprintf("%d-%d", report.GeneratedAt.UnixNano(), len(report.Findings))
	}
	// sourceFP (selected source files + their size/mtime, precomputed where
	// the doc's directory is known) keys a selection AND any on-disk change to
	// it — see sourceFilesFingerprint.
	// ExcludeContext changes what computeContextCore produces (it gates both the
	// SelectedBlock and RawSourceFiles context injection), so it MUST be in the
	// key — otherwise a free-form turn caches a source/block-less contextText
	// that a later context-bearing turn (same key) would reuse.
	exclude := fmt.Sprintf("%t", req.ExcludeContext)
	// req.SystemPrompt holds the MERGED suffix (settings + request, see
	// cachedContextCore), which is what reaches the prompt — so editing the
	// custom-instructions setting cannot serve a context built under the old
	// instructions.
	return strings.Join([]string{
		scope, flowID, fmt.Sprintf("%d", gen),
		req.ContextBlockID, req.SystemPrompt, providerID, model, reportFP, sourceFP, exclude,
	}, "|")
}

// sourceFilesFingerprint reduces the selected-source-files list to a stable
// cache-key fragment (sorted names joined by ";"). Contents aren't hashed — a
// flow reload bumps the per-flow generation in the key, covering on-disk edits.
// sourceFilesFingerprint reduces the selection to a stable cache key. Names
// alone under-keyed it: a secondary source file edited ON DISK (without a
// main-flow reload bumping the generation counter) re-served the cached raw
// copy until the flow itself changed — so the fingerprint now folds in each
// file's size+mtime. Stat failures degrade to the name alone (the file is
// unreadable anyway, so computeContextCore's read will skip it).
func sourceFilesFingerprint(dir string, files []string) string {
	if len(files) == 0 {
		return ""
	}
	cp := make([]string, len(files))
	copy(cp, files)
	sort.Strings(cp)
	for i, name := range cp {
		// Names are RELATIVE to the flow's directory (ReadSourceFiles joins
		// them there); stat against the same base or every lookup misses.
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			cp[i] = fmt.Sprintf("%s:%d:%d", name, info.Size(), info.ModTime().UnixNano())
		}
	}
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

// buildScrubbedContext scrubs the document, builds the system prompt and
// context text, looks up the turn's RAG knowledge-base guidelines, and scrubs
// all three strings. It also returns the scrubbed document so the caller can
// hand it to the tool loop. Shared with PreviewContext so the two
// context-preparation paths cannot drift apart. The guidelines are returned
// separately: they belong to the final user message (buildMessages), not the
// system prompt, which must stay byte-stable across turns for the providers'
// automatic prompt caching (PERF-8).
//
// This is the SYNCHRONOUS path (RAG blocks). The streaming worker instead uses
// cachedContextCore + a concurrent, deadline-bounded ragGuidelines call so a
// slow embedding provider cannot gate first token (C-3).
func (s *ChatService) buildScrubbedContext(ctx context.Context, scope string, provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (scrubbedDoc *models.FlowDocument, sysPrompt, contextText, guidelines string) {
	cv := s.cachedContextCore(ctx, scope, provider, doc, report, req)
	guidelines = s.ragGuidelines(ctx, scope, doc, req.UserMessage)
	return cv.scrubbedDoc, scrubber.ScrubText(cv.sysPrompt), scrubber.ScrubText(cv.contextText), scrubber.ScrubText(guidelines)
}

// ragGuidelines returns the RAG knowledge-base guidelines for a turn, or "" if
// the knowledge service is unset, the flow has no org, or the search fails. The
// caller controls the deadline via the passed context (the streaming worker
// uses a short timeout so a slow embedding provider skips guidelines for the
// turn instead of stalling first token).
//
// S2: org-scoped knowledge requires org MEMBERSHIP. A user can hold access to
// an org-owned flow through collaboration without being an org member (the
// flows policy allows it) — the knowledge REST API would refuse them
// (requireMember), but this chat path is RLS-exempt, so the gate is explicit
// here. Fails closed: no membership proof, no guidelines.
func (s *ChatService) ragGuidelines(ctx context.Context, scope string, doc *models.FlowDocument, userMessage string) string {
	if s.knowledge == nil || doc == nil || doc.OrganizationID == "" {
		return ""
	}
	if !s.isOrgMember(ctx, doc.OrganizationID, scope) {
		metrics.RecordRAGLookup("skipped")
		return ""
	}
	guidelines, err := s.knowledge.Search(ctx, scope, doc.OrganizationID, userMessage)
	guidelines = ai.TruncateToTokenLimit(guidelines, ragGuidelinesTokenCap)
	switch {
	case err != nil:
		if ctx.Err() == nil { // deadline skips are expected under load, not errors
			metrics.RecordRAGLookup("error")
		} else {
			metrics.RecordRAGLookup("skipped")
		}
	case guidelines == "":
		metrics.RecordRAGLookup("miss")
	default:
		metrics.RecordRAGLookup("hit")
	}
	if err != nil || guidelines == "" {
		return ""
	}
	return guidelines
}

// isOrgMember reports whether userID is a member of orgID, per the backend's
// org store. scope is the caller's identity ("" in local mode, where orgs
// don't exist — and an org-owned flow can't be open — so the empty case fails
// closed without a store round-trip). A store error also fails closed: a
// missing membership proof must never widen knowledge access.
//
// The lookup is a local narrow interface (not part of chatStore) so the
// chat-store surface stays minimal: both real backends satisfy it, and any
// test stub without it simply fails closed.
func (s *ChatService) isOrgMember(ctx context.Context, orgID, userID string) bool {
	if s.backend == nil || orgID == "" || userID == "" {
		return false
	}
	orgs, ok := s.backend.(interface {
		ListOrgsForUser(ctx context.Context, userID string) ([]*interfaces.Organisation, error)
	})
	if !ok {
		return false
	}
	list, err := orgs.ListOrgsForUser(ctx, userID)
	if err != nil {
		logger.Warn("rag guidelines: org membership lookup failed; skipping guidelines", "error", err)
		return false
	}
	for _, o := range list {
		if o != nil && o.ID == orgID {
			return true
		}
	}
	return false
}

// cachedContextCore returns the memoised scrub+BuildContext result, computing
// and storing it on a miss. The cache is bypassed when there is no document
// (nothing to key on) or when the cache is unset (e.g. a test-built service).
func (s *ChatService) cachedContextCore(ctx context.Context, scope string, provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) chatContextValue {
	// Resolve the caller's custom instructions once, here where the request
	// context (and so the caller) is known; the key and the computation below
	// both read the merged value from req.SystemPrompt.
	req.SystemPrompt = s.systemPromptSuffix(ctx, req)
	if s.chatCtxCache == nil || doc == nil {
		return s.computeContextCore(provider, doc, report, req)
	}
	key := s.chatContextKey(scope, doc.ID, req, provider.ID(), req.Model, report,
		sourceFilesFingerprint(filepath.Dir(doc.FilePath), req.SelectedSourceFiles))
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
		logger.Error("Failed to scrub document", "error", err)
		scrubbedDoc = doc
	}

	// Read the per-provider context token budget from settings instead of
	// hardcoding 4000. Falls back to 4000 if unset/zero (the default in
	// DefaultSettings matches, so this is backward-compatible).
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
	//
	// S1: each file is AST-scrubbed with ScrubSourceText before injection. The
	// trailing ScrubText over the assembled context is regex-only and PAD's
	// `SET Password TO $'''secret'''` / `Text: $'''secret'''` syntax matches no
	// key=value pattern — raw disk bytes used to reach the model verbatim,
	// bypassing the property-name masking the document path gets.
	if !req.ExcludeContext && len(req.SelectedSourceFiles) > 0 && s.flowCache != nil && doc != nil && doc.FilePath != "" {
		if sources, sErr := s.flowCache.ReadSourceFiles(doc, req.SelectedSourceFiles); sErr == nil && len(sources) > 0 {
			for name, content := range sources {
				sources[name] = scrubber.ScrubSourceText(content)
			}
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
func buildMessages(req models.ChatRequest, contextText, guidelines string) []ai.Message {
	messages := make([]ai.Message, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = ai.Message{Role: m.Role, Content: scrubber.ScrubText(m.Content)}
	}
	userContent := scrubber.ScrubText(req.UserMessage)
	// The per-turn RAG guidelines ride in the user turn (included regardless
	// of ExcludeContext, as they always were) so the system prompt and the
	// history prefix stay byte-identical across turns — the providers'
	// automatic prompt cache covers everything before this message (PERF-8).
	if guidelines != "" {
		userContent = guidelines + "\n\n" + userContent
	}
	if !req.ExcludeContext && contextText != "" {
		userContent = "Context:\n" + contextText + "\n\n" + userContent
	}
	return append(messages, ai.Message{Role: "user", Content: userContent})
}

// promptTokenCeiling is a tokenizer-free upper bound on the prompt's token
// count: every estimator in use (the cl100k BPE, or the rune heuristic, whose
// CJK runes are three bytes each) yields at most one token per byte, so a
// prompt whose byte count fits the window fits in tokens too and the tiktoken
// pass — ~100 ms and ~50 MB of garbage per 400 KB, run again on every tool
// iteration — can be skipped. The per-message and per-tool framing matches
// truncateForContextWindow's own estimate.
func promptTokenCeiling(req *ai.Request) int {
	n := len(req.SystemPrompt)
	for _, td := range req.Tools {
		n += len(td.InputSchema) + 8
	}
	for _, m := range req.Messages {
		n += len(m.Content) + 4
		for _, tc := range m.ToolCalls {
			n += len(tc.Input)
		}
	}
	return n
}

// truncateForContextWindow estimates the total prompt tokens and, if the prompt
// exceeds the model's context window, drops turns until it fits. If the prompt
// still exceeds the limit after truncation (system prompt + the current turn
// alone are too large), returns ErrContextLimit so the caller surfaces a clean
// "conversation too long" message instead of a raw provider 400. Mutates
// req.Messages in place.
//
// pin is the index of the CURRENT turn — the user message the caller built
// for this request (history + merged flow context + question), which is never
// dropped; an out-of-range pin means the last message. The returned int is
// the pin's index in the truncated list, so a tool loop that appends its
// exchanges after the turn can pass it back on the next iteration.
//
// Drop order: the oldest history turns before the pin first (the kept prefix
// always starts with a user role, so no provider sees two consecutive user
// turns), then the oldest exchange after the pin — an assistant message and
// the results that answered it, as one unit, so a tool_result is never
// separated from its tool_use (a 400). The previous form pinned messages[0]
// instead: on the server-reconstructed path that is the thread's FIRST stored
// question, and once the history between it and the current turn was gone
// the "junction repair" deleted the current turn itself — the provider
// re-answered the thread's first question, or received tool results for a
// question it could no longer see.
//
// The reserve is the request's own completion cap when it exceeds the default
// (providers reject prompt + max_tokens > window), and the prompt is checked
// in bytes first (promptTokenCeiling) so the common case never tokenises.
func truncateForContextWindow(provider ai.Provider, req *ai.Request, ctxLimit int, pin int) (int, error) {
	if ctxLimit <= 0 || len(req.Messages) == 0 {
		return pin, nil // unknown limit — can't enforce; nothing to truncate
	}
	msgs := req.Messages
	if pin < 0 || pin >= len(msgs) {
		pin = len(msgs) - 1 // the current turn is the last message the caller built
	}
	reserve := max(contextReserve, req.MaxTokens)
	if promptTokenCeiling(req)+reserve <= ctxLimit {
		return pin, nil // fits in bytes, so it fits in tokens — no tiktoken pass
	}
	// Fixed component (system prompt + tool schemas) — constant across the
	// drop loop, precomputed once (B1.4).
	fixed := provider.EstimateTokens(req.SystemPrompt)
	for _, td := range req.Tools {
		fixed += provider.EstimateTokens(string(td.InputSchema)) + 8
	}
	// Per-message costs + SUFFIX SUMS: suffix[i] = tokens of messages[i:].
	// The old loop re-estimated every remaining message per drop — O(n²)
	// token scanning in the per-turn hot path on long histories.
	msgCost := func(m ai.Message) int {
		// Per-message framing (~4 tokens) + each assistant tool_use's
		// argument JSON verbatim (R4b undercount fix).
		c := provider.EstimateTokens(m.Content) + 4
		for _, tc := range m.ToolCalls {
			c += provider.EstimateTokens(string(tc.Input))
		}
		return c
	}
	suffix := make([]int, len(msgs)+1)
	for i := len(msgs) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + msgCost(msgs[i])
	}
	curCost := suffix[pin] - suffix[pin+1]
	// b: first kept history index; a: first kept post-turn exchange index.
	b, a := 0, pin+1
	fits := func() bool {
		return fixed+(suffix[b]-suffix[pin])+curCost+suffix[a]+reserve <= ctxLimit
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
	if !fits() {
		// Even the current turn (+ system prompt and tools) exceeds the window.
		return pin, ai.ErrContextLimit
	}
	if b == 0 && a == pin+1 {
		return pin, nil
	}
	kept := make([]ai.Message, 0, (pin-b)+1+(len(msgs)-a))
	kept = append(kept, msgs[b:pin]...)
	kept = append(kept, msgs[pin])
	kept = append(kept, msgs[a:]...)
	req.Messages = kept
	return pin - b, nil
}

// PreviewContext returns what would be sent to the provider for a turn (system
// prompt, context text, user message, and an estimated/limit token pair) without
// actually issuing a chat call, so the UI can show the caller what's in-scope.
func (s *ChatService) PreviewContext(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (res *models.ContextPreview, err error) {
	defer logger.Guard("App.PreviewContext", &err)

	// Same resolution as prepareTurn: an org flow may run on the org's
	// shared credential (G7), and the preview must not refuse what the turn
	// would accept.
	orgID := ""
	if doc != nil {
		orgID = doc.OrganizationID
	}
	provider, err := s.factory.ForOrg(scope, orgID, req.Provider)
	if err != nil {
		return nil, err
	}

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
		ContextLimit:    provider.ContextLimit(),
	}, nil
}

// systemPromptSuffix merges the caller's "custom instructions" setting
// (Settings → AI Behavior) with any per-request override. The setting used to
// be persisted and never read — the only consumer was the per-request field,
// which the frontend never populated — so the control was inert. Merging here,
// server-side, keeps it tamper-proof: a client cannot drop the configured
// instructions by omitting the field. The setting comes from
// callerPreferences, so in cloud mode it is the user's own, not the server's
// settings file.
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
