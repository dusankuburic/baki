// Package txhook defers work until the transaction it follows has committed: deleting the blobs behind a removed
// row, emailing an invite, forgetting a cached credential. Done earlier, such work can describe a write that is
// then rolled back.
//
// The owner of a transaction (the RLS middleware for a request, withTx for a storage call that opened its own)
// installs a Registry with With and later commits or aborts it. Everyone else only calls Register, from any
// layer: the package knows nothing about databases, and what context a hook runs with is the owner's choice
// (a Runner).
package txhook

import (
	"context"
	"sync"

	"pad-core/logger"
)

// Hook is work to run after the commit. Its ctx carries no cancellation from the request and is only valid until
// the hook returns: work the hook hands to a background runner must not keep it.
type Hook func(ctx context.Context)

// Runner runs one hook with the context its owner chose. The RLS middleware gives each hook a fresh
// row-level-security scope for the same user (storage/database.RLSHookRunner), so database access from a hook
// stays scoped to that user.
type Runner func(Hook)

type state uint8

const (
	pending state = iota
	committed
	aborted
)

// Registry collects the hooks registered while its transaction is open.
type Registry struct {
	mu     sync.Mutex
	state  state
	hooks  []Hook
	runner Runner
}

type ctxKey struct{}

// With returns ctx carrying a fresh registry, and the registry, for its owner to Commit or Abort.
func With(ctx context.Context) (context.Context, *Registry) {
	r := &Registry{}
	return context.WithValue(ctx, ctxKey{}, r), r
}

func registryOf(ctx context.Context) *Registry {
	r, _ := ctx.Value(ctxKey{}).(*Registry)
	return r
}

// Register defers fn until the transaction owning ctx's registry commits:
//   - registry pending: fn waits for Commit;
//   - registry committed: fn runs now, the write being durable;
//   - registry aborted: fn is dropped and logged, the write it follows having been rolled back;
//   - no registry: fn runs now (autocommit: the write is already durable), on ctx without its cancellation.
func Register(ctx context.Context, fn Hook) {
	r := registryOf(ctx)
	if r == nil {
		runHook(Detached(ctx), fn)
		return
	}
	r.mu.Lock()
	switch r.state {
	case pending:
		r.hooks = append(r.hooks, fn)
		r.mu.Unlock()
	case committed:
		runner := r.runner
		r.mu.Unlock()
		runHook(runner, fn)
	default:
		r.mu.Unlock()
		logger.FromContext(ctx).Warn("post-commit work dropped: its transaction already rolled back")
	}
}

// Has reports whether ctx carries a registry that has not been aborted, i.e. whether Register is certain to run
// fn only after a commit. Work that is unsafe before the commit (deleting a blob the row may still point at)
// skips registration when this is false and the caller is inside an uncommitted transaction.
func Has(ctx context.Context) bool {
	r := registryOf(ctx)
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state != aborted
}

// Commit marks the transaction committed and runs the registered hooks in order through runner (nil: Detached
// over a background context); hooks registered afterwards run at once. A panicking hook is reported like any
// recovered panic and does not stop the others. A second Commit, or one after Abort, does nothing.
func (r *Registry) Commit(runner Runner) {
	if runner == nil {
		runner = Detached(context.Background())
	}
	r.mu.Lock()
	if r.state != pending {
		r.mu.Unlock()
		return
	}
	r.state = committed
	r.runner = runner
	hooks := r.hooks
	r.hooks = nil
	r.mu.Unlock()
	for _, fn := range hooks {
		runHook(runner, fn)
	}
}

// Abort marks the transaction rolled back: the registered hooks are dropped, and so is anything registered
// later. Abort after Commit does nothing.
func (r *Registry) Abort() {
	r.mu.Lock()
	if r.state != pending {
		r.mu.Unlock()
		return
	}
	r.state = aborted
	dropped := len(r.hooks)
	r.hooks = nil
	r.mu.Unlock()
	if dropped > 0 {
		logger.Info("post-commit work dropped: the transaction rolled back", "hooks", dropped)
	}
}

// Detached is the Runner for an owner with no scope to restore: each hook gets base without its cancellation.
func Detached(base context.Context) Runner {
	return func(fn Hook) { fn(context.WithoutCancel(base)) }
}

func runHook(runner Runner, fn Hook) {
	defer logger.GuardRecover("txhook")
	runner(fn)
}
