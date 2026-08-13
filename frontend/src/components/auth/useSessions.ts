import {useState, useCallback, useMemo} from 'react'
import {authApi, type SessionInfo} from '@/api/auth'
import {getCurrentSessionId} from '@/stores/authStore'
import {useAsync} from '@/hooks/useAsync'

export function useSessions() {
  const [actionError, setActionError] = useState<string | null>(null)
  const [revokingId, setRevokingId] = useState<string | null>(null)
  const [revokingOthers, setRevokingOthers] = useState(false)
  // The session list is keyed by refresh-token jti, which is also the claim
  // in the refresh token this tab is holding — decoding it client-side needs
  // no extra request and no backend change to know "which row is me".
  const currentSessionId = getCurrentSessionId()

  const {
    data,
    isLoading: loading,
    error: fetchError,
    setData: setSessions,
  } = useAsync<SessionInfo[]>(() => authApi.listSessions().then(list => list ?? []), [])
  const sessions = useMemo(() => data ?? [], [data])
  const error = actionError ?? fetchError

  const revoke = useCallback(
    async (id: string) => {
      setRevokingId(id)
      setActionError(null)
      try {
        await authApi.revokeSession(id)
        // Functional updater so two rapid revokes don't both close over the
        // same stale snapshot (the second would re-add the first's just-removed
        // session until the next refetch).
        setSessions(prev => (prev ?? []).filter(s => s.id !== id))
      } catch (err) {
        setActionError(err instanceof Error ? err.message : 'Failed to revoke session')
      } finally {
        setRevokingId(null)
      }
    },
    [setSessions],
  )

  // revokeOthers signs out every session except the current one. There's no
  // dedicated "revoke all others" endpoint — each other session is already
  // individually revocable, so this just fans that out client-side.
  const revokeOthers = useCallback(async () => {
    // Without a decodable current-session id, "others" would be EVERY
    // session — including this one, silently signing the user out. Refuse
    // rather than guess (the card also hides the button in this state).
    if (!currentSessionId) return
    const others = sessions.filter(s => s.id !== currentSessionId)
    if (others.length === 0) return
    setRevokingOthers(true)
    setActionError(null)
    const results = await Promise.allSettled(others.map(s => authApi.revokeSession(s.id)))
    const revokedIds = new Set(others.filter((_, i) => results[i].status === 'fulfilled').map(s => s.id))
    setSessions(prev => (prev ?? []).filter(s => !revokedIds.has(s.id)))
    if (revokedIds.size < others.length) {
      setActionError('Some sessions could not be signed out — try again.')
    }
    setRevokingOthers(false)
  }, [sessions, currentSessionId, setSessions])

  return {sessions, loading, error, revokingId, revoke, currentSessionId, revokingOthers, revokeOthers}
}
