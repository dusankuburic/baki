import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Activity, Shield, UserCheck, Users, UsersRound} from 'lucide-react'
import type {RoleFilter, UserStats} from '@/api/admin'
import {StatTile} from '@/components/charts'
import {SectionNotice} from '@/components/auth/SectionCard'
import {Button} from '@/components/shared'
import {useAdminStore} from '@/stores/adminStore'
import type {AdminQueryResult} from './hooks/useAdminQuery'
import {AdminSection} from './AdminSection'

// The overview tiles: the whole user table counted by role and state (from
// the stats endpoint, so paging the list never changes them) and the last
// health check. A tile that names a role opens the user list filtered to it.
export function OverviewSection({
  stats,
  onShowUsers,
  onShowOperations,
}: {
  stats: AdminQueryResult<UserStats>
  onShowUsers: (role: RoleFilter) => void
  onShowOperations: () => void
}) {
  const {t} = useTranslation(['admin', 'common'])
  const health = useAdminStore(s => s.health)
  const {data, error, isLoading, isRefetching, retry} = stats
  const unavailable = !data && !isLoading ? t('stats.unavailable') : undefined
  const n = (v: number | undefined) => (v === undefined ? '—' : String(v))
  const other = data ? data.byRole.viewer + data.byRole.guest + data.byRole.none : undefined
  const healthTone =
    health?.overall === 'ok'
      ? 'text-semantic-success'
      : health?.overall === 'degraded'
        ? 'text-semantic-warning'
        : health?.overall === 'down'
          ? 'text-semantic-error'
          : 'text-text-tertiary'

  return (
    <AdminSection id="admin-overview" title={t('sections.overview')} description={t('sectionSub.overview')}>
      {error && !data && (
        <SectionNotice
          message={t('stats.loadFailed')}
          action={
            <Button size="sm" variant="ghost" onClick={retry}>
              {t('common:retry')}
            </Button>
          }
        />
      )}
      <div
        className={clsx('admin-tiles transition-opacity duration-normal', isRefetching && 'opacity-60')}
        aria-busy={isRefetching || undefined}
      >
        {isLoading ? (
          <SkeletonTiles count={5} />
        ) : (
          <>
            <StatTile
              icon={Users}
              label={t('stats.total')}
              value={n(data?.total)}
              sub={data ? t('stats.totalSub', {unverified: data.unverified, locked: data.locked}) : undefined}
              unavailable={unavailable}
              onClick={() => onShowUsers('')}
            />
            <StatTile
              icon={Shield}
              label={t('stats.admins')}
              value={n(data?.byRole.admin)}
              sub={t('stats.adminsSub')}
              unavailable={unavailable}
              onClick={() => onShowUsers('admin')}
            />
            <StatTile
              icon={UserCheck}
              label={t('stats.members')}
              value={n(data?.byRole.member)}
              sub={t('stats.membersSub')}
              unavailable={unavailable}
              onClick={() => onShowUsers('member')}
            />
            <StatTile
              icon={UsersRound}
              label={t('stats.other')}
              value={n(other)}
              sub={
                data
                  ? t('stats.otherSub', {
                      viewers: data.byRole.viewer,
                      guests: data.byRole.guest,
                      none: data.byRole.none,
                    })
                  : undefined
              }
              unavailable={unavailable}
            />
            <StatTile
              icon={Activity}
              label={t('stats.health')}
              value={health ? t(`operations.status.${health.overall}`) : '—'}
              tone={clsx('capitalize', healthTone)}
              sub={health ? t('stats.healthSub') : t('stats.healthUnknown')}
              onClick={onShowOperations}
            />
          </>
        )}
      </div>
    </AdminSection>
  )
}

function SkeletonTiles({count}: {count: number}) {
  return (
    <>
      {Array.from({length: count}, (_, i) => (
        <div
          key={i}
          aria-hidden="true"
          className="stat-tile h-22 rounded-xl border border-border-subtle bg-surface-2 animate-pulse-soft"
        />
      ))}
    </>
  )
}
