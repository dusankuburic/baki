package service

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-core/ai/scrubber"
	"pad-core/logger"
	"pad-core/models"
)

// The outcomes of a turn's knowledge-base lookup, the label values of pad_chat_rag_lookups_total (R-observability-5).
const (
	ragOutcomeNonMember        = "non_member"
	ragOutcomeTrivial          = "trivial"
	ragOutcomeNoEmbedder       = "no_embedder"
	ragOutcomeIdentityMismatch = "identity_mismatch"
	ragOutcomeDeadline         = "deadline"
	ragOutcomeError            = "error"
	ragOutcomeMiss             = "miss"
	ragOutcomeHit              = "hit"
)

// ragLookupOutcomes lists every outcome, pre-initialised in the metric so a dashboard reads a zero, not no series.
func ragLookupOutcomes() []string {
	return []string{ragOutcomeNonMember, ragOutcomeTrivial, ragOutcomeNoEmbedder, ragOutcomeIdentityMismatch,
		ragOutcomeDeadline, ragOutcomeError, ragOutcomeMiss, ragOutcomeHit}
}

// noticeKnowledge is the kind of the journaled notice a turn emits, once and before its first chunk, when its
// knowledge-base lookup left the organisation's guidelines out for a reason the user or an admin can act on
// (R-knowledge-1): the status says which.
const noticeKnowledge = "knowledge"

// The statuses of a knowledge notice.
const (
	// knowledgeStatusIdentityMismatch: the member's embedding model sees none of the corpus (it was indexed with
	// another); a re-index, or the operator's PAD_EMBEDDING_PROVIDER pin, is the remedy.
	knowledgeStatusIdentityMismatch = ragOutcomeIdentityMismatch
	// knowledgeStatusNoEmbedder: the member has no embedding provider (no key of an embedding-capable provider, or
	// the policy refuses it).
	knowledgeStatusNoEmbedder = ragOutcomeNoEmbedder
	// knowledgeStatusDeadline: the lookup did not finish within PAD_RAG_DEADLINE_MS.
	knowledgeStatusDeadline = ragOutcomeDeadline
)

func knowledgeNotice(status string) map[string]interface{} {
	return map[string]interface{}{"kind": noticeKnowledge, "status": status}
}

// minRetrievalRunes is the shortest question a lookup embeds when nothing is selected: "ok", "thanks" or "is this
// ok?" match nothing a guideline says, yet each distinct one paid for an embedding and a vector query
// (R-knowledge-10).
const minRetrievalRunes = 12

// maxSelectionRules bounds the selected block's finding rule ids the retrieval query names.
const maxSelectionRules = 3

// ragFollowUpTimeout bounds a search's follow-up (an empty result's census), which runs after the turn has its
// guidelines, on a context of its own.
const ragFollowUpTimeout = 5 * time.Second

// ragWarnInterval is how often a lookup failure is logged per organisation: a member without an embedder fails
// every turn.
const ragWarnInterval = time.Minute

// ragLookup is what a turn's knowledge-base lookup delivers: the guidelines ("" for none) and, when the turn should
// say why it has none, the knowledge notice's status. followUp is the search's best-effort aftermath.
type ragLookup struct {
	guidelines string
	status     string
	followUp   func(context.Context)
}

// ragHandle is a turn's lookup running beside the context build: done closes once result is set.
type ragHandle struct {
	done   chan struct{}
	result ragLookup
}

// wait is the lookup's result once it is delivered; ok is false when ctx ended first. A nil handle (no knowledge
// base for the turn) has nothing to wait for.
func (h *ragHandle) wait(ctx context.Context) (ragLookup, bool) {
	if h == nil {
		return ragLookup{}, true
	}
	select {
	case <-h.done:
		return h.result, true
	case <-ctx.Done():
		return ragLookup{}, false
	}
}

// status is the delivered lookup's knowledge notice status; "" before delivery and without one.
func (h *ragHandle) status() string {
	if h == nil {
		return ""
	}
	select {
	case <-h.done:
		return h.result.status
	default:
		return ""
	}
}

// ragLookupDeadline is how long a turn waits for its guidelines: PAD_RAG_DEADLINE_MS (ChatOptions.RAGDeadline), else
// ragGuidelinesDeadline.
func (s *ChatService) ragLookupDeadline() time.Duration {
	if s.ragDeadline > 0 {
		return s.ragDeadline
	}
	return ragGuidelinesDeadline
}

