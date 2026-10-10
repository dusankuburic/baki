package ai

import (
	"context"
	"sync"
)

// BudgetCallKind names the billed call a BudgetChecker is asked about.
type BudgetCallKind string

const (
	BudgetCallChat   BudgetCallKind = "chat"
	BudgetCallStream BudgetCallKind = "stream"
	BudgetCallEmbed  BudgetCallKind = "embed"
)

// BudgetCall is one billed provider call about to be made: who it is charged to (Scope, and OrgID when the
// spend lands on an organisation's budget), on which provider and model, and what kind of call.
type BudgetCall struct {
	Scope      string
	OrgID      string
	ProviderID string
	Model      string
	Kind       BudgetCallKind
}

// BudgetChecker is the daily AI budget gate the audited wrapper consults before every billed call (ADR 0028):
// a non-nil error refuses the call before it reaches the breaker, the retry policy or the provider. The error
// is returned to the caller unwrapped, so the checker's own sentinels keep classifying it.
type BudgetChecker interface {
	Check(ctx context.Context, call BudgetCall) error
}

// BudgetReasoner is implemented by a checker's refusals that can name their class for the refusal metric
// (pad_ai_budget_refusals_total): a closed set, "exceeded" or "unavailable".
type BudgetReasoner interface {
	BudgetReason() string
}

type budgetScopeKey struct{}

type budgetScope struct {
	scope, orgID string
}

// WithBudgetScope names who a chain's calls under ctx are charged to, for calls that carry no Request of
// their own (Embed): the knowledge base runs an org corpus's embeds under the org whatever key the chain
// resolved to, so the budget gate and the usage row agree. Absent, the wrapper falls back to the chain's own
// scope and the request's org, else the credential's org.
func WithBudgetScope(ctx context.Context, scope, orgID string) context.Context {
	return context.WithValue(ctx, budgetScopeKey{}, budgetScope{scope: scope, orgID: orgID})
}

// BudgetScopeFrom reads WithBudgetScope; ok is false when none was set.
func BudgetScopeFrom(ctx context.Context) (scope, orgID string, ok bool) {
	s, ok := ctx.Value(budgetScopeKey{}).(budgetScope)
	return s.scope, s.orgID, ok
}

type budgetMemoKey struct{}

// BudgetMemo memoises one request's daily-usage reads, keyed by (scope, org): a chat turn checks the budget
// at the start, once per tool-loop iteration and once per billed call in the hook, and each read is a SUM over
// usage_metrics. Errors memoise too (fail closed for the whole request). The checker's in-flight ledger covers
// the memo's staleness. Request-scoped, never shared across requests, so it needs no declared cache spec.
type BudgetMemo struct {
	mu    sync.Mutex
	reads map[budgetMemoRead]budgetMemoEntry
}

type budgetMemoRead struct {
	scope, orgID string
}

type budgetMemoEntry struct {
	usage float64
	err   error
}

// NewBudgetMemo is an empty memo.
func NewBudgetMemo() *BudgetMemo { return &BudgetMemo{} }

// Usage returns the memoised usage for (scope, orgID), calling read once on the first ask; a nil memo reads
// every time. Safe for concurrent use: a turn's RAG lookup and its stream share one.
func (m *BudgetMemo) Usage(scope, orgID string, read func() (float64, error)) (float64, error) {
	if m == nil {
		return read()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := budgetMemoRead{scope: scope, orgID: orgID}
	if e, ok := m.reads[key]; ok {
		return e.usage, e.err
	}
	usage, err := read()
	if m.reads == nil {
		m.reads = make(map[budgetMemoRead]budgetMemoEntry, 2)
	}
	m.reads[key] = budgetMemoEntry{usage: usage, err: err}
	return usage, err
}

// WithBudgetMemo attaches a fresh memo to ctx, keeping one already there (a knowledge-base search inside a
// chat turn shares the turn's).
func WithBudgetMemo(ctx context.Context) context.Context {
	if BudgetMemoFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, budgetMemoKey{}, NewBudgetMemo())
}

// BudgetMemoFrom reads the request's memo; nil when none was attached.
func BudgetMemoFrom(ctx context.Context) *BudgetMemo {
	m, _ := ctx.Value(budgetMemoKey{}).(*BudgetMemo)
	return m
}
