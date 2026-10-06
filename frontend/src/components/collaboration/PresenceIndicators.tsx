import {useEffect} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {usePresenceStore, type PresenceUser} from '@/stores/presenceStore'
import Tooltip from '@/components/shared/Tooltip'
import Avatar from '@/components/shared/Avatar'
import {useOptionalToast} from '@/components/shared'

interface PresenceIndicatorsProps {
  className?: string
  maxVisible?: number
}

export default function PresenceIndicators({className, maxVisible = 5}: PresenceIndicatorsProps) {
  const {t} = useTranslation('collaboration')
  const users = usePresenceStore(s => s.users)
  const status = usePresenceStore(s => s.status)
  const lastError = usePresenceStore(s => s.lastError)
  const toast = useOptionalToast()

  // Server-side refusals (a viewer's edit, an unknown event) arrive as `error`
  // envelopes on the socket. Without this they were silently dropped and the
  // user saw "nothing happens". Keyed on the whole object (its `at` changes)
  // so the same code twice still toasts twice.
  useEffect(() => {
    if (!lastError || !toast) return
    toast.error(lastError.message, {description: lastError.code})
  }, [lastError, toast])

  const list = Object.values(users)
  const visible = list.slice(0, maxVisible)
  const overflow = list.length - visible.length

  if (status === 'disconnected' || list.length === 0) return null

  return (
    <div className={clsx('flex items-center gap-1', className)}>
      {status === 'connecting' && (
        <span className="w-2 h-2 rounded-full bg-semantic-warning animate-pulse" title={t('presence.statusConnecting')} />
      )}
      {status === 'error' && (
        <span className="w-2 h-2 rounded-full bg-semantic-error" title={t('presence.statusError')} />
      )}
      {status === 'connected' && (
        <span className="w-2 h-2 rounded-full bg-semantic-success" title={t('presence.statusConnected')} />
      )}

      <div className="flex -space-x-2">
        {visible.map(user => (
          <UserAvatar key={user.userId} user={user} />
        ))}
        {overflow > 0 && (
          <div className="w-7 h-7 rounded-full bg-surface-3 border-2 border-surface-2 flex items-center justify-center text-2xs font-medium text-text-tertiary z-0">
            +{overflow}
          </div>
        )}
      </div>
    </div>
  )
}

function UserAvatar({user}: {user: PresenceUser}) {
  const {t} = useTranslation('collaboration')
  const label = user.selectedBlockId
    ? t('presence.viewingBlock', {name: user.displayName ?? user.userId})
    : (user.displayName ?? user.userId)
  return (
    <Tooltip content={label}>
      <Avatar
        name={user.displayName ?? user.userId}
        colorSeed={user.userId}
        avatarUrl={user.avatarUrl}
        size="sm"
        className={`border-2 border-surface-2 z-0 hover:z-10 transition-transform duration-fast hover:scale-110 cursor-default ${
          user.selectedBlockId ? 'ring-2 ring-brand-500/50' : ''
        }`}
      />
    </Tooltip>
  )
}
