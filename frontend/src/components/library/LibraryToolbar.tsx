import {Search, LayoutGrid, List, RefreshCw} from 'lucide-react'
import clsx from 'clsx'
import Input from '@/components/shared/Input'
import IconButton from '@/components/shared/IconButton'
import {useLibraryBrowseStore} from '@/stores/libraryBrowseStore'
import type {LibrarySort} from '@/api/library'

const SORT_OPTIONS: {value: LibrarySort; label: string}[] = [
  {value: 'updated_desc', label: 'Recently updated'},
  {value: 'updated_asc', label: 'Oldest updated'},
  {value: 'name_asc', label: 'Name A → Z'},
  {value: 'name_desc', label: 'Name Z → A'},
  {value: 'blocks_desc', label: 'Most blocks'},
]

export default function LibraryToolbar({
  total,
  isLoading,
  onRefresh,
}: {
  total: number
  isLoading: boolean
  onRefresh: () => void
}) {
  const view = useLibraryBrowseStore(s => s.view)
  const setView = useLibraryBrowseStore(s => s.setView)
  const sort = useLibraryBrowseStore(s => s.sort)
  const setSort = useLibraryBrowseStore(s => s.setSort)
  const query = useLibraryBrowseStore(s => s.query)
  const setQuery = useLibraryBrowseStore(s => s.setQuery)

  return (
    // Wraps rather than squeezing the search box to nothing in a narrow pane.
    <div className="flex flex-wrap items-center gap-2 px-4 py-3 border-b border-border-subtle flex-shrink-0">
      <Input
        icon={Search}
        placeholder="Search flows by name…"
        value={query}
        onChange={e => setQuery(e.target.value)}
        className="flex-1 min-w-[10rem] max-w-md"
      />
      <select
        value={sort}
        onChange={e => setSort(e.target.value as LibrarySort)}
        className="h-9 px-2 text-sm bg-surface-2 border border-border-default rounded-md text-text-primary focus:outline-none focus:ring-2 focus:ring-brand-500/20"
        aria-label="Sort flows"
      >
        {SORT_OPTIONS.map(o => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      <div className="inline-flex rounded-md border border-border-default overflow-hidden">
        <ViewToggle active={view === 'grid'} onClick={() => setView('grid')} icon={LayoutGrid} label="Grid view" />
        <ViewToggle active={view === 'list'} onClick={() => setView('list')} icon={List} label="List view" />
      </div>
      <div className="ml-auto flex items-center gap-1">
        <span className="text-xs text-text-tertiary tabular-nums" aria-live="polite">
          {isLoading ? 'loading…' : `${total} flow${total === 1 ? '' : 's'}`}
        </span>
        <IconButton icon={RefreshCw} size="sm" label="Refresh" onClick={onRefresh} disabled={isLoading} />
      </div>
    </div>
  )
}

function ViewToggle({
  active,
  onClick,
  icon: Icon,
  label,
}: {
  active: boolean
  onClick: () => void
  icon: typeof LayoutGrid
  label: string
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      aria-pressed={active}
      className={clsx(
        'h-9 w-9 inline-flex items-center justify-center transition-colors',
        active
          ? 'bg-brand-500/15 text-brand-400'
          : 'bg-surface-2 text-text-tertiary hover:text-text-primary hover:bg-surface-3',
      )}
    >
      <Icon size={14} />
    </button>
  )
}
