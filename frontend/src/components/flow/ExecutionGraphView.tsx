import {Loader2, AlertCircle, AlertTriangle, Search} from 'lucide-react'
import {useCytoscapeGraph} from './useCytoscapeGraph'

export default function ExecutionGraphView({subflowId}: {subflowId?: string} = {}) {
  const {containerRef, loading, error, searchQuery, setSearchQuery, matchCount, handleSearch, retry} =
    useCytoscapeGraph(subflowId)

  return (
    <div className="flex-1 relative bg-surface-0 overflow-hidden">
      {/* Search Overlay */}
      <form
        onSubmit={handleSearch}
        className="absolute top-6 left-6 z-20 flex items-center gap-2 p-1 bg-surface-1/80 backdrop-blur-md border border-border-default rounded-lg shadow-xl"
      >
        <div className="pl-2 text-text-tertiary">
          <Search size={14} />
        </div>
        <input
          type="text"
          placeholder="Find subflow..."
          className="bg-transparent border-none outline-none text-xs w-48 h-8 placeholder:text-text-tertiary"
          value={searchQuery}
          onChange={e => setSearchQuery(e.target.value)}
        />
        {searchQuery.trim() && matchCount !== null && (
          <span className="pr-2 text-2xs text-text-tertiary tabular-nums shrink-0 select-none">
            {matchCount === 0 ? 'No matches' : `${matchCount} match${matchCount === 1 ? '' : 'es'}`}
          </span>
        )}
      </form>

      {loading && (
        <div className="absolute inset-0 z-10 flex items-center justify-center bg-surface-0/50 backdrop-blur-sm">
          <Loader2 className="animate-spin text-brand-500" size={32} />
        </div>
      )}

      {error && (
        <div className="absolute inset-0 z-10 flex items-center justify-center p-4">
          <div className="bg-semantic-error/10 border border-semantic-error/20 rounded-lg p-6 max-w-md text-center">
            <AlertCircle className="text-semantic-error mx-auto mb-4" size={48} />
            <h3 className="text-lg font-bold text-text-primary mb-2">Graph Error</h3>
            <p className="text-sm text-text-secondary mb-4">{error}</p>
            <button
              onClick={retry}
              className="px-4 py-2 bg-semantic-error text-white rounded-md hover:bg-semantic-error/90 transition-colors"
            >
              Retry
            </button>
          </div>
        </div>
      )}

      <div ref={containerRef} className="absolute inset-0" />

      {/* Legend / Stats overlay */}
      {!loading && !error && (
        <div className="absolute bottom-6 left-6 p-4 bg-surface-1/80 backdrop-blur-md border border-border-default rounded-xl shadow-lg pointer-events-none animate-slide-up">
          <h4 className="text-2xs font-black uppercase tracking-widest text-text-tertiary mb-3">Subflow Map Legend</h4>
          <div className="space-y-2">
            {subflowId && (
              <div className="flex items-center gap-3">
                <div className="w-3 h-3 rounded border-[3px] border-brand-500 bg-surface-2" />
                <span className="text-xs text-text-secondary font-medium">Current Subflow</span>
              </div>
            )}
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded bg-brand-500" />
              <span className="text-xs text-text-secondary font-medium">Subflow Node (Dbl-click to open)</span>
            </div>
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded border border-semantic-error bg-semantic-error/10" />
              <span className="text-xs text-text-secondary font-medium flex items-center gap-1">
                <AlertCircle size={10} className="text-semantic-error" /> Subflow with Errors
              </span>
            </div>
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded border border-semantic-warning bg-semantic-warning/10" />
              <span className="text-xs text-text-secondary font-medium flex items-center gap-1">
                <AlertTriangle size={10} className="text-semantic-warning" /> Subflow with Warnings
              </span>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
