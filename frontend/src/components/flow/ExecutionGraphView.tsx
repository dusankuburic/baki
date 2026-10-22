import {useCallback, useState} from 'react'
import {
  Loader2,
  AlertCircle,
  AlertTriangle,
  Search,
  Maximize2,
  Compass,
  Boxes,
  Network,
  ArrowRight,
  ArrowDown,
  Route,
  Info,
  X,
  ExternalLink,
  CheckCircle2,
} from 'lucide-react'
import {useTranslation} from 'react-i18next'
import FlowCanvas from '@/components/graph/flow/FlowCanvas'
import {flowEdgeTypes} from '@/components/graph/flow/edges'
import {useSubflowMapGraph} from './subflowMap/useSubflowMapGraph'
import SubflowCard, {SUBFLOW_CARD_TYPE} from './subflowMap/SubflowCard'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {roleConfig} from './subflowMap/roles'

const nodeTypes = {[SUBFLOW_CARD_TYPE]: SubflowCard}

export default function ExecutionGraphView({subflowId}: {subflowId?: string} = {}) {
  const {t} = useTranslation('flow')
  const [showLegend, setShowLegend] = useState(true)

  const subflowMapLayout = useUIStore(s => s.subflowMapLayout)
  const setSubflowMapLayout = useUIStore(s => s.setSubflowMapLayout)
  const subflowMapDirection = useUIStore(s => s.subflowMapDirection)
  const setSubflowMapDirection = useUIStore(s => s.setSubflowMapDirection)
  const subflowMapRouting = useUIStore(s => s.subflowMapRouting)
  const setSubflowMapRouting = useUIStore(s => s.setSubflowMapRouting)
  const selectSubflow = useFlowStore(s => s.selectSubflow)

  const {
    nodes,
    edges,
    loading,
    error,
    searchQuery,
    setSearchQuery,
    matchCount,
    handleSearch,
    retry,
    focusedNode,
    focusedEdge,
    clearFocus,
    fitToScreen,
    onNodeClick,
    onNodeDoubleClick,
    onEdgeClick,
    focusIds,
    focusToken,
    fitSignal,
  } = useSubflowMapGraph(subflowId)

  const exportFileName = useCallback(() => `${subflowId ?? 'subflow'}-map.png`, [subflowId])

  return (
    <div className="flex-1 relative bg-surface-0 overflow-hidden bg-[radial-gradient(circle_at_1px_1px,rgba(148,163,184,0.18)_1px,transparent_0)] [background-size:20px_20px]">
      {/* Top Floating Control Bar */}
      <div className="absolute top-4 left-4 z-20 flex flex-wrap items-center gap-2 max-w-[calc(100%-2rem)]">
        {/* Search Pill */}
        <form
          onSubmit={handleSearch}
          className="flex items-center gap-1.5 px-2.5 h-9 bg-surface-1/90 backdrop-blur-md border border-border-default rounded-lg shadow-md shrink-0"
        >
          <Search size={14} className="text-text-tertiary shrink-0" />
          <input
            type="text"
            placeholder={t('graph.findPlaceholder')}
            className="bg-transparent border-none outline-hidden text-xs w-36 sm:w-44 h-full placeholder:text-text-tertiary text-text-primary"
            value={searchQuery}
            onChange={e => setSearchQuery(e.target.value)}
          />
          {searchQuery.trim() && (
            <>
              {matchCount !== null && (
                <span className="text-2xs text-text-tertiary tabular-nums shrink-0 select-none px-1">
                  {matchCount === 0 ? t('graph.noMatches') : t('graph.matches', {count: matchCount})}
                </span>
              )}
              <button
                type="button"
                onClick={() => setSearchQuery('')}
                className="text-text-tertiary hover:text-text-primary p-0.5"
              >
                <X size={12} />
              </button>
            </>
          )}
        </form>

        {/* Layout Switcher Segmented Control */}
        <div className="flex items-center p-0.5 bg-surface-1/90 backdrop-blur-md border border-border-default rounded-lg shadow-md shrink-0">
          <button
            type="button"
            onClick={() => setSubflowMapLayout('hierarchy')}
            className={`flex items-center gap-1 px-2.5 h-8 text-xs font-medium rounded-md transition-all ${
              subflowMapLayout === 'hierarchy'
                ? 'bg-brand-500 text-white shadow-xs'
                : 'text-text-secondary hover:text-text-primary hover:bg-surface-2/60'
            }`}
            title={t('graph.layoutHierarchy')}
          >
            <Network size={13} />
            <span className="hidden sm:inline">{t('graph.layoutHierarchy')}</span>
          </button>

          <button
            type="button"
            onClick={() => setSubflowMapLayout('radial')}
            className={`flex items-center gap-1 px-2.5 h-8 text-xs font-medium rounded-md transition-all ${
              subflowMapLayout === 'radial'
                ? 'bg-brand-500 text-white shadow-xs'
                : 'text-text-secondary hover:text-text-primary hover:bg-surface-2/60'
            }`}
            title={t('graph.layoutRadial')}
          >
            <Compass size={13} />
            <span className="hidden sm:inline">{t('graph.layoutRadial')}</span>
          </button>

          <button
            type="button"
            onClick={() => setSubflowMapLayout('cluster')}
            className={`flex items-center gap-1 px-2.5 h-8 text-xs font-medium rounded-md transition-all ${
              subflowMapLayout === 'cluster'
                ? 'bg-brand-500 text-white shadow-xs'
                : 'text-text-secondary hover:text-text-primary hover:bg-surface-2/60'
            }`}
            title={t('graph.layoutCluster')}
          >
            <Boxes size={13} />
            <span className="hidden sm:inline">{t('graph.layoutCluster')}</span>
          </button>
        </div>

        {/* Hierarchy Options (Direction & Corridor routing) */}
        {subflowMapLayout === 'hierarchy' && (
          <div className="flex items-center gap-1 p-0.5 bg-surface-1/90 backdrop-blur-md border border-border-default rounded-lg shadow-md shrink-0">
            {/* Direction toggle */}
            <button
              type="button"
              onClick={() => setSubflowMapDirection(subflowMapDirection === 'LR' ? 'TB' : 'LR')}
              className="flex items-center gap-1 px-2 h-8 text-xs font-medium text-text-secondary hover:text-text-primary hover:bg-surface-2/60 rounded-md transition-colors"
              title={subflowMapDirection === 'LR' ? t('graph.directionLR') : t('graph.directionTB')}
            >
              {subflowMapDirection === 'LR' ? (
                <>
                  <ArrowRight size={13} />
                  <span className="text-2xs font-semibold">LR</span>
                </>
              ) : (
                <>
                  <ArrowDown size={13} />
                  <span className="text-2xs font-semibold">TB</span>
                </>
              )}
            </button>

            {/* Corridor Routing toggle */}
            <button
              type="button"
              onClick={() =>
                setSubflowMapRouting(subflowMapRouting === 'corridor' ? 'straight' : 'corridor')
              }
              className={`flex items-center gap-1 px-2 h-8 text-xs font-medium rounded-md transition-colors ${
                subflowMapRouting === 'corridor'
                  ? 'text-brand-600 dark:text-brand-400 bg-brand-500/10 font-semibold'
                  : 'text-text-secondary hover:text-text-primary hover:bg-surface-2/60'
              }`}
              title={
                subflowMapRouting === 'corridor'
                  ? t('graph.corridorActive')
                  : t('graph.corridorInactive')
              }
            >
              <Route size={13} />
              <span className="hidden md:inline text-2xs">
                {subflowMapRouting === 'corridor' ? t('graph.corridorActive') : t('graph.corridorInactive')}
              </span>
            </button>
          </div>
        )}

        {/* Actions: Fit to screen & Legend toggle */}
        <div className="flex items-center gap-1 p-0.5 bg-surface-1/90 backdrop-blur-md border border-border-default rounded-lg shadow-md shrink-0">
          <button
            type="button"
            onClick={fitToScreen}
            className="p-1.5 text-text-secondary hover:text-text-primary hover:bg-surface-2/60 rounded-md transition-colors"
            title={t('graph.fitToScreen')}
          >
            <Maximize2 size={15} />
          </button>
          <button
            type="button"
            onClick={() => setShowLegend(prev => !prev)}
            className={`p-1.5 rounded-md transition-colors ${
              showLegend
                ? 'text-brand-600 dark:text-brand-400 bg-brand-500/10'
                : 'text-text-secondary hover:text-text-primary hover:bg-surface-2/60'
            }`}
            title={t('graph.toggleLegend')}
          >
            <Info size={15} />
          </button>
        </div>
      </div>

      {/* Loading Overlay */}
      {loading && (
        <div className="absolute inset-0 z-10 flex items-center justify-center bg-surface-0/50 backdrop-blur-xs">
          <Loader2 className="animate-spin text-brand-500" size={32} />
        </div>
      )}

      {/* Error Overlay */}
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

      {/* Graph surface */}
      <FlowCanvas
        className="absolute inset-0"
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        edgeTypes={flowEdgeTypes}
        minZoom={0.1}
        maxZoom={2.5}
        fitPadding="50px"
        fitSignal={fitSignal}
        syncZoom
        exportFileName={exportFileName}
        focusIds={focusIds}
        focusToken={focusToken}
        onNodeClick={onNodeClick}
        onNodeDoubleClick={onNodeDoubleClick}
        onEdgeClick={onEdgeClick}
        onPaneClick={clearFocus}
      />

      {/* Focused Node Side Card (Top Right) */}
      {focusedNode && (
        <div className="absolute top-4 right-4 z-20 w-72 bg-surface-1/95 backdrop-blur-md border border-border-default rounded-xl shadow-xl p-4 animate-slide-left space-y-3">
          <div className="flex items-start justify-between gap-2">
            <div>
              <span
                className="inline-block px-2 py-0.5 text-2xs font-bold rounded-md uppercase tracking-wider mb-1"
                style={{
                  color: roleConfig(focusedNode.role).color,
                  backgroundColor: roleConfig(focusedNode.role).bgDark,
                }}
              >
                {t(roleConfig(focusedNode.role).badgeKey)}
              </span>
              <h3 className="text-sm font-bold text-text-primary break-words">
                {focusedNode.label}
              </h3>
            </div>
            <button
              type="button"
              onClick={clearFocus}
              className="p-1 text-text-tertiary hover:text-text-primary hover:bg-surface-2 rounded-md transition-colors"
            >
              <X size={14} />
            </button>
          </div>

          <div className="grid grid-cols-2 gap-2 text-2xs text-text-secondary bg-surface-2/50 p-2.5 rounded-lg border border-border-default/60">
            <div>
              <span className="text-text-tertiary block">{t('graph.blocksCount')}</span>
              <span className="font-semibold text-text-primary">{focusedNode.blockCount}</span>
            </div>
            <div>
              <span className="text-text-tertiary block">{t('graph.hierarchyDepth')}</span>
              <span className="font-semibold text-text-primary">
                {t('graph.levelCount', {depth: focusedNode.depth})}
              </span>
            </div>
            <div>
              <span className="text-text-tertiary block">{t('graph.callersIn')}</span>
              <span className="font-semibold text-text-primary">{focusedNode.inDegree}</span>
            </div>
            <div>
              <span className="text-text-tertiary block">{t('graph.callsOut')}</span>
              <span className="font-semibold text-text-primary">{focusedNode.outDegree}</span>
            </div>
          </div>

          {/* Health Badge */}
          <div className="flex items-center gap-2 text-xs">
            {focusedNode.errorCount > 0 ? (
              <span className="flex items-center gap-1 text-semantic-error font-medium">
                <AlertCircle size={14} /> {t('graph.errorsCount', {count: focusedNode.errorCount})}
              </span>
            ) : focusedNode.warnCount > 0 ? (
              <span className="flex items-center gap-1 text-semantic-warning font-medium">
                <AlertTriangle size={14} /> {t('graph.warningsCount', {count: focusedNode.warnCount})}
              </span>
            ) : (
              <span className="flex items-center gap-1 text-semantic-success font-medium">
                <CheckCircle2 size={14} /> {t('graph.cleanNoIssues')}
              </span>
            )}
          </div>

          <button
            type="button"
            onClick={() => selectSubflow(focusedNode.id)}
            className="w-full flex items-center justify-center gap-2 py-1.5 px-3 bg-brand-500 hover:bg-brand-600 text-white text-xs font-semibold rounded-lg shadow-xs transition-colors"
          >
            <ExternalLink size={13} />
            {t('graph.openSubflow')}
          </button>
        </div>
      )}

      {/* Focused Edge Side Card (Top Right) */}
      {focusedEdge && (
        <div className="absolute top-4 right-4 z-20 w-72 bg-surface-1/95 backdrop-blur-md border border-border-default rounded-xl shadow-xl p-4 animate-slide-left space-y-3">
          <div className="flex items-start justify-between gap-2">
            <div>
              <span className="text-2xs font-bold uppercase tracking-wider text-text-tertiary">
                {t('graph.subflowCallTitle')}
              </span>
              <h3 className="text-sm font-semibold text-text-primary break-words mt-0.5">
                {focusedEdge.source} → {focusedEdge.target}
              </h3>
            </div>
            <button
              type="button"
              onClick={clearFocus}
              className="p-1 text-text-tertiary hover:text-text-primary hover:bg-surface-2 rounded-md transition-colors"
            >
              <X size={14} />
            </button>
          </div>

          <div className="space-y-1.5 text-xs">
            <div className="flex justify-between py-1 border-b border-border-default/50 text-text-secondary">
              <span>{t('graph.totalCallSites')}</span>
              <span className="font-semibold text-text-primary">{focusedEdge.callCount}</span>
            </div>
            <div className="flex justify-between py-1 border-b border-border-default/50 text-text-secondary">
              <span>{t('graph.activeCalls')}</span>
              <span className="font-semibold text-semantic-success">{focusedEdge.enabledCount}</span>
            </div>
            {focusedEdge.disabledCount > 0 && (
              <div className="flex justify-between py-1 text-semantic-warning">
                <span>{t('graph.disabledCallsLabel')}</span>
                <span className="font-semibold">{focusedEdge.disabledCount}</span>
              </div>
            )}
            {focusedEdge.isFullyDisabled && (
              <p className="text-2xs text-text-tertiary italic pt-1">
                {t('graph.allCallsDisabled')}
              </p>
            )}
          </div>
        </div>
      )}

      {/* Bottom Collapsible Legend */}
      {!loading && !error && showLegend && (
        <div className="absolute bottom-4 left-4 p-3.5 bg-surface-1/90 backdrop-blur-md border border-border-default rounded-xl shadow-lg animate-slide-up max-w-xs text-xs space-y-2.5 z-10">
          <div className="flex items-center justify-between">
            <h4 className="text-2xs font-black uppercase tracking-wider text-text-tertiary">
              {t('graph.legendTitle')}
            </h4>
            <button
              type="button"
              onClick={() => setShowLegend(false)}
              className="text-text-tertiary hover:text-text-primary p-0.5"
            >
              <X size={12} />
            </button>
          </div>

          <div className="grid grid-cols-2 gap-x-3 gap-y-1.5 text-2xs text-text-secondary">
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-full bg-blue-500 shrink-0" />
              <span>{t('graph.roleEntry')}</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-full bg-purple-500 shrink-0" />
              <span>{t('graph.roleCoordinator')}</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-full bg-emerald-500 shrink-0" />
              <span>{t('graph.roleLeaf')}</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-full bg-sky-600 shrink-0" />
              <span>{t('graph.roleIntermediate')}</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2.5 h-2.5 rounded-full bg-slate-500 shrink-0" />
              <span>{t('graph.roleIsolated')}</span>
            </div>
          </div>

          <div className="pt-2 border-t border-border-default/60 space-y-1.5 text-2xs text-text-secondary">
            <div className="flex items-center gap-2">
              <div className="w-6 h-0.5 bg-slate-400 dark:bg-slate-500" />
              <span>{t('graph.legendActiveCall')}</span>
            </div>
            <div className="flex items-center gap-2">
              <div className="w-6 border-b border-dashed border-slate-400 dark:border-slate-500" />
              <span>{t('graph.legendDisabledCall')}</span>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
