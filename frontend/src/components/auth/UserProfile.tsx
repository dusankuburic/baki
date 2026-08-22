import React, {useEffect} from 'react'
import clsx from 'clsx'
import {HardDrive} from 'lucide-react'
import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import {isTauri} from '@/platform/guards'
import Button from '@/components/shared/Button'
import Avatar from '@/components/shared/Avatar'
import {roleBadgeClass} from '@/lib/roleBadge'
import {ProfileCard} from './ProfileCard'
import {ChangePasswordCard} from './ChangePasswordCard'
import {OrganisationsCard} from './OrganisationsCard'
import {ActiveSessionsCard} from './ActiveSessionsCard'
import {AccountDataCard} from './AccountDataCard'

function formatMemberSince(iso: string | undefined): string | null {
  if (!iso) return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  return d.toLocaleDateString(undefined, {year: 'numeric', month: 'long'})
}

// LocalModeCard fills the profile pane for the desktop
// app: Tauri skips the auth gate entirely (ProtectedRoute.tsx), so `user` is
// always null there — there's no account to display or edit. This explains
// why and points at the local equivalents (Settings) instead of showing an
// empty pane with just a toolbar.
function LocalModeCard() {
  const toggleSettings = useUIStore(s => s.toggleSettings)
  return (
    <div className="p-6 md:p-8 max-w-lg mx-auto">
      <div className="bg-surface-2 border border-border-default rounded-xl p-6 text-center space-y-3">
        <div className="w-11 h-11 rounded-full bg-brand-500/10 flex items-center justify-center text-brand-400 mx-auto">
          <HardDrive size={20} />
        </div>
        <div>
          <p className="text-sm font-medium text-text-primary">Local Mode</p>
          <p className="text-sm text-text-tertiary mt-1">
            You're running the desktop app in local mode — flows, findings, and analysis stay on this machine. There's
            no cloud account to manage here.
          </p>
        </div>
        <p className="text-xs text-text-tertiary">AI provider keys and app preferences live in Settings.</p>
        <Button variant="secondary" size="sm" onClick={toggleSettings}>
          Open Settings
        </Button>
      </div>
    </div>
  )
}

export const UserProfile: React.FC = () => {
  const user = useAuthStore(s => s.user)
  const logout = useAuthStore(s => s.logout)
  const organisations = useOrgStore(s => s.organisations)
  const loadOrgs = useOrgStore(s => s.loadOrgs)

  useEffect(() => {
    if (user) void loadOrgs()
  }, [user, loadOrgs])

  if (!user) return isTauri() ? <LocalModeCard /> : null

  const displayName = user.displayName?.trim()
  const memberSince = formatMemberSince(user.createdAt)

  return (
    <div className="p-6 md:p-8 max-w-3xl mx-auto space-y-5">
      <div className="bg-surface-2 border border-border-default rounded-xl p-5">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-center gap-3">
            <Avatar name={displayName || user.email} colorSeed={user.id} avatarUrl={user.avatarUrl} size="lg" />
            <div>
              <p className="text-sm font-medium text-text-primary">{displayName || user.email}</p>
              {displayName && <p className="text-xs text-text-tertiary">{user.email}</p>}
              <div className="flex items-center gap-2 mt-1">
                <span
                  className={clsx(
                    'inline-block px-2 py-0.5 rounded-md text-xs font-semibold uppercase',
                    roleBadgeClass(user.role),
                  )}
                >
                  {user.role}
                </span>
                {memberSince && <span className="text-xs text-text-tertiary">Member since {memberSince}</span>}
              </div>
            </div>
          </div>
          <Button variant="danger" size="sm" onClick={() => logout()}>
            Logout
          </Button>
        </div>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-2 gap-5 items-start">
        <div className="space-y-5">
          <ProfileCard />
          <OrganisationsCard organisations={organisations} userId={user.id} />
          <AccountDataCard />
        </div>
        <div className="space-y-5">
          <ActiveSessionsCard />
          <ChangePasswordCard />
        </div>
      </div>
    </div>
  )
}
