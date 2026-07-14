package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/ai"
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
func (s *ChatService) chatContextKey(scope, flowID string, req models.ChatRequest, providerID, model string, report *models.AnalysisReport) string {
	gen := uint64(0)
	if v, ok := s.chatCtxGenCache().Get(context.Background(), flowID); ok {
		gen = v.(uint64)
	}
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
		scope, flowID, fmt.Sprintf("%d", gen),
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

// truncateForContextWindow estimates the total prompt tokens and, if the prompt
// exceeds the model's context window, drops the oldest turns until it fits. If
// the prompt still exceeds the limit after truncation (system prompt + the
// pinned first turn alone are too large), returns ErrContextLimit so the caller
// surfaces a clean "conversation too long" message instead of a raw provider
// 400. Mutates req.Messages in place.
//
// Truncation is turn-aware so it never produces a sequence a provider rejects:
// it keeps the first message (the conversation's initial user turn, which also
// guarantees a leading user role) plus the most-recent messages, and drops the
// oldest turns in between. Drops happen in whole turns — an assistant message
// and the tool_result messages that answered it travel together — so a
// tool_result is never separated from its tool_use (a 400), and the message
// following the pinned first turn is always an assistant (never a second
// consecutive user turn, also a 400). That malformed-sequence 400 is the very
// failure this guard exists to prevent, so the repair must not reintroduce it.
func truncateForContextWindow(provider ai.Provider, req *ai.Request, ctxLimit int) error {
	if ctxLimit <= 0 {
		return nil // unknown limit — can't enforce
	}
	estimate := func(msgs []ai.Message) int {
		total := provider.EstimateTokens(req.SystemPrompt)
		for _, m := range msgs {
			total += provider.EstimateTokens(m.Content)
		}
		return total
	}
	if estimate(req.Messages)+contextReserve <= ctxLimit {
		return nil // fits
	}
	if len(req.Messages) <= 1 {
		return ai.ErrContextLimit // nothing left to drop
	}

	head := req.Messages[:1] // pinned initial turn (kept as the leading user role)
	tail := req.Messages[1:]
	for {
		// Repair the head→tail junction: skip any leading messages that would be
		// invalid immediately after the pinned user turn — another user turn
		// (consecutive user roles) or an orphaned tool_result whose assistant
		// was already dropped. The next kept message must be an assistant turn.
		for len(tail) > 0 && tail[0].Role != "assistant" {
			tail = tail[1:]
		}
		req.Messages = append(append([]ai.Message{}, head...), tail...)
		if estimate(req.Messages)+contextReserve <= ctxLimit {
			return nil
		}
		if len(tail) == 0 {
			break // only the pinned turn remains and it still overflows
		}
		// Drop the oldest assistant turn: the assistant message plus the
		// tool_result messages that answered it.
		tail = tail[1:]
		for len(tail) > 0 && tail[0].Role == "tool" {
			tail = tail[1:]
		}
	}
	// Even the pinned first turn (+ system prompt) exceeds the window.
	return ai.ErrContextLimit
}

// PreviewContext returns what would be sent to the provider for a turn (system
// prompt, context text, user message, and an estimated/limit token pair) without
// actually issuing a chat call, so the UI can show the caller what's in-scope.
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
