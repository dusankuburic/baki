import React, {useEffect} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {RefreshCw, ShieldOff} from 'lucide-react'
import type {RoleFilter} from '@/api/admin'
import {EmptyState, IconButton} from '@/components/shared'
import {useActiveSection} from '@/components/auth/useActiveSection'
import {formatTimeOfDay} from '@/lib/format'
import {isTauri} from '@/platform/guards'
import {useAdminStore} from '@/stores/adminStore'
import {useAuthStore} from '@/stores/authStore'
import {AuditLogSection} from './AuditLogSection'
import {DataMigrationSection} from './DataMigrationSection'
import {OperationsSection} from './OperationsSection'
import {OverviewSection} from './OverviewSection'
import {UsersSection} from './UsersSection'
import {useMigration} from './hooks/useMigration'
import {useUserStats} from './hooks/useUserStats'

const SECTIONS = [
  {id: 'admin-overview', key: 'overview'},
  {id: 'admin-users', key: 'users'},
  {id: 'admin-operations', key: 'operations'},
  {id: 'admin-audit', key: 'audit'},
] as const

// Local mode has no audit backend (logAudit records nothing there), so the
// desktop build neither lists the section nor its nav entry. The id arrays
// are module constants because useActiveSection wants a stable array.
const ALL_SECTION_IDS = SECTIONS.map(s => s.id)
const LOCAL_SECTION_IDS = ALL_SECTION_IDS.filter(id => id !== 'admin-audit')

// The Admin Dashboard: an overview, the user list, operations and the audit
// log, with a section nav that is a sticky chip row in a narrow pane and a
// rail beside the content from 880px (the `admin` container in index.css).
// One Refresh reloads every section, health included.
export const AdminDashboard: React.FC = () => {
  const {t} = useTranslation('admin')
  const role = useAuthStore(s => s.user?.role)
  if (role !== 'admin') {
    return (
      <div className="admin-page h-full w-full overflow-y-auto bg-surface-1">
        <EmptyState
          icon={ShieldOff}
          title={t('page.accessDenied')}
          description={t('page.accessDeniedBody')}
          className="py-20"
        />
      </div>
    )
  }
  return <AdminPage />
}

function AdminPage() {
  const {t} = useTranslation('admin')
  const showAudit = !isTauri()
  const {active, jumpTo} = useActiveSection(showAudit ? ALL_SECTION_IDS : LOCAL_SECTION_IDS)
  const nonce = useAdminStore(s => s.nonce)
  const pending = useAdminStore(s => s.pending)
  const lastUpdated = useAdminStore(s => s.lastUpdated)
  const refresh = useAdminStore(s => s.refresh)
  const pendingSection = useAdminStore(s => s.pendingSection)
  const consumeSection = useAdminStore(s => s.consumeSection)
  const setUsersRole = useAdminStore(s => s.setUsersRole)
  const clearUsersFilters = useAdminStore(s => s.clearUsersFilters)
  const stats = useUserStats()
  const migration = useMigration()

  // A user row's "View activity" filtered the audit log: scroll there once
  // the filtered page has rendered.
  useEffect(() => {
    if (!pendingSection) return
    const id = consumeSection()
    if (id) requestAnimationFrame(() => jumpTo(id))
  }, [pendingSection, consumeSection, jumpTo])

  const showUsers = (role: RoleFilter) => {
    if (role === '') clearUsersFilters()
    else setUsersRole(role)
    jumpTo('admin-users')
  }
  const busy = pending > 0

  return (
    <div className="admin-page h-full w-full overflow-y-auto bg-surface-1">
      <div className="admin-layout">
        <header className="mb-4 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h1 className="text-2xl font-bold text-text-primary">{t('page.title')}</h1>
            <p className="mt-0.5 text-sm text-text-tertiary">{t('page.subtitle')}</p>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            {lastUpdated !== null && (
              <span className="text-xs tabular-nums text-text-tertiary">
                {t('page.updated', {time: formatTimeOfDay(lastUpdated)})}
              </span>
            )}
            <IconButton
              icon={RefreshCw}
              size="sm"
              label={t('page.refresh')}
              onClick={refresh}
              className={clsx(busy && '[&_svg]:animate-spin')}
            />
          </div>
        </header>
        <div
          className={clsx('admin-progress', busy && 'is-active')}
          role="progressbar"
          aria-label={t('page.updating')}
          aria-hidden={busy ? undefined : true}
        />

        <div className="admin-body">
          <nav className="admin-nav" aria-label={t('page.nav')}>
            {SECTIONS.filter(s => showAudit || s.id !== 'admin-audit').map(s => (
              <button
                key={s.id}
                type="button"
                onClick={() => jumpTo(s.id)}
                aria-current={active === s.id ? 'location' : undefined}
                className={clsx(
                  'admin-nav-item rounded-md px-2.5 py-1.5 text-xs font-medium transition-colors duration-fast',
                  active === s.id
                    ? 'bg-brand-500/10 text-brand-400'
                    : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary',
                )}
              >
                {t(`sections.${s.key}`)}
              </button>
            ))}
          </nav>
          <div className="admin-content min-w-0">
            <OverviewSection
              stats={stats}
              onShowUsers={showUsers}
              onShowOperations={() => jumpTo('admin-operations')}
            />
            <UsersSection stats={stats.data} />
            <OperationsSection refreshKey={nonce}>
              <DataMigrationSection
                status={migration.status}
                isLoading={migration.isLoading}
                isStarting={migration.isStarting}
                onStart={() => void migration.start()}
              />
            </OperationsSection>
            {showAudit && <AuditLogSection />}
          </div>
        </div>
      </div>
    </div>
  )
}
