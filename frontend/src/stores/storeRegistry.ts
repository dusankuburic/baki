// Central registry for tearing down stores on logout.
//
// Each store registers a reset handler at module-init time via
// registerStoreReset(). resetAllStores() then runs them all, so authStore does
// NOT have to import every store (that eager fan-out is what previously forced
// the lazy import() workaround to dodge a cycle), and a newly added store can't
// be forgotten at logout — it self-registers. A store that was never imported
// this session simply isn't registered, but it was also never instantiated, so
// it holds no state to leak.
//
// Handlers are isolated: one throwing or rejecting never aborts the others
// (mirrors the previous guard() behaviour). Any cross-step ordering a single
// store needs — e.g. presence must disconnect (which re-persists the sync queue)
// before that queue is discarded — lives inside that store's own handler, kept
// sequential there rather than relying on ordering across handlers.

type ResetHandler = () => void | Promise<void>

const handlers = new Set<ResetHandler>()

export function registerStoreReset(handler: ResetHandler): void {
  handlers.add(handler)
}

export async function resetAllStores(): Promise<void> {
  const run = (fn: ResetHandler) =>
    Promise.resolve()
      .then(fn)
      .catch(() => {})
  await Promise.all([...handlers].map(run))
}
