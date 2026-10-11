import {Globe, User, Share2, Building2, Tag} from 'lucide-react'
import clsx from 'clsx'
import {useTranslation} from 'react-i18next'
import {useLibraryBrowseStore} from '@/stores/libraryBrowseStore'
import type {LibraryScope} from '@/api/library'
import type {Organisation} from '@/stores/orgStore'

interface LibraryFilterRailProps {
  orgs: Organisation[]
  tags: string[]
}

const SCOPES: {value: LibraryScope; label: 'rail.scopeAll' | 'rail.scopeMine' | 'rail.scopeShared'; icon: typeof Globe}[] =
  [
    {value: 'all', label: 'rail.scopeAll', icon: Globe},
    {value: 'mine', label: 'rail.scopeMine', icon: User},
    {value: 'shared', label: 'rail.scopeShared', icon: Share2},
  ]

export default function LibraryFilterRail({orgs, tags}: LibraryFilterRailProps) {
  const {t} = useTranslation('library')
  const scope = useLibraryBrowseStore(s => s.scope)
  const setScope = useLibraryBrowseStore(s => s.setScope)
  const selectedOrgIds = useLibraryBrowseStore(s => s.selectedOrgIds)
  const toggleOrg = useLibraryBrowseStore(s => s.toggleOrg)
  const setSelectedOrgIds = useLibraryBrowseStore(s => s.setSelectedOrgIds)
  const selectedTags = useLibraryBrowseStore(s => s.selectedTags)
  const toggleTag = useLibraryBrowseStore(s => s.toggleTag)
  const clearTags = useLibraryBrowseStore(s => s.clearTags)
  const tagChips = [...new Set([...tags, ...selectedTags])].sort()

  const isAllOrgs = selectedOrgIds === null
  const isOrgChecked = (id: string) => isAllOrgs || (selectedOrgIds?.has(id) ?? false)

  return (
    <nav className="p-3 space-y-4">
      <div>
        <div className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary mb-1.5 px-2">
          {t('rail.scopeHeading')}
        </div>
        <ul className="space-y-0.5">
          {SCOPES.map(s => (
            <li key={s.value}>
              <button
                type="button"
                onClick={() => setScope(s.value)}
                className={clsx(
                  'w-full flex items-center gap-2 px-2 py-1.5 rounded-md text-xs transition-colors',
                  scope === s.value
                    ? 'bg-brand-500/15 text-brand-400 font-medium'
                    : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary',
                )}
              >
                <s.icon size={13} />
                {t(s.label)}
              </button>
            </li>
          ))}
        </ul>
      </div>

      <div>
        <div className="flex items-center justify-between mb-1.5 px-2">
          <span className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary">
            {t('rail.orgsHeading')}
          </span>
          {!isAllOrgs && (
            <button
              type="button"
              onClick={() => setSelectedOrgIds(null)}
              className="text-2xs text-brand-400 hover:underline"
            >
              {t('rail.allOrgs')}
            </button>
          )}
        </div>
        <ul className="space-y-0.5">
          <OrgRow id="" label={t('rail.personal')} checked={isOrgChecked('')} onToggle={toggleOrg} />
          {orgs.map(o => (
            <OrgRow key={o.id} id={o.id} label={o.name} checked={isOrgChecked(o.id)} onToggle={toggleOrg} />
          ))}
          {orgs.length === 0 && (
            <li className="px-2 py-1.5 text-2xs text-text-tertiary italic">{t('rail.noOrgs')}</li>
          )}
        </ul>
      </div>

      <div>
        <div className="flex items-center justify-between mb-1.5 px-2">
          <span className="text-2xs font-semibold uppercase tracking-wider text-text-tertiary">
            {t('rail.tagsHeading')}
          </span>
          {selectedTags.size > 0 && (
            <button type="button" onClick={clearTags} className="text-2xs text-brand-400 hover:underline">
              {t('rail.clearTags')}
            </button>
          )}
        </div>
        {tagChips.length === 0 ? (
          <p className="px-2 py-1.5 text-2xs text-text-tertiary italic">{t('rail.noTaggedFlows')}</p>
        ) : (
          <ul className="flex flex-wrap gap-1 px-2" aria-label={t('rail.filterByTagAria')}>
            {tagChips.map(tag => {
              const active = selectedTags.has(tag)
              return (
                <li key={tag}>
                  <button
                    type="button"
                    onClick={() => toggleTag(tag)}
                    aria-pressed={active}
                    className={clsx(
                      'inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-2xs font-medium transition-colors',
                      active
                        ? 'bg-brand-500/20 text-brand-300 ring-1 ring-brand-500/40'
                        : 'bg-surface-2 text-text-secondary hover:bg-surface-3 hover:text-text-primary',
                    )}
                  >
                    <Tag size={9} />
                    {tag}
                  </button>
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </nav>
  )
}

function OrgRow({
  id,
  label,
  checked,
  onToggle,
}: {
  id: string
  label: string
  checked: boolean
  onToggle: (id: string) => void
}) {
  return (
    <li>
      <label className="flex items-center gap-2 px-2 py-1.5 rounded-md text-xs text-text-secondary hover:bg-surface-2 cursor-pointer">
        <input type="checkbox" checked={checked} onChange={() => onToggle(id)} className="accent-brand-500 w-3 h-3" />
        <Building2 size={12} className="text-text-tertiary" />
        <span className="truncate">{label}</span>
      </label>
    </li>
  )
}
