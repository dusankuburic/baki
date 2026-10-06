import {useTranslation} from 'react-i18next'
import {X, LogOut} from 'lucide-react'
import clsx from 'clsx'
import type {AuthUser} from '@/api/auth'
import type {Organisation, OrgMember, OrgRole} from '@/stores/orgStore'
import {ROLES, ROLE_LABEL_KEYS, roleLabelKey} from './helpers'

interface MembersSectionProps {
  org: Organisation
  // The roster comes with the list, but a server that omits it must
  // not take the whole panel down with a nil dereference.
  members: OrgMember[]
  currentUser: AuthUser | null
  canManageMembers: boolean
  handleRoleChange: (userId: string, role: OrgRole, name?: string) => Promise<void>
  handleRemoveMember: (userId: string, name?: string) => Promise<void>
  handleLeave: (org: Organisation) => Promise<void>
}

export default function MembersSection({
  org,
  members,
  currentUser,
  canManageMembers,
  handleRoleChange,
  handleRemoveMember,
  handleLeave,
}: MembersSectionProps) {
  const {t} = useTranslation('settings')
  const roleLabel = (role: string) => {
    const key = roleLabelKey(role)
    return key ? t(key) : role
  }
  return (
    <div>
      <h4 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-2">
        {t('orgs.members')}
      </h4>
      <div className="border border-border-subtle rounded-lg overflow-hidden">
        {members.length === 0 ? (
          <div className="p-3 text-xs text-text-tertiary text-center">{t('orgs.noMembers')}</div>
        ) : (
          members.map((m, j) => (
            <div
              key={m.userId}
              className={clsx(
                'flex items-center gap-3 p-2.5',
                j !== members.length - 1 && 'border-b border-border-subtle',
              )}
            >
              <div className="flex-1 min-w-0">
                <p
                  title={m.user?.displayName || m.user?.email || m.userId}
                  className="text-sm text-text-primary truncate"
                >
                  {m.user?.displayName || m.user?.email || m.userId}
                </p>
                {m.user?.email && m.user.displayName && (
                  <p title={m.user.email} className="text-xs text-text-tertiary truncate">
                    {m.user.email}
                  </p>
                )}
              </div>
              {canManageMembers ? (
                <select
                  value={m.role}
                  onChange={e =>
                    void handleRoleChange(
                      m.userId,
                      e.target.value as OrgRole,
                      m.user?.displayName || m.user?.email || m.userId,
                    )
                  }
                  disabled={m.userId === org.ownerId}
                  className="px-2 py-1 rounded-md bg-surface-2 border border-border-default text-xs text-text-primary focus:outline-hidden focus:border-brand-500 disabled:opacity-50"
                >
                  {ROLES.map(r => (
                    <option key={r} value={r}>
                      {t(ROLE_LABEL_KEYS[r])}
                    </option>
                  ))}
                </select>
              ) : (
                <span className="px-2 py-1 text-xs text-text-tertiary">{roleLabel(m.role)}</span>
              )}
              {canManageMembers ? (
                <button
                  onClick={() =>
                    void handleRemoveMember(m.userId, m.user?.displayName || m.user?.email || m.userId)
                  }
                  disabled={m.userId === org.ownerId}
                  className="p-1.5 rounded text-text-tertiary hover:text-semantic-error hover:bg-semantic-error/10 transition-colors disabled:opacity-30 disabled:hover:bg-transparent disabled:hover:text-text-tertiary"
                  title={
                    m.userId === org.ownerId ? t('orgs.ownerNotRemovable') : t('orgs.removeMemberAria')
                  }
                  aria-label={
                    m.userId === org.ownerId ? t('orgs.ownerNotRemovable') : t('orgs.removeMemberAria')
                  }
                >
                  <X size={14} />
                </button>
              ) : m.userId === currentUser?.id ? (
                <button
                  onClick={() => void handleLeave(org)}
                  className="flex items-center gap-1 px-2 py-1 rounded text-xs text-text-tertiary hover:text-semantic-error hover:bg-semantic-error/10 transition-colors"
                  title={t('orgs.leaveAria')}
                  aria-label={t('orgs.leaveAria')}
                >
                  <LogOut size={12} />
                  {t('orgs.leave')}
                </button>
              ) : null}
            </div>
          ))
        )}
      </div>
    </div>
  )
}
