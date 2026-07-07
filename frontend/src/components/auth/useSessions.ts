import {useState, useEffect, useCallback} from 'react'
import {authApi, type SessionInfo} from '@/api/auth'
import {getCurrentSessionId} from '@/stores/authStore'

export function useSessions() {
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [revokingId, setRevokingId] = useState<string | null>(null)
  const [revokingOthers, setRevokingOthers] = useState(false)
  // The session list is keyed by refresh-token jti, which is also the claim
  // in the refresh token this tab is holding — decoding it client-side needs
  // no extra request and no backend change to know "which row is me".
  const currentSessionId = getCurrentSessionId()

  useEffect(() => {
    let cancelled = false
    authApi.listSessions()
      .then(list => { if (!cancelled) setSessions(list ?? []) })
      .catch(err => { if (!cancelled) setError(err instanceof Error ? err.message : 'Failed to load sessions') })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [])

  const revoke = useCallback(async (id: string) => {
    setRevokingId(id)
    setError(null)
    try {
      await authApi.revokeSession(id)
      setSessions(prev => prev.filter(s => s.id !== id))
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to revoke session')
    } finally {
      setRevokingId(null)
    }
  }, [])

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
    setError(null)
    const results = await Promise.allSettled(others.map(s => authApi.revokeSession(s.id)))
    const revokedIds = new Set(others.filter((_, i) => results[i].status === 'fulfilled').map(s => s.id))
    setSessions(prev => prev.filter(s => !revokedIds.has(s.id)))
    if (revokedIds.size < others.length) {
      setError('Some sessions could not be signed out — try again.')
    }
    setRevokingOthers(false)
  }, [sessions, currentSessionId])

  return {sessions, loading, error, revokingId, revoke, currentSessionId, revokingOthers, revokeOthers}
}
