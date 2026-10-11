import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {BadgeCheck, Check, CircleDashed, Copy, ScrollText} from 'lucide-react'
import type {AdminUser, UserRole} from '@/api/admin'
import {Avatar, IconButton} from '@/components/shared'
import {useCopy} from '@/hooks/useCopy'
import {roleBadgeClass} from '@/lib/roleBadge'
import {absoluteTime, relativeTimeText} from '@/lib/time'
import {ROLE_OPTIONS, roleLabel} from './roles'

export function UserRow({
  user,
  isSelf,
  busy,
  showActivity,
  onChangeRole,
  onViewActivity,
}: {
  user: AdminUser
  isSelf: boolean
  busy: boolean
  showActivity: boolean
  onChangeRole: (role: UserRole) => void
  onViewActivity: () => void
}) {
  const {t} = useTranslation('admin')
  const {copied, copy} = useCopy()
  const name = user.displayName || user.email

  return (
    <tr className="transition-colors duration-fast hover:bg-surface-3/60">
      <td className="px-4 py-2.5">
        <div className="flex min-w-0 items-center gap-3">
          <Avatar name={name} colorSeed={user.id} avatarUrl={user.avatarUrl || undefined} size="sm" />
          <div className="min-w-0">
            <div className="flex min-w-0 items-center gap-2">
              <span className="truncate font-medium text-text-primary">{name}</span>
              {isSelf && (
                <span className="shrink-0 rounded bg-brand-500/10 px-1.5 py-0.5 text-2xs font-semibold uppercase tracking-wide text-brand-400">
                  {t('users.you')}
                </span>
              )}
            </div>
            {user.displayName && <div className="truncate text-xs text-text-tertiary">{user.email}</div>}
          </div>
        </div>
      </td>
      <td className="px-4 py-2.5">
        <span
          className={clsx(
            'inline-block rounded-md px-2 py-0.5 text-2xs font-semibold uppercase tracking-wide',
            roleBadgeClass(user.role),
          )}
        >
          {roleLabel(t, user.role)}
        </span>
      </td>
      <td className="admin-col-optional whitespace-nowrap px-4 py-2.5 text-xs text-text-secondary">
        <time dateTime={user.createdAt} title={absoluteTime(user.createdAt)}>
          {relativeTimeText(user.createdAt)}
        </time>
      </td>
      <td className="px-4 py-2.5">
        {user.emailVerified ? (
          <BadgeCheck size={15} className="text-semantic-success" aria-hidden="true" />
        ) : (
          <CircleDashed size={15} className="text-text-disabled" aria-hidden="true" />
        )}
        <span className="sr-only">{user.emailVerified ? t('users.verified') : t('users.unverified')}</span>
      </td>
      <td className="admin-col-optional whitespace-nowrap px-4 py-2.5">
        <span className="inline-flex items-center gap-1">
          <span className="font-mono text-2xs text-text-tertiary" title={user.id}>
            {user.id.slice(0, 8)}
          </span>
          <IconButton
            icon={copied ? Check : Copy}
            size="sm"
            label={copied ? t('users.copied') : t('users.copyId')}
            onClick={() => copy(user.id)}
          />
        </span>
      </td>
      <td className="px-4 py-2.5">
        <select
          value={user.role}
          onChange={e => onChangeRole(e.target.value as UserRole)}
          disabled={busy}
          aria-label={t('users.roleSelect', {email: user.email})}
          className="h-8 rounded-md border border-border-default bg-surface-2 px-2 text-xs text-text-primary focus:outline-hidden focus:ring-2 focus:ring-brand-500/20 disabled:opacity-50"
        >
          {user.role === '' && (
            <option value="" disabled>
              {t('users.rolePlaceholder')}
            </option>
          )}
          {ROLE_OPTIONS.map(r => (
            <option key={r} value={r}>
              {roleLabel(t, r)}
            </option>
          ))}
        </select>
      </td>
      {showActivity && (
        <td className="px-4 py-2.5 text-right">
          <IconButton icon={ScrollText} size="sm" label={t('users.viewActivity')} onClick={onViewActivity} />
        </td>
      )}
    </tr>
  )
}
