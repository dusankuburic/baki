import React, {useEffect} from 'react'
import clsx from 'clsx'
import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'
import Button from '@/components/shared/Button'
import {ProfileCard} from './ProfileCard'
import {ChangePasswordCard} from './ChangePasswordCard'
import {OrganisationsCard} from './OrganisationsCard'
import {ActiveSessionsCard} from './ActiveSessionsCard'

function roleBadgeClass(role: string) {
  switch (role) {
    case 'admin':   return 'bg-block-subflow/10 text-block-subflow'
    case 'member':  return 'bg-block-action/10 text-block-action'
    case 'viewer':  return 'bg-block-condition/10 text-block-condition'
    default:        return 'bg-surface-4 text-text-tertiary'
  }
}

export const UserProfile: React.FC = () => {
  const {user, logout} = useAuthStore()
  const {organisations, loadOrgs} = useOrgStore()

  useEffect(() => { loadOrgs() }, [loadOrgs])

  if (!user) return null

  const displayName = user.displayName?.trim()
  const initials = (displayName?.charAt(0) || user.email.charAt(0)).toUpperCase()

  return (
    <div className="p-6 md:p-8 max-w-lg mx-auto space-y-5">
      <div className="bg-surface-2 border border-border-default rounded-xl p-5">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-center gap-3">
            <div className="w-11 h-11 rounded-full bg-brand-500/20 flex items-center justify-center text-brand-400 font-bold text-lg flex-shrink-0 select-none">
              {initials}
            </div>
            <div>
              <p className="text-sm font-medium text-text-primary">{displayName || user.email}</p>
              {displayName && <p className="text-xs text-text-tertiary">{user.email}</p>}
              <span className={clsx(
                'inline-block mt-1 px-2 py-0.5 rounded-md text-xs font-semibold uppercase',
                roleBadgeClass(user.role)
              )}>
                {user.role}
              </span>
            </div>
          </div>
          <Button variant="danger" size="sm" onClick={() => logout()}>Logout</Button>
        </div>
      </div>

      <ProfileCard />
      <OrganisationsCard organisations={organisations} userId={user.id} />
      <ActiveSessionsCard />
      <ChangePasswordCard />
    </div>
  )
}
