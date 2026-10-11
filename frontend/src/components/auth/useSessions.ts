import {getErrorMessage} from '@/lib/getErrorMessage'
import {useTranslation} from 'react-i18next'
import {useState, useCallback, useMemo} from 'react'
import {authApi, type SessionInfo} from '@/api/auth'
import {getCurrentSessionId} from '@/stores/authStore'
import {useAsync} from '@/hooks/useAsync'

function issuedAt(session: SessionInfo): number {
  const t = Date.parse(session.createdAt)
  return Number.isNaN(t) ? 0 : t
}

export function useSessions() {
  const {t} = useTranslation('auth')
  const [actionError, setActionError] = useState<string | null>(null)
  const [revokingId, setRevokingId] = useState<string | null>(null)
  const [revokingOthers, setRevokingOthers] = useState(false)
  const currentSessionId = getCurrentSessionId()

  const {
    data,
    isLoading: loading,
    error: fetchError,
    setData: setSessions,
    refetch,
  } = useAsync<SessionInfo[]>(() => authApi.listSessions().then(list => list ?? []), [])
  const sessions = useMemo(() => {
    const list = data ?? []
    return [...list].sort((a, b) => {
      if (a.id === currentSessionId) return -1
      if (b.id === currentSessionId) return 1
      return issuedAt(b) - issuedAt(a)
    })
  }, [data, currentSessionId])
  const error = actionError ?? fetchError

  const revoke = useCallback(
    async (id: string): Promise<boolean> => {
      setRevokingId(id)
      setActionError(null)
      try {
        await authApi.revokeSession(id)
        setSessions(prev => (prev ?? []).filter(s => s.id !== id))
        return true
      } catch (err) {
        setActionError(getErrorMessage(err, t('sessions.revokeFailed')))
        return false
      } finally {
        setRevokingId(null)
      }
    },
    [setSessions, t],
  )

  const revokeOthers = useCallback(async (): Promise<number> => {
    if (!currentSessionId) return 0
    const others = sessions.filter(s => s.id !== currentSessionId)
    if (others.length === 0) return 0
    setRevokingOthers(true)
    setActionError(null)
    const results = await Promise.allSettled(others.map(s => authApi.revokeSession(s.id)))
    const revokedIds = new Set(others.filter((_, i) => results[i]?.status === 'fulfilled').map(s => s.id))
    setSessions(prev => (prev ?? []).filter(s => !revokedIds.has(s.id)))
    if (revokedIds.size < others.length) {
      setActionError(t('sessions.revokeOthersPartial'))
    }
    setRevokingOthers(false)
    return revokedIds.size
  }, [sessions, currentSessionId, setSessions, t])

  return {sessions, loading, error, refetch, revokingId, revoke, currentSessionId, revokingOthers, revokeOthers}
}
