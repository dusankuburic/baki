package service

import (
	"context"
	"fmt"
	"sync"
)

// DailyUsageReader reads the AI spend recorded today: the caller's personal total (orgID "") or the org's.
type DailyUsageReader interface {
	GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error)
}

// inFlightSpend is a running AI call whose cost is not yet a usage row: a chat stream's spend so far plus its
// reservation.
type inFlightSpend interface {
	spender() (scope, orgID string)
	inflight() float64
}

// AIBudget owns the daily AI budget: the settings' limits, today's recorded spend and the ledger of in-flight
// spend. Chat turns and the knowledge base's billed embedding runs consult one instance, so an embedding run
// sees what concurrent chat streams have committed (R-4). It is not ChatService's own because the chat service
// also needs the knowledge base: the two cannot take each other in their constructors.
type AIBudget struct {
	settings SettingsProvider
	usage    DailyUsageReader
	// ledger holds the running spends (inFlightSpend → struct{}); usage rows land only after each turn.
	ledger sync.Map
}

// NewAIBudget builds the budget gate. A nil settings provider means unlimited; a nil usage reader reads $0
// (no usage store: the desktop).
func NewAIBudget(settings SettingsProvider, usage DailyUsageReader) *AIBudget {
	return &AIBudget{settings: settings, usage: usage}
}

// hold enters a running spend in the ledger; release takes it out when the stream ends.
func (b *AIBudget) hold(e inFlightSpend)    { b.ledger.Store(e, struct{}{}) }
func (b *AIBudget) release(e inFlightSpend) { b.ledger.Delete(e) }

// CheckBudget reports whether the caller may spend on AI right now (orgID "" = personal): daily budget plus
// per-user sub-limit, in-flight included; errors are ErrAIBudgetExceeded or ErrAIBudgetUnavailable.
func (b *AIBudget) CheckBudget(ctx context.Context, scope, orgID string) error {
	return b.check(ctx, nil, scope, orgID, 0, nil)
}

// dailyUsage returns today's AI spend; (0, nil) without a usage reader. Callers must fail closed on error —
// treating it as 0 removes the guardrail.
func (b *AIBudget) dailyUsage(ctx context.Context, scope, orgID string) (float64, error) {
	if b.usage == nil {
		return 0, nil
	}
	usage, err := b.usage.GetDailyUsage(ctx, scope, orgID)
	if err != nil {
		return 0, err
	}
	return usage, nil
}

// turnBudgetUsage memoizes one tool loop's usage reads; errors cache too (fail closed).
type turnBudgetUsage struct {
	orgRead, userRead bool
	org               float64
	orgErr            error
	user              float64
	userErr           error
}

// read returns the memoized usage, reading the store once; a nil memo reads fresh.
func (t *turnBudgetUsage) read(b *AIBudget, ctx context.Context, scope, orgID string) (float64, error) {
	if t == nil {
		return b.dailyUsage(ctx, scope, orgID)
	}
	if orgID == "" {
		if !t.userRead {
			t.user, t.userErr = b.dailyUsage(ctx, scope, "")
			t.userRead = true
		}
		return t.user, t.userErr
	}
	if !t.orgRead {
		t.org, t.orgErr = b.dailyUsage(ctx, scope, orgID)
		t.orgRead = true
	}
	return t.org, t.orgErr
}

// check enforces the daily budget with an optional per-loop usage memo (the in-flight ledger covers its
// staleness gap); inflight is the caller's own not-yet-persisted spend and self its ledger entry, excluded from
// the others' sum (nil before registration).
func (b *AIBudget) check(ctx context.Context, memo *turnBudgetUsage, scope, orgID string, inflight float64, self inFlightSpend) error {
	if b.settings == nil {
		return nil // no settings provider wired (bare-struct tests/local) — unlimited
	}
	settings := b.settings.Get()
	if settings == nil || settings.AI.DailyBudget <= 0 {
		return nil
	}
	usage, err := memo.read(b, ctx, scope, orgID)
	if err != nil {
		return budgetUnavailable(scope, err)
	}
	// Concurrent streams' in-flight spend; usage rows land only after each turn.
	orgOthers, userOthers := b.othersInFlight(scope, orgID, self)
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

	// Org-scoped: also enforce the per-user sub-limit (orgID "" reads the personal-only total).
	if orgID != "" && settings.AI.PerUserDailyBudget > 0 {
		userUsage, err := memo.read(b, ctx, scope, "")
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

// othersInFlight sums the ledger except self: orgSum for orgID, userSum for scope.
func (b *AIBudget) othersInFlight(scope, orgID string, self inFlightSpend) (orgSum, userSum float64) {
	b.ledger.Range(func(k, _ any) bool {
		e, ok := k.(inFlightSpend)
		if !ok || e == self {
			return true
		}
		cost := e.inflight()
		if cost <= 0 {
			return true
		}
		owner, org := e.spender()
		if owner == scope {
			userSum += cost
		}
		if orgID != "" && org == orgID {
			orgSum += cost
		}
		return true
	})
	return orgSum, userSum
}
