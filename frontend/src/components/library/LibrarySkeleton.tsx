import type {LibraryViewMode} from '@/stores/libraryBrowseStore'

export default function LibrarySkeleton({view}: {view: LibraryViewMode}) {
  if (view === 'list') {
    return (
      <div className="library-list rounded-lg border border-border-default bg-surface-2 overflow-hidden animate-pulse">
        {Array.from({length: 8}).map((_, i) => (
          <div key={i} className="library-row px-4 py-3 border-b border-border-subtle last:border-b-0">
            <div className="h-3 rounded bg-surface-3" />
            <div className="library-col-owner h-3 rounded bg-surface-3" />
            <div className="h-3 rounded bg-surface-3" />
            <div className="h-3 rounded bg-surface-3" />
            <div className="library-col-subflows h-3 rounded bg-surface-3" />
          </div>
        ))}
      </div>
    )
  }
  return (
    <div className="library-grid animate-pulse">
      {Array.from({length: 8}).map((_, i) => (
        <div key={i} className="h-32 rounded-xl border border-border-default bg-surface-2" />
      ))}
    </div>
  )
}
