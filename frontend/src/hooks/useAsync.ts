import {getErrorMessage} from '@/lib/getErrorMessage'
import {useState, useEffect, useRef, useCallback} from 'react'

interface UseAsyncResult<T> {
  data: T | null
  isLoading: boolean
  error: string | null
  setData: React.Dispatch<React.SetStateAction<T | null>>
  refetch: () => void
}

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
    // eslint-disable-next-line react-hooks/set-state-in-effect
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
        if (err instanceof DOMException && err.name === 'AbortError') return
        setError(getErrorMessage(err))
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
