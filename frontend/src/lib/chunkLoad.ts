// Recovery for lazy chunks that failed to download.
//
// A failed chunk cannot be retried in place. The browser keeps the failed
// module in its module map for the life of the document, so importing it
// again, or any chunk that imports it (even under a cache-busting query),
// fails without a new request, and React.lazy keeps the rejected promise as
// well. Only a reload recovers. Two causes land here: a transient network
// failure (public/sw.js retries those before the page sees them) and a
// redeploy that removed the hashed chunk an open tab still points at.

// Chromium, Firefox and Safari phrasings, plus Vite's CSS preload failure.
const CHUNK_LOAD_ERROR =
  /Failed to fetch dynamically imported module|error loading dynamically imported module|Importing a module script failed|Unable to preload CSS/i

export function isChunkLoadError(error: unknown): boolean {
  return error instanceof Error && CHUNK_LOAD_ERROR.test(error.message)
}

export function reloadPage(): void {
  window.location.reload()
}

const RELOAD_AT_KEY = 'baki.chunk-reload-at'

// At most one automatic reload per window: a chunk that is broken in the
// deployed build fails again right after the reload and then shows the error
// fallback instead of reload-looping. Unlike a one-shot flag, the window lets a
// long-lived tab recover from a later failure too.
export const CHUNK_RELOAD_WINDOW_MS = 60_000

/** Reloads the page unless an automatic reload already ran within the window; returns whether it did. */
export function reloadForChunkError(reload: () => void = reloadPage, now: number = Date.now()): boolean {
  try {
    const last = Number(sessionStorage.getItem(RELOAD_AT_KEY))
    if (last && Math.abs(now - last) < CHUNK_RELOAD_WINDOW_MS) return false
    sessionStorage.setItem(RELOAD_AT_KEY, String(now))
  } catch {
    // Without storage there is no loop guard, so never reload automatically.
    return false
  }
  reload()
  return true
}
