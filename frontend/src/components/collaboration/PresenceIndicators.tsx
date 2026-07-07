import clsx from 'clsx'
import { usePresenceStore, type PresenceUser } from '@/stores/presenceStore'
import Tooltip from '@/components/shared/Tooltip'
import Avatar from '@/components/shared/Avatar'

interface PresenceIndicatorsProps {
  className?: string
  maxVisible?: number
}

export default function PresenceIndicators({
  className,
  maxVisible = 5,
}: PresenceIndicatorsProps) {
  const users = usePresenceStore(s => s.users)
  const status = usePresenceStore(s => s.status)

  const list = Object.values(users)
  const visible = list.slice(0, maxVisible)
  const overflow = list.length - visible.length

  if (status === 'disconnected' || list.length === 0) return null

  return (
    <div className={clsx('flex items-center gap-1', className)}>
      {status === 'connecting' && (
        <span className="w-2 h-2 rounded-full bg-semantic-warning animate-pulse" title="Connecting…" />
      )}
      {status === 'error' && (
        <span className="w-2 h-2 rounded-full bg-semantic-error" title="Connection error" />
      )}
      {status === 'connected' && (
        <span className="w-2 h-2 rounded-full bg-semantic-success" title="Connected" />
      )}

      <div className="flex -space-x-2">
        {visible.map(user => (
          <UserAvatar key={user.userId} user={user} />
        ))}
        {overflow > 0 && (
          <div className="w-7 h-7 rounded-full bg-surface-3 border-2 border-surface-2 flex items-center justify-center text-2xs font-medium text-text-muted z-0">
            +{overflow}
          </div>
        )}
      </div>
    </div>
  )
}

function UserAvatar({ user }: { user: PresenceUser }) {
  return (
    <Tooltip content={user.displayName ?? user.userId}>
      <Avatar
        name={user.displayName ?? user.userId}
        colorSeed={user.userId}
        avatarUrl={user.avatarUrl}
        size="sm"
        className="border-2 border-surface-2 z-0 hover:z-10 transition-transform duration-fast hover:scale-110 cursor-default"
      />
    </Tooltip>
  )
}

