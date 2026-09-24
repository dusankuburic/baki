import {useTranslation} from 'react-i18next'
import React, {useEffect, useState} from 'react'
import clsx from 'clsx'
import {HardDrive, LogOut, Pencil} from 'lucide-react'
import {type AuthUser} from '@/api/auth'
import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import {isTauri} from '@/platform/guards'
import {Avatar, Button} from '@/components/shared'
import {RoleBadge} from './SectionCard'
import {ProfileSection} from './ProfileSection'
import {OrganisationsSection} from './OrganisationsSection'
import {ChangePasswordSection} from './ChangePasswordSection'
import {ActiveSessionsSection} from './ActiveSessionsSection'
import {LinkedIdentitiesSection} from './LinkedIdentitiesSection'
import {AccountDataSection} from './AccountDataSection'
import {ApiTokensSection} from './ApiTokensSection'
import {DangerZoneSection} from './DangerZoneSection'
import {useActiveSection} from './useActiveSection'

function formatMemberSince(iso: string | undefined): string | null {
  if (!iso) return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  return d.toLocaleDateString(undefined, {year: 'numeric', month: 'long'})
}

// The page is three groups — who you are, how you sign in, what is stored
// about you — in the order a visitor needs them: the everyday edit first,
// the irreversible action last. The nav jumps between the groups; the
// ids are the anchors it observes (see useActiveSection).
const GROUPS = [
  {id: 'profile-account', label: 'account', body: 'accountBody'},
  {id: 'profile-security', label: 'security', body: 'securityBody'},
  {id: 'profile-data', label: 'data', body: 'dataBody'},
] as const
const GROUP_IDS = GROUPS.map(g => g.id)

// LocalModeCard fills the profile pane for the desktop
// app: Tauri skips the auth gate entirely (ProtectedRoute.tsx), so `user` is
// always null there — there's no account to display or edit. This explains
// why and points at the local equivalents (Settings) instead of showing an
// empty pane with just a toolbar.
function LocalModeCard() {
  const {t} = useTranslation('auth')
  const toggleSettings = useUIStore(s => s.toggleSettings)
  return (
    <div className="p-6 md:p-8 max-w-lg mx-auto">
      <div className="bg-surface-2 border border-border-default rounded-xl p-6 text-center space-y-3">
        <div className="w-11 h-11 rounded-full bg-brand-500/10 flex items-center justify-center text-brand-400 mx-auto">
          <HardDrive size={20} />
        </div>
        <div>
          <p className="text-sm font-medium text-text-primary">{t('localMode.title')}</p>
          <p className="text-sm text-text-tertiary mt-1">{t('localMode.body')}</p>
        </div>
        <p className="text-xs text-text-tertiary">{t('localMode.settingsHint')}</p>
        <Button variant="secondary" size="sm" onClick={toggleSettings}>
          {t('localMode.openSettings')}
        </Button>
      </div>
    </div>
  )
}

function ProfileHero({
  user,
  orgCount,
  onEdit,
  onSignOut,
}: {
  user: AuthUser
  orgCount: number
  onEdit: () => void
  onSignOut: () => void
}) {
  const {t} = useTranslation('auth')
  const displayName = user.displayName?.trim()
  const memberSince = formatMemberSince(user.createdAt)
  return (
    <header className="profile-hero bg-surface-2 border border-border-default rounded-xl p-5 flex flex-wrap items-center gap-4">
      <Avatar name={displayName || user.email} colorSeed={user.id} avatarUrl={user.avatarUrl} size="xl" />
      <div className="min-w-0 flex-1">
        <h1 className="text-xl font-semibold text-text-primary truncate">{displayName || user.email}</h1>
        {displayName && <p className="text-sm text-text-secondary truncate">{user.email}</p>}
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 mt-1.5 text-xs text-text-tertiary">
          <RoleBadge role={user.role} />
          {memberSince && <span>{t('profile.memberSince', {date: memberSince})}</span>}
          {orgCount > 0 && <span>{t('profile.orgCount', {count: orgCount})}</span>}
        </div>
      </div>
      <div className="profile-hero-actions flex items-center gap-2">
        <Button variant="secondary" size="sm" icon={Pencil} onClick={onEdit}>
          {t('profile.editProfile')}
        </Button>
        <Button variant="ghost" size="sm" icon={LogOut} onClick={onSignOut}>
          {t('profile.logout')}
        </Button>
      </div>
    </header>
  )
}

function ProfileGroup({
  id,
  title,
  description,
  children,
}: {
  id: string
  title: string
  description: string
  children: React.ReactNode
}) {
  return (
    <section id={id} aria-labelledby={`${id}-title`} className="profile-group">
      <div className="mb-3">
        <h2 id={`${id}-title`} tabIndex={-1} className="text-base font-semibold text-text-primary outline-none">
          {title}
        </h2>
        <p className="text-sm text-text-tertiary mt-0.5">{description}</p>
      </div>
      <div className="space-y-4">{children}</div>
    </section>
  )
}

export const UserProfile: React.FC = () => {
  const {t} = useTranslation('auth')
  const user = useAuthStore(s => s.user)
  const logout = useAuthStore(s => s.logout)
  const organisations = useOrgStore(s => s.organisations)
  const orgsLoading = useOrgStore(s => s.isLoading)
  const loadOrgs = useOrgStore(s => s.loadOrgs)
  const [editing, setEditing] = useState(false)
  const {active, jumpTo} = useActiveSection(GROUP_IDS)

  useEffect(() => {
    if (user) void loadOrgs()
  }, [user, loadOrgs])

  if (!user) return isTauri() ? <LocalModeCard /> : null

  const handleEditProfile = () => {
    setEditing(true)
    jumpTo('profile-account')
  }

  return (
    <div className="profile-page">
      <div className="profile-layout">
        <ProfileHero
          user={user}
          orgCount={organisations.length}
          onEdit={handleEditProfile}
          onSignOut={() => void logout()}
        />
        <div className="profile-body">
          <nav aria-label={t('nav.label')} className="profile-nav">
            {GROUPS.map(group => {
              const current = active === group.id
              return (
                <button
                  key={group.id}
                  type="button"
                  aria-current={current ? 'true' : undefined}
                  onClick={() => jumpTo(group.id)}
                  className={clsx(
                    'profile-nav-item px-3 py-1.5 rounded-md text-sm font-medium transition-colors duration-fast',
                    'focus-visible:ring-2 focus-visible:ring-brand-500/40 focus-visible:outline-none',
                    current
                      ? 'bg-brand-500/10 text-brand-400'
                      : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary',
                  )}
                >
                  {t(`nav.${group.label}`)}
                </button>
              )
            })}
          </nav>
          <div className="profile-content space-y-8 min-w-0">
            <ProfileGroup id="profile-account" title={t('nav.account')} description={t('groups.accountBody')}>
              <ProfileSection editing={editing} onEditingChange={setEditing} />
              <OrganisationsSection organisations={organisations} userId={user.id} loading={orgsLoading} />
            </ProfileGroup>
            <ProfileGroup id="profile-security" title={t('nav.security')} description={t('groups.securityBody')}>
              <ChangePasswordSection />
              <ActiveSessionsSection />
              <LinkedIdentitiesSection />
            </ProfileGroup>
            <ProfileGroup id="profile-data" title={t('nav.data')} description={t('groups.dataBody')}>
              <AccountDataSection />
              <ApiTokensSection />
              <DangerZoneSection />
            </ProfileGroup>
          </div>
        </div>
      </div>
    </div>
  )
}
