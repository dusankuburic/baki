import { Trash2 } from 'lucide-react'
import type { Collaborator, Permission } from '@/api/sharing'
import PermissionSelect from './PermissionSelect'
import IconButton from '@/components/shared/IconButton'
import Avatar from '@/components/shared/Avatar'

interface CollaboratorListProps {
  collaborators: Collaborator[]
  currentUserId?: string
  onChangePermission: (userId: string, permission: Permission) => void
  onRemove: (userId: string) => void
  disabled?: boolean
}

export default function CollaboratorList({
  collaborators,
  currentUserId,
  onChangePermission,
  onRemove,
  disabled,
}: CollaboratorListProps) {
  if (collaborators.length === 0) {
    return (
      <p className="py-4 text-center text-sm text-text-muted">
        No collaborators yet
      </p>
    )
  }

  return (
    <ul className="flex flex-col gap-1">
      {collaborators.map(c => {
        const isMe = c.userId === currentUserId
        return (
          <li key={c.userId} className="flex items-center gap-3 py-1.5">
            <Avatar name={c.displayName ?? c.email} colorSeed={c.userId} avatarUrl={c.avatarUrl} size="md" />

            <div className="flex-1 min-w-0">
              <p className="text-sm font-medium text-text-primary truncate">
                {c.displayName ?? c.email}
                {isMe && <span className="ml-1 text-xs text-text-muted">(you)</span>}
              </p>
              <p className="text-xs text-text-muted truncate">{c.email}</p>
            </div>

            <PermissionSelect
              value={c.permission}
              onChange={p => onChangePermission(c.userId, p)}
              disabled={disabled || isMe}
            />

            <IconButton
              icon={Trash2}
              label="Remove"
              variant="ghost"
              size="sm"
              onClick={() => onRemove(c.userId)}
              disabled={disabled || isMe}
              className="text-semantic-error hover:bg-semantic-error/10"
            />
          </li>
        )
      })}
    </ul>
  )
}
