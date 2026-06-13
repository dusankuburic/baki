import {useState, useEffect, useRef, useCallback} from 'react'

interface UseAsyncResult<T> {
  data: T | null
  isLoading: boolean
  error: string | null
  setData: (data: T | null) => void
  refetch: () => void
}

/**
 * Generic async-data-fetching hook that replaces the repeated
 * useCallback + useEffect + isLoading/error boilerplate.
 *
 * The fn is stored in a ref so it always runs the latest closure without
 * needing to be in the dependency array. Pass the values fn depends on
 * as `deps` — when they change, the effect re-runs.
 */
export function useAsync<T>(
  fn: () => Promise<T>,
  deps: React.DependencyList,
): UseAsyncResult<T> {
  const [data, setData] = useState<T | null>(null)
  const [isLoading, setIsLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [nonce, setNonce] = useState(0)
  const fnRef = useRef(fn)
  useEffect(() => { fnRef.current = fn })

  useEffect(() => {
    let cancelled = false
    setIsLoading(true)
    setError(null)
    fnRef.current()
      .then(result => {
        if (cancelled) return
        setData(result)
        setIsLoading(false)
      })
      .catch(err => {
        if (cancelled) return
        setError(err instanceof Error ? err.message : String(err))
        setIsLoading(false)
      })
    return () => { cancelled = true }
  }, [...deps, nonce]) // eslint-disable-line react-hooks/exhaustive-deps

  const refetch = useCallback(() => setNonce(n => n + 1), [])

  return {data, isLoading, error, setData, refetch}
}
