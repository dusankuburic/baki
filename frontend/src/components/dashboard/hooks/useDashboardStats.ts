import {useCallback, useEffect, useRef, useState} from 'react'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import {useToast} from '@/components/shared'
import type {DashboardStats} from '@/types'

// useDashboardStats owns the session-analytics fetch: capability gating, the
// per-request id race guard (a stale response from an older refresh must never
// overwrite a newer one), the "first load vs background refresh" loading/error
// split (a background refresh failure toasts instead of blowing away good
// data), and re-fetching when the open document changes.
//
// Extracted from AnalyticsDashboard so the fetch/state machine is independently
// testable and the component is just rendering.
export interface DashboardStatsState {
  stats: DashboardStats | null
  loading: boolean
  error: string | null
  refresh: (background?: boolean) => void
}

export function useDashboardStats(enabled: boolean, isLoaded: boolean, depKey: string | null): DashboardStatsState {
  const toast = useToast()
  const [stats, setStats] = useState<DashboardStats | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const reqIdRef = useRef(0)
  const hasStatsRef = useRef(false)
  // Keep the latest toast so the async callbacks don't capture a stale one.
  const toastRef = useRef(toast)
  toastRef.current = toast

  const refresh = useCallback(
    (background = false) => {
      // Session analytics aggregate the app's in-process analyzer cache and read
      // server-local folders. In cloud/JWT mode the backend returns 403 (data
      // would otherwise span tenants), so we check the capability flag provided
      // by the backend. Wait for the capability to load before fetching so we
      // don't fire a doomed 403 in cloud mode.
      if (!isLoaded || !enabled) return

      reqIdRef.current++
      const myReq = reqIdRef.current
      if (!background) {
        setLoading(true)
        setError(null)
      }
      analysisApi
        .getDashboard()
        .then(s => {
          if (myReq !== reqIdRef.current) return
          setStats(s)
          hasStatsRef.current = true
          setError(null)
        })
        .catch(err => {
          if (myReq !== reqIdRef.current) return
          logger.warn('Failed to load dashboard stats', err)
          if (hasStatsRef.current) {
            // We already have data on screen — a background refresh failure
            // toasts rather than wiping the good data with an error state.
            toastRef.current.error('Dashboard refresh failed', {
              description: err instanceof Error ? err.message : 'Unknown error',
            })
          } else {
            setError(err instanceof Error ? err.message : 'Failed to load')
          }
        })
        .finally(() => {
          if (myReq === reqIdRef.current) setLoading(false)
        })
    },
    [enabled, isLoaded],
  )

  useEffect(() => {
    refresh()
    return () => {
      // Invalidate any in-flight request on unmount/dep-change so a late
      // response can't write state into an unmounted/changed component.
      reqIdRef.current++
    }
  }, [refresh, depKey])

  return {stats, loading, error, refresh}
}
