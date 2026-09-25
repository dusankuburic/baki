import {useEffect, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {dashboardApi} from '@/api'
import {logger} from '@/lib/logger'
import {useToast} from '@/components/shared'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useAnalyticsStore, toQuery} from '@/stores/analyticsStore'
import type {DashboardAnalyticsData} from '@/types'

interface Loaded {
  /** The request key the data/error belong to. */
  key: string
  data: DashboardAnalyticsData | null
  error: string | null
}

// Fetches the analytics payload for the store's filters. A filter change
// aborts the request it supersedes and keeps the previous payload on screen
// (isRefetching) until the new one lands; an error after data was shown is a
// toast plus an inline notice, never a blank page. Finishing an analysis
// refreshes the current view, as Home does.
export function useAnalyticsData(): {
  data: DashboardAnalyticsData | null
  error: string | null
  isLoading: boolean
  isRefetching: boolean
  retry: () => void
  query: string
} {
  const {t} = useTranslation('dashboard')
  const filters = useAnalyticsStore(s => s.filters)
  const nonce = useAnalyticsStore(s => s.nonce)
  const refresh = useAnalyticsStore(s => s.refresh)
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const toast = useToast()
  const toastRef = useRef(toast)
  useEffect(() => {
    toastRef.current = toast
  })

  const query = useMemo(() => toQuery(filters), [filters])
  const key = `${query}#${nonce}`
  const [loaded, setLoaded] = useState<Loaded>({key: '', data: null, error: null})
  // What is on screen, for the failure path of a later request (which decides
  // between a blank page and a notice over the previous charts).
  const dataRef = useRef<DashboardAnalyticsData | null>(null)
  useEffect(() => {
    dataRef.current = loaded.data
  }, [loaded.data])

  useEffect(() => {
    const controller = new AbortController()
    dashboardApi
      .getAnalytics(query, controller.signal)
      .then(d => {
        if (controller.signal.aborted) return
        setLoaded({key, data: d, error: null})
      })
      .catch((e: unknown) => {
        if (controller.signal.aborted || (e instanceof Error && e.name === 'AbortError')) return
        logger.error('analytics: load failed', e)
        const message = e instanceof Error ? e.message : t('common.unknownError')
        if (dataRef.current) toastRef.current.error(t('analytics.refreshFailed'), {description: message})
        setLoaded({key, data: dataRef.current, error: message})
      })
    return () => controller.abort()
  }, [key, query, t])

  const wasAnalyzing = useRef(false)
  useEffect(() => {
    if (wasAnalyzing.current && !isAnalyzing) refresh()
    wasAnalyzing.current = isAnalyzing
  }, [isAnalyzing, refresh])

  const pending = loaded.key !== key
  return {
    data: loaded.data,
    error: loaded.error,
    isLoading: pending && !loaded.data,
    isRefetching: pending && !!loaded.data,
    retry: refresh,
    query,
  }
}
