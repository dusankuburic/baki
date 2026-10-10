package service

import (
	"context"
	"fmt"
	"sync"

	"pad-analyzer/internal/ai"
)

// DailyUsageReader reads the AI spend recorded today: the caller's total (orgID "") or the org's. The user total
// sums every row of the user, org rows included (GetDailyUsage): the per-user sub-limit counts a member's org
// spend, and so does a personal budget.
type DailyUsageReader interface {
	GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error)
}

// inFlightSpend is a running AI call whose cost is not yet a usage row: a chat stream's spend so far plus its
// reservation.
type inFlightSpend interface {
	spender() (scope, orgID string)
	inflight() float64
}

// spendLedger holds the running spends (chat streams) between admission and their usage rows. memLedger is the
// one in-process ledger; the seam is for a shared one (Redis, Phase 5b): a ledger that cannot answer makes the
// check fail closed (ErrAIBudgetUnavailable), the same as an unreadable spend.
type spendLedger interface {
	hold(e inFlightSpend)
	release(e inFlightSpend)
	// others sums the ledger except self: orgSum for orgID, userSum for scope.
	others(scope, orgID string, self inFlightSpend) (orgSum, userSum float64, err error)
}

// memLedger is the in-process spendLedger: a set of inFlightSpend. It never errs.
type memLedger struct {
	entries sync.Map // inFlightSpend → struct{}
}

func (l *memLedger) hold(e inFlightSpend)    { l.entries.Store(e, struct{}{}) }
func (l *memLedger) release(e inFlightSpend) { l.entries.Delete(e) }

func (l *memLedger) others(scope, orgID string, self inFlightSpend) (orgSum, userSum float64, err error) {
	l.entries.Range(func(k, _ any) bool {
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
	return orgSum, userSum, nil
}

// AIBudget owns the daily AI budget: the settings' limits, today's recorded spend and the ledger of in-flight
// spend. Chat turns, the knowledge base's billed embedding runs and — through ai.BudgetChecker, in the audited
// wrapper — every billed provider call consult one instance, so an embedding run sees what concurrent chat
// streams have committed (R-4). It is not ChatService's own because the chat service also needs the knowledge
// base: the two cannot take each other in their constructors.
type AIBudget struct {
	settings SettingsProvider
	usage    DailyUsageReader
	ledger   spendLedger
}

// NewAIBudget builds the budget gate on the in-process ledger. A nil settings provider means unlimited; a nil
// usage reader reads $0 (no usage store: the desktop).
func NewAIBudget(settings SettingsProvider, usage DailyUsageReader) *AIBudget {
	return newAIBudgetWithLedger(settings, usage, &memLedger{})
}

// newAIBudgetWithLedger is NewAIBudget on a ledger of the caller's choosing (tests; the shared ledger of 5b).
func newAIBudgetWithLedger(settings SettingsProvider, usage DailyUsageReader, ledger spendLedger) *AIBudget {
	return &AIBudget{settings: settings, usage: usage, ledger: ledger}
}

// hold enters a running spend in the ledger; release takes it out when the stream ends.
func (b *AIBudget) hold(e inFlightSpend)    { b.ledger.hold(e) }
func (b *AIBudget) release(e inFlightSpend) { b.ledger.release(e) }

type streamSpendKey struct{}

// withStreamSpend names the calling stream on ctx, so the hook's check (Check) excludes the stream's own ledger
// entry, as the chat service's own checks do. Service-private: the wrapper only sees ai.BudgetChecker.
func withStreamSpend(ctx context.Context, ctl *streamCtl) context.Context {
	if ctl == nil {
		return ctx
	}
	return context.WithValue(ctx, streamSpendKey{}, ctl)
}

// streamSpendFrom reads withStreamSpend; a nil interface when none.
func streamSpendFrom(ctx context.Context) inFlightSpend {
	if ctl, ok := ctx.Value(streamSpendKey{}).(*streamCtl); ok && ctl != nil {
		return ctl
	}
	return nil
}

// Check is the ai.BudgetChecker the audited wrapper calls before every billed call (ADR 0028): the daily
// budget for the call's spender, in-flight included, on the request's usage memo when the context carries one
// (ai.WithBudgetMemo: a chat turn's start check, loop re-checks and hook checks share one read) and excluding
// the calling stream's own ledger entry (withStreamSpend). Its own spend so far is not added again here: the
// chat's checks count it; the hook is the net under them.
func (b *AIBudget) Check(ctx context.Context, call ai.BudgetCall) error {
	return b.check(ctx, ai.BudgetMemoFrom(ctx), call.Scope, call.OrgID, 0, streamSpendFrom(ctx))
}

// CheckBudget reports whether the caller may spend on AI right now (orgID "" = personal): daily budget plus
// per-user sub-limit, in-flight included; errors are ErrAIBudgetExceeded or ErrAIBudgetUnavailable. Reads
// through the context's usage memo when one is attached (a knowledge-base run), fresh otherwise.
func (b *AIBudget) CheckBudget(ctx context.Context, scope, orgID string) error {
	return b.check(ctx, ai.BudgetMemoFrom(ctx), scope, orgID, 0, nil)
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

// memoUsage is dailyUsage through memo (nil reads fresh); errors memoise too (fail closed).
func (b *AIBudget) memoUsage(ctx context.Context, memo *ai.BudgetMemo, scope, orgID string) (float64, error) {
	return memo.Usage(scope, orgID, func() (float64, error) { return b.dailyUsage(ctx, scope, orgID) })
}

// check enforces the daily budget with an optional per-request usage memo (the in-flight ledger covers its
// staleness gap); inflight is the caller's own not-yet-persisted spend and self its ledger entry, excluded from
// the others' sum (nil before registration).
func (b *AIBudget) check(ctx context.Context, memo *ai.BudgetMemo, scope, orgID string, inflight float64, self inFlightSpend) error {
	if b.settings == nil {
		return nil // no settings provider wired (bare-struct tests/local) — unlimited
	}
	settings := b.settings.Get()
	if settings == nil || settings.AI.DailyBudget <= 0 {
		return nil
	}
	usage, err := b.memoUsage(ctx, memo, scope, orgID)
	if err != nil {
		return budgetUnavailable(scope, err)
	}
	// Concurrent streams' in-flight spend; usage rows land only after each turn.
	orgOthers, userOthers, err := b.ledger.others(scope, orgID, self)
	if err != nil {
		return budgetUnavailable(scope, fmt.Errorf("in-flight spend ledger: %w", err))
	}
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

	// Org-scoped: also enforce the per-user sub-limit (orgID "" reads the user's own total).
	if orgID != "" && settings.AI.PerUserDailyBudget > 0 {
		userUsage, err := b.memoUsage(ctx, memo, scope, "")
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

// othersInFlight sums the ledger except self: orgSum for orgID, userSum for scope (tests).
func (b *AIBudget) othersInFlight(scope, orgID string, self inFlightSpend) (orgSum, userSum float64) {
	orgSum, userSum, _ = b.ledger.others(scope, orgID, self)
	return orgSum, userSum
}