// startRAGLookup starts the turn's knowledge-base lookup under the deadline, beside the work that does not need it;
// nil when the turn has no knowledge base (no service, no organisation). Once the result is delivered the search's
// follow-up runs on the same goroutine, off the turn's path.
func (s *ChatService) startRAGLookup(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) *ragHandle {
	if s.knowledge == nil || doc == nil || doc.OrganizationID == "" {
		return nil
	}
	h := &ragHandle{done: make(chan struct{})}
	ragCtx, ragCancel := context.WithTimeout(ctx, s.ragLookupDeadline())
	go func() {
		defer ragCancel()
		var l ragLookup
		func() {
			defer func() {
				if r := recover(); r != nil {
					// A panic here must not crash the process: deliver empty RAG guidelines.
					logger.FromContext(ctx).Error("rag guidelines lookup panicked", "scope", scope, "panic", r)
					l = ragLookup{}
				}
			}()
			l = s.ragGuidelines(ragCtx, scope, doc, report, req)
		}()
		h.result = ragLookup{guidelines: l.guidelines, status: l.status}
		close(h.done)
		runRAGFollowUp(ctx, l.followUp)
	}()
	return h
}

// runRAGFollowUp runs a search's follow-up best effort, bounded by ragFollowUpTimeout on a context detached from the
// turn's cancellation: the turn may well have ended by then.
func runRAGFollowUp(ctx context.Context, f func(context.Context)) {
	if f == nil {
		return
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ragFollowUpTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Error("rag search follow-up panicked", "panic", r)
		}
	}()
	f(fctx)
}

// ragGuidelines is the turn's knowledge-base lookup: the guidelines or "", counted under one outcome
// (ragLookupOutcomes) with each search phase's time. This RLS-exempt path gates org knowledge on MEMBERSHIP, fail
// closed. The retrieval query is the question plus a compact, scrubbed description of the selected block
// (retrievalSelection); a short question with nothing selected retrieves nothing.
func (s *ChatService) ragGuidelines(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) ragLookup {
	return s.ragGuidelinesFor(ctx, ragSourceTurn, scope, doc, report, req)
}

// ragGuidelinesFor is ragGuidelines counted under source: a turn's, or a context preview's.
func (s *ChatService) ragGuidelinesFor(ctx context.Context, source, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) ragLookup {
	if s.knowledge == nil || doc == nil || doc.OrganizationID == "" {
		return ragLookup{}
	}
	orgID := doc.OrganizationID
	if !s.isOrgMember(ctx, orgID, scope) {
		recordRAGLookup(source, ragOutcomeNonMember)
		return ragLookup{}
	}
	req, _ = resolveContextBlock(doc, req)
	selection := retrievalSelection(doc, s.currentReport(doc, report), req)
	question := strings.TrimSpace(req.UserMessage)
	if selection == "" && utf8.RuneCountInString(question) < minRetrievalRunes {
		recordRAGLookup(source, ragOutcomeTrivial)
		return ragLookup{}
	}
	query := question
	if selection != "" {
		query = strings.TrimSpace(question + "\n\n" + selection)
	}

	l, _, _ := s.searchOrgKnowledge(ctx, source, scope, orgID, query, 0)
	return l
}

// The sources of a knowledge-base lookup, the source label of pad_chat_rag_lookups_total: the guidelines a turn
// fetches before it starts, the model's search_knowledge calls (R-knowledge-6), and a context preview's lookup,
// which is not a turn.
const (
	ragSourceTurn    = "turn"
	ragSourceTool    = "tool"
	ragSourcePreview = "preview"
)

// searchOrgKnowledge runs one knowledge-base search of a member of orgID for query (at most limit chunks; 0 = a
// page) and counts it under source and the outcome it names; the guidelines are cut to ragGuidelinesTokenCap. The
// outcomes the user can act on carry the knowledge notice's status (the turn's lookup emits it; the tool does not).
// A lookup the caller's own end cut short (the turn stopped) names no outcome and counts nothing.
func (s *ChatService) searchOrgKnowledge(ctx context.Context, source, scope, orgID, query string, limit int) (l ragLookup, outcome string, err error) {
	res, err := s.knowledge.SearchLimited(ctx, scope, orgID, query, limit)
	recordRAGPhases(res.Phases)
	l = ragLookup{followUp: res.FollowUp()}
	switch {
	case err != nil && ctx.Err() != nil:
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return l, "", err // the turn itself ended: nothing to count or say
		}
		outcome, l.status = ragOutcomeDeadline, knowledgeStatusDeadline
	case errors.Is(err, rag.ErrEmbedderMismatch):
		outcome, l.status = ragOutcomeIdentityMismatch, knowledgeStatusIdentityMismatch
	case errors.Is(err, rag.ErrEmbeddingUnavailable), errors.Is(err, rag.ErrEmbeddingNotConfigured), errors.Is(err, ai.ErrProviderNotAllowed):
		outcome, l.status = ragOutcomeNoEmbedder, knowledgeStatusNoEmbedder
	case err != nil:
		outcome = ragOutcomeError
	}
	if outcome != "" {
		recordRAGLookup(source, outcome)
		if outcome != ragOutcomeDeadline && s.ragWarnAllowed(orgID) {
			// The org's guidelines were silently missing from every such turn before (R-knowledge-1).
			logger.FromContext(ctx).Warn("chat: the organisation's knowledge base could not be searched (logged once a minute per organisation)",
				"org_id", orgID, "source", source, "outcome", outcome, "error", err,
				"hint", "a member embeds with their own key unless the operator pins PAD_EMBEDDING_PROVIDER, which embeds on the organisation's shared key")
		}
		return l, outcome, err
	}
	l.guidelines = ai.TruncateToTokenLimit(res.Guidelines, ragGuidelinesTokenCap)
	outcome = ragOutcomeHit
	if l.guidelines == "" {
		outcome = ragOutcomeMiss
	}
	recordRAGLookup(source, outcome)
	return l, outcome, nil
}

