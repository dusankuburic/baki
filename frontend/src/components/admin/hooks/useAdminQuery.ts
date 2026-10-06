import {getErrorMessage} from '@/lib/getErrorMessage'
import {useEffect, useRef, useState} from 'react'
import {useAdminStore} from '@/stores/adminStore'
import {logger} from '@/lib/logger'

interface Loaded<T> {
  key: string
  data: T | null
  error: string | null
}

export interface AdminQueryResult<T> {
  data: T | null
  error: string | null
  /** No data yet: show a skeleton. */
  isLoading: boolean
  /** New data on its way while the previous page stays on screen. */
  isRefetching: boolean
  retry: () => void
}

// The load cycle every admin section shares. `key` names what is on screen
// (filters, page, refresh nonces); when it changes the request it supersedes
// is aborted and the previous payload stays visible (isRefetching) until the
// new one lands. A failure after data was shown keeps that data and reports
// through `onRefetchError` (a toast); a failure with nothing to show is left to
// the section's inline notice. In-flight requests and the last successful
// load are mirrored into the admin store for the page header.
export function useAdminQuery<T>(
  key: string,
  fetcher: (signal: AbortSignal) => Promise<T>,
  onRefetchError?: (message: string) => void,
): AdminQueryResult<T> {
  const [loaded, setLoaded] = useState<Loaded<T>>({key: '', data: null, error: null})
  const [attempt, setAttempt] = useState(0)
  const fetcherRef = useRef(fetcher)
  const onErrorRef = useRef(onRefetchError)
  const dataRef = useRef<T | null>(null)
  useEffect(() => {
    fetcherRef.current = fetcher
    onErrorRef.current = onRefetchError
  })
  useEffect(() => {
    dataRef.current = loaded.data
  }, [loaded.data])

  const requestKey = `${key}#${attempt}`
  useEffect(() => {
    const controller = new AbortController()
    const {beginLoad, endLoad, markUpdated} = useAdminStore.getState()
    beginLoad()
    fetcherRef
      .current(controller.signal)
      .then(d => {
        if (controller.signal.aborted) return
        markUpdated()
        setLoaded({key: requestKey, data: d, error: null})
      })
      .catch((e: unknown) => {
        if (controller.signal.aborted || (e instanceof Error && e.name === 'AbortError')) return
        logger.error('admin: load failed', e)
        const message = getErrorMessage(e)
        if (dataRef.current !== null) onErrorRef.current?.(message)
        setLoaded({key: requestKey, data: dataRef.current, error: message})
      })
      .finally(endLoad)
    return () => controller.abort()
  }, [requestKey])

  const pending = loaded.key !== requestKey
  return {
    data: loaded.data,
    error: pending ? null : loaded.error,
    isLoading: pending && loaded.data === null,
    isRefetching: pending && loaded.data !== null,
    retry: () => setAttempt(a => a + 1),
  }
}
