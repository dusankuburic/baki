import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Search, Users, X} from 'lucide-react'
import type {RoleFilter, UserStats} from '@/api/admin'
import {Button, EmptyState, Input, Pager} from '@/components/shared'
import {SectionNotice} from '@/components/auth/SectionCard'
import {FilterChip} from '@/components/dashboard/analytics/FilterChip'
import {useDebouncedSearch} from '@/hooks/useDebouncedSearch'
import {SEARCH_DEBOUNCE_MS} from '@/lib/constants'
import {isTauri} from '@/platform/guards'
import {USERS_PAGE_SIZES, useAdminStore} from '@/stores/adminStore'
import {useAuthStore} from '@/stores/authStore'
import {AdminSection, SkeletonRows} from './AdminSection'
import {UserRow} from './UserRow'
import {ROLE_FILTERS, roleLabel} from './roles'
import {useAdminUsers} from './hooks/useAdminUsers'

// The user table: search (debounced) and role chips above, ten rows a page
// below, the role select on every row. Refetches keep the previous page on
// screen at reduced opacity; the counts on the chips come from the stats so
// they hold for the whole table, not the page.
export function UsersSection({stats}: {stats: UserStats | null}) {
  const {t} = useTranslation(['admin', 'common'])
  const list = useAdminStore(s => s.users)
  const setUsersQuery = useAdminStore(s => s.setUsersQuery)
  const setUsersRole = useAdminStore(s => s.setUsersRole)
  const setUsersPage = useAdminStore(s => s.setUsersPage)
  const setUsersPageSize = useAdminStore(s => s.setUsersPageSize)
  const clearUsersFilters = useAdminStore(s => s.clearUsersFilters)
  const focusUserActivity = useAdminStore(s => s.focusUserActivity)
  const currentUserId = useAuthStore(s => s.user?.id)
  const {data, error, isLoading, isRefetching, retry, changeRole, busyUserId} = useAdminUsers()

  // The search box is local until the debounce commits it to the store; a
  // filter cleared elsewhere (an overview tile, "Clear filters") empties the
  // box too. Adjusted during render, as React recommends for derived state.
  const [draft, setDraft] = useState(list.q)
  const [lastQuery, setLastQuery] = useState(list.q)
  if (list.q !== lastQuery) {
    setLastQuery(list.q)
    setDraft(list.q)
  }
  const {search} = useDebouncedSearch({delay: SEARCH_DEBOUNCE_MS, onSearch: setUsersQuery})

  const filtersActive = list.q !== '' || list.role !== ''
  const showActivity = !isTauri()
  const counts: Partial<Record<RoleFilter, number>> = stats
    ? {
        '': stats.total,
        admin: stats.byRole.admin,
        member: stats.byRole.member,
        viewer: stats.byRole.viewer,
        guest: stats.byRole.guest,
        none: stats.byRole.none,
      }
    : {}
  const columns = showActivity ? 7 : 6

  return (
    <AdminSection id="admin-users" title={t('sections.users')} description={t('sectionSub.users')}>
      <div className="overflow-hidden rounded-xl border border-border-default bg-surface-2">
        <div className="flex flex-wrap items-center gap-2 border-b border-border-subtle px-4 py-3">
          <Input
            icon={Search}
            value={draft}
            onChange={e => {
              setDraft(e.target.value)
              search(e.target.value)
            }}
            placeholder={t('users.search')}
            aria-label={t('users.search')}
            trailingIcon={draft ? X : undefined}
            trailingLabel={draft ? t('users.clearSearch') : undefined}
            onTrailingClick={() => {
              setDraft('')
              setUsersQuery('')
            }}
            className="w-full min-w-[12rem] flex-1 sm:max-w-sm"
          />
          <div className="analytics-chip-row" role="group" aria-label={t('users.roleFilter')}>
            {ROLE_FILTERS.map(r => {
              const label = r === '' ? t('users.roles.all') : roleLabel(t, r)
              const count = counts[r]
              return (
                <FilterChip
                  key={r || 'all'}
                  label={label}
                  count={count}
                  ariaLabel={count === undefined ? label : t('users.roleChip', {label, count})}
                  pressed={list.role === r}
                  disabled={r !== '' && count === 0}
                  onClick={() => setUsersRole(r)}
                />
              )
            })}
          </div>
          <p className="ml-auto text-xs tabular-nums text-text-tertiary" aria-live="polite">
            {data
              ? filtersActive
                ? t('users.countFiltered', {count: data.total})
                : t('users.count', {count: data.total})
              : ''}
          </p>
        </div>

        {error && !data && (
          <div className="px-4 pt-3">
            <SectionNotice
              message={t('users.loadFailed')}
              action={
                <Button size="sm" variant="ghost" onClick={retry}>
                  {t('common:retry')}
                </Button>
              }
            />
          </div>
        )}

        {/* relative: contains the absolutely positioned sr-only cells so the scroll wrapper clips them */}
        <div className="relative overflow-x-auto">
          <table className="min-w-full text-sm">
            <thead>
              <tr className="bg-surface-3 text-left text-2xs font-semibold uppercase tracking-wide text-text-tertiary">
                <th scope="col" className="px-4 py-2">
                  {t('users.columns.user')}
                </th>
                <th scope="col" className="px-4 py-2">
                  {t('users.columns.role')}
                </th>
                <th scope="col" className="admin-col-optional px-4 py-2">
                  {t('users.columns.joined')}
                </th>
                <th scope="col" className="px-4 py-2">
                  {t('users.columns.verified')}
                </th>
                <th scope="col" className="admin-col-optional px-4 py-2">
                  {t('users.columns.id')}
                </th>
                <th scope="col" className="px-4 py-2">
                  {t('users.columns.change')}
                </th>
                {showActivity && (
                  <th scope="col" className="px-4 py-2">
                    <span className="sr-only">{t('users.columns.actions')}</span>
                  </th>
                )}
              </tr>
            </thead>
            <tbody
              className={clsx(
                'divide-y divide-border-subtle transition-opacity duration-normal',
                isRefetching && 'opacity-60',
              )}
              aria-busy={isRefetching || undefined}
            >
              {isLoading ? (
                <SkeletonRows rows={Math.min(list.pageSize, 10)} cols={columns} />
              ) : (
                data?.items.map(u => (
                  <UserRow
                    key={u.id}
                    user={u}
                    isSelf={u.id === currentUserId}
                    busy={busyUserId === u.id}
                    showActivity={showActivity}
                    onChangeRole={role => void changeRole(u, role)}
                    onViewActivity={() => focusUserActivity(u.id)}
                  />
                ))
              )}
            </tbody>
          </table>
          {data && data.items.length === 0 && (
            <EmptyState
              icon={Users}
              title={filtersActive ? t('users.emptyTitle') : t('users.noUsersTitle')}
              description={filtersActive ? t('users.emptyBody') : t('users.noUsersBody')}
              action={
                filtersActive ? (
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => {
                      setDraft('')
                      clearUsersFilters()
                    }}
                  >
                    {t('users.clearFilters')}
                  </Button>
                ) : undefined
              }
            />
          )}
        </div>

        {data && (
          <div className="border-t border-border-subtle px-4 py-2">
            <Pager
              page={list.page}
              pageSize={list.pageSize}
              count={data.items.length}
              total={data.total}
              onPageChange={setUsersPage}
              pageSizeOptions={USERS_PAGE_SIZES}
              onPageSizeChange={setUsersPageSize}
              label={t('users.pager')}
            />
          </div>
        )}
      </div>
    </AdminSection>
  )
}
