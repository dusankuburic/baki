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
  // The session list is keyed by refresh-token jti, which is also the claim
  // in the refresh token this tab is holding — decoding it client-side needs
  // no extra request and no backend change to know "which row is me".
  const currentSessionId = getCurrentSessionId()

  const {
    data,
    isLoading: loading,
    error: fetchError,
    setData: setSessions,
    refetch,
  } = useAsync<SessionInfo[]>(() => authApi.listSessions().then(list => list ?? []), [])
  // This device first, then the newest sign-in first: the row a user looks
  // for is either "me" or the one that just appeared.
  const sessions = useMemo(() => {
    const list = data ?? []
    return [...list].sort((a, b) => {
      if (a.id === currentSessionId) return -1
      if (b.id === currentSessionId) return 1
      return issuedAt(b) - issuedAt(a)
    })
  }, [data, currentSessionId])
  const error = actionError ?? fetchError

  // Resolves true when the session is gone so the caller can confirm it.
  const revoke = useCallback(
    async (id: string): Promise<boolean> => {
      setRevokingId(id)
      setActionError(null)
      try {
        await authApi.revokeSession(id)
        // Functional updater so two rapid revokes don't both close over the
        // same stale snapshot (the second would re-add the first's just-removed
        // session until the next refetch).
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

  // revokeOthers signs out every session except the current one and resolves
  // to how many it signed out. There's no dedicated "revoke all others"
  // endpoint — each other session is already individually revocable, so this
  // just fans that out client-side.
  const revokeOthers = useCallback(async (): Promise<number> => {
    // Without a decodable current-session id, "others" would be EVERY
    // session — including this one, silently signing the user out. Refuse
    // rather than guess (the section also hides the button in this state).
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
