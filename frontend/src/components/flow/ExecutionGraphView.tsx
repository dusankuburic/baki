import {Loader2, AlertCircle, AlertTriangle, Search} from 'lucide-react'
import {useTranslation} from 'react-i18next'
import {useCytoscapeGraph} from './useCytoscapeGraph'

export default function ExecutionGraphView({subflowId}: {subflowId?: string} = {}) {
  const {t} = useTranslation('flow')
  const {containerRef, loading, error, searchQuery, setSearchQuery, matchCount, handleSearch, retry} =
    useCytoscapeGraph(subflowId)

  return (
    <div className="flex-1 relative bg-surface-0 overflow-hidden">
      <form
        onSubmit={handleSearch}
        className="absolute top-6 left-6 z-20 flex items-center gap-2 p-1 bg-surface-1/80 backdrop-blur-md border border-border-default rounded-lg shadow-xl"
      >
        <div className="pl-2 text-text-tertiary">
          <Search size={14} />
        </div>
        <input
          type="text"
          placeholder={t('graph.findPlaceholder')}
          className="bg-transparent border-none outline-hidden text-xs w-48 h-8 placeholder:text-text-tertiary"
          value={searchQuery}
          onChange={e => setSearchQuery(e.target.value)}
        />
        {searchQuery.trim() && matchCount !== null && (
          <span className="pr-2 text-2xs text-text-tertiary tabular-nums shrink-0 select-none">
            {matchCount === 0 ? t('graph.noMatches') : t('graph.matches', {count: matchCount})}
          </span>
        )}
      </form>

      {loading && (
        <div className="absolute inset-0 z-10 flex items-center justify-center bg-surface-0/50 backdrop-blur-xs">
          <Loader2 className="animate-spin text-brand-500" size={32} />
        </div>
      )}

      {error && (
        <div className="absolute inset-0 z-10 flex items-center justify-center p-4">
          <div className="bg-semantic-error/10 border border-semantic-error/20 rounded-lg p-6 max-w-md text-center">
            <AlertCircle className="text-semantic-error mx-auto mb-4" size={48} />
            <h3 className="text-lg font-bold text-text-primary mb-2">{t('graph.errorTitle')}</h3>
            <p className="text-sm text-text-secondary mb-4">{error}</p>
            <button
              onClick={retry}
              className="px-4 py-2 bg-semantic-error text-white rounded-md hover:bg-semantic-error/90 transition-colors"
            >
              {t('graph.retry')}
            </button>
          </div>
        </div>
      )}

      <div ref={containerRef} className="absolute inset-0" />

      {!loading && !error && (
        <div className="absolute bottom-6 left-6 p-4 bg-surface-1/80 backdrop-blur-md border border-border-default rounded-xl shadow-lg pointer-events-none animate-slide-up">
          <h4 className="text-2xs font-black uppercase tracking-widest text-text-tertiary mb-3">
            {t('graph.legendTitle')}
          </h4>
          <div className="space-y-2">
            {subflowId && (
              <div className="flex items-center gap-3">
                <div className="w-3 h-3 rounded border-[3px] border-brand-500 bg-surface-2" />
                <span className="text-xs text-text-secondary font-medium">{t('graph.currentSubflow')}</span>
              </div>
            )}
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded bg-brand-500" />
              <span className="text-xs text-text-secondary font-medium">{t('graph.subflowNode')}</span>
            </div>
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded border border-semantic-error bg-semantic-error/10" />
              <span className="text-xs text-text-secondary font-medium flex items-center gap-1">
                <AlertCircle size={10} className="text-semantic-error" /> {t('graph.subflowErrors')}
              </span>
            </div>
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded border border-semantic-warning bg-semantic-warning/10" />
              <span className="text-xs text-text-secondary font-medium flex items-center gap-1">
                <AlertTriangle size={10} className="text-semantic-warning" /> {t('graph.subflowWarnings')}
              </span>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
