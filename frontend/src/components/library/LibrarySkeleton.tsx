import type {LibraryViewMode} from '@/stores/libraryBrowseStore'

export default function LibrarySkeleton({view}: {view: LibraryViewMode}) {
  if (view === 'list') {
    return (
      <div className="rounded-lg border border-border-default bg-surface-2 overflow-hidden animate-pulse">
        {Array.from({length: 8}).map((_, i) => (
          <div
            key={i}
            className="grid grid-cols-[1fr_120px_140px_80px_80px] gap-3 px-4 py-3 border-b border-border-subtle last:border-b-0"
          >
            <div className="h-3 rounded bg-surface-3" />
            <div className="h-3 rounded bg-surface-3" />
            <div className="h-3 rounded bg-surface-3" />
            <div className="h-3 rounded bg-surface-3" />
            <div className="h-3 rounded bg-surface-3" />
          </div>
        ))}
      </div>
    )
  }
  return (
    <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4 gap-3 animate-pulse">
      {Array.from({length: 8}).map((_, i) => (
        <div key={i} className="h-32 rounded-xl border border-border-default bg-surface-2" />
      ))}
    </div>
  )
}
