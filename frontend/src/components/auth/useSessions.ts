import {useState, useEffect, useCallback} from 'react'
import {authApi, type SessionInfo} from '@/api/auth'

export function useSessions() {
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [revokingId, setRevokingId] = useState<string | null>(null)

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

  return {sessions, loading, error, revokingId, revoke}
}
