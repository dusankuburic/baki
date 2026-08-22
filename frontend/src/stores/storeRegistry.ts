// Central registry for tearing down stores on logout.
//
// Each store registers a reset handler at module-init time via
// registerStoreReset(). resetAllStores() then runs them all, so authStore does
// NOT have to import every store, and a newly added store can't
// be forgotten at logout — it self-registers. A store that was never imported
// this session simply isn't registered, but it was also never instantiated, so
// it holds no state to leak.
//
// Handlers are isolated: one throwing or rejecting never aborts the others
// (mirrors the previous guard() behaviour). Any cross-step ordering a single
// store needs — e.g. presence must disconnect (which re-persists the sync queue)
// before that queue is discarded — lives inside that store's own handler, kept
// sequential there rather than relying on ordering across handlers.

import {logger} from '@/lib/logger'
import {clearRequestCache, abortAllRequests} from '@/api/client'

type ResetHandler = () => void | Promise<void>

const handlers = new Set<ResetHandler>()

export function registerStoreReset(handler: ResetHandler): void {
  handlers.add(handler)
}

export async function resetAllStores(): Promise<void> {
  // Drop the API client's GET micro-cache first: cached responses belong to
  // the outgoing session and must never be served to the next account.
  clearRequestCache()
  // Abort anything still in flight under the old session's token — a 300s
  // batch analysis or bulk upload should not keep running (and logging 401s
  // once the token is invalidated server-side) after logout.
  abortAllRequests()
  // Each handler is isolated so one failure can't abort the others, but the
  // error is LOGGED rather than silently swallowed — a logout-time failure
  // (e.g. presence failing to re-persist its sync queue) must not be
  // silently swallowed — it can mask data-loss-adjacent bugs.
  const run = async (fn: ResetHandler, i: number) => {
    try {
      await fn()
    } catch (err) {
      logger.error('store reset handler failed during logout (index', i, '):', err)
    }
  }
  await Promise.all([...handlers].map((fn, i) => run(fn, i)))
}