// ragWarnAllowed reports whether a lookup failure of orgID may be logged now: once per ragWarnInterval per org.
func (s *ChatService) ragWarnAllowed(orgID string) bool {
	now := time.Now().UnixNano()
	v, _ := s.ragWarned.LoadOrStore(orgID, new(atomic.Int64))
	last := v.(*atomic.Int64)
	prev := last.Load()
	if prev != 0 && now-prev < int64(ragWarnInterval) {
		return false
	}
	return last.CompareAndSwap(prev, now)
}

// retrievalSelection is the compact description of the turn's selected block that joins the question in the
// retrieval query (R-knowledge-10): the action's display name and type, its subflow and up to maxSelectionRules
// rule ids of its findings, most severe first. It is built from the raw document, so it holds only what cannot carry
// the flow's data: the action type (an identifier the parser read), the display name when it is that type's own
// (an action's name is its type's last segment, split; a comment's or a condition's name is the user's text, which
// only the document scrubber can judge) and the analyzer's rule ids — never a property value. Scrubbed like the
// question; "" without a selected block that is still in the flow.
func retrievalSelection(doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) string {
	if req.ExcludeContext || req.ContextBlockID == "" || doc == nil || doc.BlocksByID == nil {
		return ""
	}
	block := doc.BlocksByID[req.ContextBlockID]
	if block == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Selected: ")
	kind := string(block.Type)
	if actionType.MatchString(block.RawType) {
		kind = block.RawType
	}
	if block.Type == models.BlockTypeAction && actionDisplayName(block.Name, block.RawType) {
		b.WriteString(block.Name + " (" + kind + ")")
	} else {
		b.WriteString(kind)
	}
	if sf := doc.BlockSubflow[block.ID]; sf != nil && sf.Name != "" {
		b.WriteString(", subflow " + sf.Name)
	}
	if rules := topFindingRules(report, block.ID); len(rules) > 0 {
		b.WriteString("; findings: " + strings.Join(rules, ", "))
	}
	return scrubber.ScrubFreeText(b.String())
}

// actionType is a PAD action type as the parser reads it: dotted identifiers (HTTPClient.InvokeUrl).
var actionType = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

// actionDisplayName reports whether name is the display name the parser derives from the action type rawType: its
// last segment with spaces at the case changes ("Invoke Url" for HTTPClient.InvokeUrl).
func actionDisplayName(name, rawType string) bool {
	if name == "" || !actionType.MatchString(rawType) {
		return false
	}
	last := rawType[strings.LastIndexByte(rawType, '.')+1:]
	return strings.ReplaceAll(name, " ", "") == last
}

// topFindingRules is up to maxSelectionRules distinct rule ids of the block's findings, the most severe first.
func topFindingRules(report *models.AnalysisReport, blockID string) []string {
	if report == nil {
		return nil
	}
	var findings []models.Finding
	for _, f := range report.Findings {
		if f.BlockID == blockID && f.RuleID != "" {
			findings = append(findings, f)
		}
	}
	severities := models.ValidSeverities()
	rank := func(s models.Severity) int {
		if i := slices.Index(severities, s); i >= 0 {
			return i
		}
		return len(severities)
	}
	slices.SortStableFunc(findings, func(a, c models.Finding) int { return rank(a.Severity) - rank(c.Severity) })
	var rules []string
	for _, f := range findings {
		if !slices.Contains(rules, f.RuleID) && len(rules) < maxSelectionRules {
			rules = append(rules, f.RuleID)
		}
	}
	return rules
}
