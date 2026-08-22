import {useState, useEffect, useRef, useCallback} from 'react'

interface UseAsyncResult<T> {
  data: T | null
  isLoading: boolean
  error: string | null
  // Exposed as the full setState dispatch so callers can apply functional
  // updaters (prev => next) — needed when two rapid mutations would otherwise
  // both close over the same stale snapshot (e.g. two session revokes before a
  // re-render). Plain value updates still work.
  setData: React.Dispatch<React.SetStateAction<T | null>>
  refetch: () => void
}

/**
 * Generic async-data-fetching hook that replaces the repeated
 * useCallback + useEffect + isLoading/error boilerplate.
 *
 * The fn is stored in a ref so it always runs the latest closure without
 * needing to be in the dependency array. Pass the values fn depends on
 * as `deps` — when they change, the effect re-runs.
 *
 * Cancellation: the current AbortSignal is passed to fn (optional second
 * parameter) so fetches can genuinely abort on unmount/dep-change instead of
 * merely discarding the late response. fn may ignore it — the boolean guard
 * still prevents stale state writes either way.
 */
export function useAsync<T>(fn: (signal: AbortSignal) => Promise<T>, deps: React.DependencyList): UseAsyncResult<T> {
  const [data, setData] = useState<T | null>(null)
  const [isLoading, setIsLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [nonce, setNonce] = useState(0)
  const fnRef = useRef(fn)
  useEffect(() => {
    fnRef.current = fn
  })

  useEffect(() => {
    const controller = new AbortController()
    let cancelled = false
    setIsLoading(true)
    setError(null)
    fnRef
      .current(controller.signal)
      .then(result => {
        if (cancelled) return
        setData(result)
        setIsLoading(false)
      })
      .catch(err => {
        if (cancelled) return
        // An abort (unmount / deps change) is intentional, not an error.
        if (err instanceof DOMException && err.name === 'AbortError') return
        setError(err instanceof Error ? err.message : String(err))
        setIsLoading(false)
      })
    return () => {
      cancelled = true
      controller.abort()
    }
  }, [...deps, nonce]) // eslint-disable-line react-hooks/exhaustive-deps

  const refetch = useCallback(() => setNonce(n => n + 1), [])

  return {data, isLoading, error, setData, refetch}
}
