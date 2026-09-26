import {useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Filter, RotateCcw, ScrollText, X} from 'lucide-react'
import {Button, EmptyState, Input, Pager} from '@/components/shared'
import {SectionNotice} from '@/components/auth/SectionCard'
import {actionLabel} from '@/components/dashboard/home/activity'
import {useDebouncedSearch} from '@/hooks/useDebouncedSearch'
import {SEARCH_DEBOUNCE_MS} from '@/lib/constants'
import {AUDIT_PAGE_SIZES, useAdminStore} from '@/stores/adminStore'
import {AdminSection, SkeletonRows} from './AdminSection'
import {AuditRow} from './AuditRow'
import {groupActions} from './auditActions'
import {useAuditActions} from './hooks/useAuditActions'
import {useAuditLog} from './hooks/useAuditLog'

// The audit log: the action filter grouped by area with human labels, a user
// filter (also set by a row's user cell and by a user row's "View activity"),
// fifteen rows a page, and a details row per event for its metadata.
export function AuditLogSection() {
  const {t} = useTranslation(['admin', 'dashboard', 'common'])
  const filter = useAdminStore(s => s.audit)
  const setAuditAction = useAdminStore(s => s.setAuditAction)
  const setAuditUserId = useAdminStore(s => s.setAuditUserId)
  const setAuditPage = useAdminStore(s => s.setAuditPage)
  const setAuditPageSize = useAdminStore(s => s.setAuditPageSize)
  const clearAuditFilters = useAdminStore(s => s.clearAuditFilters)
  const {events, hasMore, error, isLoading, isRefetching, retry} = useAuditLog()
  const actions = useAuditActions()
  const labels = t('dashboard:actions', {returnObjects: true}) as Record<string, string>
  const groups = useMemo(
    () => groupActions(actions.data ?? (filter.action ? [filter.action] : [])),
    [actions.data, filter.action],
  )

  // The user box is local until the debounce (or Enter) commits it; a filter
  // set elsewhere lands in the box too, adjusted during render.
  const [userDraft, setUserDraft] = useState(filter.userId)
  const [lastUserId, setLastUserId] = useState(filter.userId)
  if (filter.userId !== lastUserId) {
    setLastUserId(filter.userId)
    setUserDraft(filter.userId)
  }
  const {search} = useDebouncedSearch({delay: SEARCH_DEBOUNCE_MS, onSearch: setAuditUserId})
  const filtersActive = filter.action !== '' || filter.userId !== ''

  return (
    <AdminSection id="admin-audit" title={t('sections.audit')} description={t('sectionSub.audit')}>
      <div className="overflow-hidden rounded-xl border border-border-default bg-surface-2">
        <div className="flex flex-wrap items-center gap-2 border-b border-border-subtle px-4 py-3">
          <Filter size={12} className="text-text-tertiary" aria-hidden="true" />
          <select
            value={filter.action}
            onChange={e => setAuditAction(e.target.value)}
            aria-label={t('audit.filterAction')}
            className="h-9 max-w-[16rem] rounded-md border border-border-default bg-surface-2 px-2 text-sm text-text-primary focus:outline-none focus:ring-2 focus:ring-brand-500/20"
          >
            <option value="">{t('audit.allActions')}</option>
            {groups.map(g => (
              <optgroup key={g.group} label={t(`audit.groups.${g.group}`)}>
                {g.actions.map(a => (
                  <option key={a} value={a}>
                    {actionLabel(labels, a)}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>
          <Input
            value={userDraft}
            onChange={e => {
              setUserDraft(e.target.value)
              search(e.target.value)
            }}
            onKeyDown={e => {
              if (e.key === 'Enter') setAuditUserId(userDraft.trim())
            }}
            placeholder={t('audit.userPlaceholder')}
            aria-label={t('audit.filterUser')}
            trailingIcon={userDraft ? X : undefined}
            trailingLabel={userDraft ? t('audit.clearUser') : undefined}
            onTrailingClick={() => {
              setUserDraft('')
              setAuditUserId('')
            }}
            className="w-48 font-mono"
          />
          {filtersActive && (
            <Button
              size="sm"
              variant="ghost"
              icon={RotateCcw}
              onClick={() => {
                setUserDraft('')
                clearAuditFilters()
              }}
            >
              {t('audit.clearFilters')}
            </Button>
          )}
        </div>

        {error && !events && (
          <div className="px-4 pt-3">
            <SectionNotice
              message={t('audit.loadFailed')}
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
                  {t('audit.columns.time')}
                </th>
                <th scope="col" className="px-4 py-2">
                  {t('audit.columns.user')}
                </th>
                <th scope="col" className="px-4 py-2">
                  {t('audit.columns.action')}
                </th>
                <th scope="col" className="px-4 py-2">
                  {t('audit.columns.resource')}
                </th>
                <th scope="col" className="px-4 py-2">
                  {t('audit.columns.ip')}
                </th>
                <th scope="col" className="px-2 py-2">
                  <span className="sr-only">{t('audit.columns.details')}</span>
                </th>
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
                <SkeletonRows rows={Math.min(filter.pageSize, 10)} cols={6} />
              ) : (
                events?.map(ev => <AuditRow key={ev.id} event={ev} labels={labels} onFilterUser={setAuditUserId} />)
              )}
            </tbody>
          </table>
          {events && events.length === 0 && (
            <EmptyState
              icon={ScrollText}
              title={filtersActive ? t('audit.noMatchTitle') : t('audit.emptyTitle')}
              description={filtersActive ? t('audit.noMatchBody') : t('audit.emptyBody')}
              action={
                filtersActive ? (
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => {
                      setUserDraft('')
                      clearAuditFilters()
                    }}
                  >
                    {t('audit.clearFilters')}
                  </Button>
                ) : undefined
              }
            />
          )}
        </div>

        {events && (
          <div className="border-t border-border-subtle px-4 py-2">
            <Pager
              page={filter.page}
              pageSize={filter.pageSize}
              count={events.length}
              hasMore={hasMore}
              onPageChange={setAuditPage}
              pageSizeOptions={AUDIT_PAGE_SIZES}
              onPageSizeChange={setAuditPageSize}
              label={t('audit.pager')}
            />
          </div>
        )}
      </div>
    </AdminSection>
  )
}
