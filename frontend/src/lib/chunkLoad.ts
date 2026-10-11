
const CHUNK_LOAD_ERROR =
  /Failed to fetch dynamically imported module|error loading dynamically imported module|Importing a module script failed|Unable to preload CSS/i

export function isChunkLoadError(error: unknown): boolean {
  return error instanceof Error && CHUNK_LOAD_ERROR.test(error.message)
}

export function reloadPage(): void {
  window.location.reload()
}

const RELOAD_AT_KEY = 'baki.chunk-reload-at'

export const CHUNK_RELOAD_WINDOW_MS = 60_000

export function reloadForChunkError(reload: () => void = reloadPage, now: number = Date.now()): boolean {
  try {
    const last = Number(sessionStorage.getItem(RELOAD_AT_KEY))
    if (last && Math.abs(now - last) < CHUNK_RELOAD_WINDOW_MS) return false
    sessionStorage.setItem(RELOAD_AT_KEY, String(now))
  } catch {
    return false
  }
  reload()
  return true
}
