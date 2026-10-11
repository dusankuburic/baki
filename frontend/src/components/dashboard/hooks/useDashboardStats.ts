import {getErrorMessage} from '@/lib/getErrorMessage'
import {useCallback, useEffect, useRef, useState} from 'react'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import {useToast} from '@/components/shared'
import type {DashboardStats} from '@/types'

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
  const toastRef = useRef(toast)
  useEffect(() => {
    toastRef.current = toast
  })

  const refresh = useCallback(
    (background = false) => {
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
            toastRef.current.error('Dashboard refresh failed', {
              description: getErrorMessage(err, 'Unknown error'),
            })
          } else {
            setError(getErrorMessage(err, 'Failed to load'))
          }
        })
        .finally(() => {
          if (myReq === reqIdRef.current) setLoading(false)
        })
    },
    [enabled, isLoaded],
  )

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    refresh()
    return () => {
      // eslint-disable-next-line react-hooks/exhaustive-deps
      reqIdRef.current++
    }
  }, [refresh, depKey])

  return {stats, loading, error, refresh}
}
