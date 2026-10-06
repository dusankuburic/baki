import {useMemo, useRef, type KeyboardEvent, type RefObject} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Search, X} from 'lucide-react'
import Input from '@/components/shared/Input'
import {useSystemStore} from '@/stores/systemStore'
import type {SettingsSection} from '@/stores/uiStore'
import {SETTINGS_SECTIONS, matchesQuery, type SettingsGroup} from './sections'

// The Settings sidebar: title, the section search, the grouped section tabs
// and the pinned footer (About + version). One tablist holds every tab, so
// arrow keys walk the whole list and the footer is simply its last entry;
// the desktop/phone layouts are CSS only (index.css, "SETTINGS MODAL").
export default function SettingsNav({
  groups,
  footer,
  active,
  onSelect,
  query,
  onQueryChange,
  selectedTabRef,
}: {
  // Already filtered for the platform (cloud-only groups and sections gone).
  groups: SettingsGroup[]
  footer: readonly SettingsSection[]
  active: SettingsSection
  onSelect: (id: SettingsSection) => void
  query: string
  onQueryChange: (query: string) => void
  // Attached to the selected tab so the dialog can focus it on open.
  selectedTabRef: RefObject<HTMLButtonElement | null>
}) {
  const {t} = useTranslation('settings')
  const version = useSystemStore(s => s.info?.version)
  const listRef = useRef<HTMLDivElement>(null)

  // What the search box matches against, per section: its label, description
  // and group, plus the keywords from en.settings.ts (never rendered).
  const haystack = useMemo(() => {
    const out = {} as Record<SettingsSection, string>
    const add = (id: SettingsSection, group: string) => {
      out[id] = [
        t(`modal.sections.${id}.label`),
        t(`modal.sections.${id}.description`),
        group,
        t(`modal.sections.${id}.keywords`),
      ].join(' ')
    }
    for (const group of groups) for (const id of group.sections) add(id, t(`modal.groups.${group.id}`))
    for (const id of footer) add(id, '')
    return out
  }, [t, groups, footer])

  const isVisible = (id: SettingsSection) => matchesQuery(haystack[id], query)
  const visibleGroups = groups
    .map(group => ({...group, sections: group.sections.filter(isVisible)}))
    .filter(group => group.sections.length > 0)
  const visibleFooter = footer.filter(isVisible)
  const visibleIds = [...visibleGroups.flatMap(group => group.sections), ...visibleFooter]
  // Roving tabindex: the selected tab is the one tab stop, or the first
  // visible tab when the search hides the selection.
  const tabbableId = visibleIds.includes(active) ? active : visibleIds[0]

  const focusTab = (id: SettingsSection) =>
    listRef.current?.querySelector<HTMLButtonElement>(`[data-tab="${id}"]`)?.focus()
  // Selection follows focus, the shared Tabs contract.
  const go = (id: SettingsSection | undefined) => {
    if (!id) return
    onSelect(id)
    focusTab(id)
  }

  const onListKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const count = visibleIds.length
    if (count === 0) return
    const current = (e.target as HTMLElement).dataset.tab as SettingsSection | undefined
    const index = visibleIds.indexOf(current ?? active)
    switch (e.key) {
      case 'ArrowDown':
      case 'ArrowRight':
        e.preventDefault()
        go(visibleIds[index === -1 ? 0 : (index + 1) % count])
        break
      case 'ArrowUp':
      case 'ArrowLeft':
        e.preventDefault()
        go(visibleIds[index === -1 ? count - 1 : (index - 1 + count) % count])
        break
      case 'Home':
        e.preventDefault()
        go(visibleIds[0])
        break
      case 'End':
        e.preventDefault()
        go(visibleIds[count - 1])
        break
    }
  }

  const onSearchKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Escape' && query) {
      // preventDefault is what keeps useDialogFocus from closing the dialog
      // on the same keypress: one Escape clears the query, the next closes.
      e.preventDefault()
      onQueryChange('')
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (visibleIds[0]) onSelect(visibleIds[0])
    } else if (e.key === 'ArrowDown') {
      e.preventDefault()
      go(tabbableId)
    }
  }

  const renderTab = (id: SettingsSection) => {
    const selected = id === active
    const Icon = SETTINGS_SECTIONS[id].icon
    return (
      <button
        key={id}
        type="button"
        role="tab"
        id={`settings-tab-${id}`}
        data-tab={id}
        aria-selected={selected}
        aria-controls="settings-panel"
        tabIndex={id === tabbableId ? 0 : -1}
        ref={selected ? selectedTabRef : undefined}
        onClick={() => onSelect(id)}
        className={clsx(
          'settings-nav-item flex w-full items-center gap-2.5 rounded-md px-3 py-1.5 text-left text-sm font-medium transition-colors duration-fast',
          'focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40',
          selected
            ? 'bg-brand-500/10 text-brand-400'
            : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary',
        )}
      >
        <Icon
          size={16}
          aria-hidden="true"
          className={clsx('shrink-0', selected ? 'text-brand-400' : 'text-text-tertiary')}
        />
        <span className="min-w-0 truncate">{t(`modal.sections.${id}.label`)}</span>
      </button>
    )
  }

  return (
    <aside className="settings-nav border-r border-border-subtle">
      <div className="settings-nav-head px-3 pb-2 pt-5">
        <h2 className="px-3 text-lg font-semibold text-text-primary">{t('modal.ui.title')}</h2>
        <Input
          className="mt-3"
          icon={Search}
          value={query}
          onChange={e => onQueryChange(e.target.value)}
          onKeyDown={onSearchKeyDown}
          placeholder={t('modal.ui.search')}
          aria-label={t('modal.ui.search')}
          autoComplete="off"
          spellCheck={false}
          trailingIcon={query ? X : undefined}
          trailingLabel={t('modal.ui.clearSearch')}
          onTrailingClick={() => onQueryChange('')}
        />
      </div>
      <div
        ref={listRef}
        role="tablist"
        aria-orientation="vertical"
        aria-label={t('modal.ui.nav')}
        onKeyDown={onListKeyDown}
        className="settings-nav-scroll px-3 pb-3"
      >
        {visibleGroups.map(group => (
          // Padding sits on the group, not the heading: every heading is the
          // first child of its own group, so `first:` on it would hit all.
          <div key={group.id} role="presentation" className="settings-nav-group pt-4 first:pt-1">
            <div
              role="presentation"
              className="settings-nav-heading px-3 pb-1 text-2xs font-bold uppercase tracking-widest text-text-tertiary"
            >
              {t(`modal.groups.${group.id}`)}
            </div>
            {group.sections.map(renderTab)}
          </div>
        ))}
        {visibleIds.length === 0 && (
          <p role="status" className="px-3 py-6 text-center text-sm text-text-tertiary">
            {t('modal.ui.noResults', {query: query.trim()})}
          </p>
        )}
        <div role="presentation" className="settings-nav-footer border-t border-border-subtle bg-surface-1 pt-2">
          {visibleFooter.map(renderTab)}
          {version && (
            <p className="settings-nav-version px-3 pt-1 text-2xs tabular-nums text-text-tertiary">
              {t('modal.ui.version', {version})}
            </p>
          )}
        </div>
      </div>
    </aside>
  )
}
