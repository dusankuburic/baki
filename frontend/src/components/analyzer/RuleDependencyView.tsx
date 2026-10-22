import {getErrorMessage} from '@/lib/getErrorMessage'
import {memo, useEffect, useState, useCallback, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {Position, type Edge, type Node, type NodeMouseHandler, type NodeProps} from '@xyflow/react'
import {Loader2, AlertTriangle, RefreshCw} from 'lucide-react'
import {analysisApi} from '@/api'
import FlowCanvas from '@/components/graph/flow/FlowCanvas'
import FlowHandles from '@/components/graph/flow/FlowHandles'
import {arrowMarker} from '@/components/graph/flow/edges'
import {layoutDagre} from '@/components/graph/flow/layout'
import {useFlowColors} from '@/components/graph/flow/colors'
import {useUIStore} from '@/stores/uiStore'
import {logger} from '@/lib/logger'
import type {DependencyAnalysis} from '@/types'

const RULE_NODE_WIDTH = 180
const RULE_NODE_HEIGHT = 44
const RULE_NODE_TYPE = 'rule'

type RuleNodeData = {label: string}
type RuleFlowNode = Node<RuleNodeData, typeof RULE_NODE_TYPE>

const RuleNode = memo(function RuleNode({data}: NodeProps<RuleFlowNode>) {
  return (
    <div className="flow-node" style={{width: RULE_NODE_WIDTH, height: RULE_NODE_HEIGHT}}>
      <FlowHandles source={Position.Bottom} target={Position.Top} />
      <div className="flow-card flex h-full w-full items-center justify-center rounded-lg border border-border-strong bg-surface-2 px-1.5">
        <span className="truncate font-mono text-[11px] text-text-primary">{data.label}</span>
      </div>
    </div>
  )
})

const nodeTypes = {[RULE_NODE_TYPE]: RuleNode}

type RelatedRulesResponse = DependencyAnalysis & {editorial?: boolean; description?: string}

export default function RuleDependencyView() {
  const {t} = useTranslation('analyzer')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [data, setData] = useState<RelatedRulesResponse | null>(null)
  const [selected, setSelected] = useState<string | null>(null)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const colors = useFlowColors()

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      setData(await analysisApi.getDependencies())
    } catch (err) {
      setError(getErrorMessage(err))
      logger.warn('related rules view: load failed', err)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void load()
  }, [load])

  const graph = useMemo(() => {
    const ids = new Set<string>(data?.topoOrder ?? [])
    for (const dep of data?.dependencies ?? []) {
      ids.add(dep.fromRuleId)
      ids.add(dep.toRuleId)
    }
    const nodes = [...ids].map(id => ({id, width: RULE_NODE_WIDTH, height: RULE_NODE_HEIGHT}))
    const edges = (data?.dependencies ?? []).map((dep, i) => ({
      id: `e${i}`,
      source: dep.fromRuleId,
      target: dep.toRuleId,
      reason: shortReason(dep.reason),
    }))
    return {nodes, edges, inCycle: new Set<string>((data?.cycles ?? []).flat())}
  }, [data])

  const layout = useMemo(
    () => layoutDagre(graph.nodes, graph.edges, {rankdir: 'TB', nodesep: 50, ranksep: 70}),
    [graph],
  )

  const nodes = useMemo<RuleFlowNode[]>(
    () =>
      graph.nodes.map(node => ({
        id: node.id,
        type: RULE_NODE_TYPE,
        position: layout.positions.get(node.id) ?? {x: 0, y: 0},
        width: RULE_NODE_WIDTH,
        height: RULE_NODE_HEIGHT,
        data: {label: node.id},
        className: [
          'flow-node',
          graph.inCycle.has(node.id) ? 'flow-node--cycle' : '',
          node.id === selected ? 'flow-node--selected' : '',
        ]
          .filter(Boolean)
          .join(' '),
      })),
    [graph, layout, selected],
  )

  const edges = useMemo<Edge[]>(
    () =>
      graph.edges.map(edge => ({
        id: edge.id,
        source: edge.source,
        target: edge.target,
        label: edge.reason,
        labelStyle: {fill: colors.edge, fontSize: 9},
        labelBgStyle: {fill: colors.surface1, fillOpacity: 0.9},
        labelBgPadding: [2, 1] as [number, number],
        style: {stroke: colors.edge, strokeWidth: 1.5},
        markerEnd: arrowMarker(colors.edge, 0.9),
      })),
    [graph, colors],
  )

  const onNodeClick = useCallback<NodeMouseHandler>((_, node) => setSelected(node.id), [])
  const onPaneClick = useCallback(() => setSelected(null), [])

  const selectedDeps =
    data && selected ? (data.dependencies ?? []).filter(d => d.fromRuleId === selected || d.toRuleId === selected) : []
  const hasCycles = !!(data?.cycles && data.cycles.length > 0)

  return (
    <div className="relative w-full h-full bg-surface-1">
      <FlowCanvas
        className="absolute inset-0"
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        minZoom={0.3}
        maxZoom={2.5}
        fitPadding="40px"
        fitSignal={`${nodes.length}:${edges.length}`}
        onNodeClick={onNodeClick}
        onPaneClick={onPaneClick}
      />

      <div className="absolute top-3 left-3 right-3 flex items-center justify-between pointer-events-none">
        <div className="flex items-center gap-2 pointer-events-auto">
          <button
            onClick={() => setMainPaneView('dashboard')}
            className="text-xs px-2 py-1 rounded-lg bg-surface-2/80 border border-border-subtle text-text-secondary hover:text-text-primary backdrop-blur-sm transition-colors"
          >
            {t('dependencies.back')}
          </button>
          <h2 className="text-sm font-semibold text-text-primary">{t('dependencies.title')}</h2>
          {data && (
            <span className="text-2xs text-text-tertiary font-mono">
              {t('dependencies.summary', {
                links: (data.dependencies ?? []).length,
                rules: (data.topoOrder ?? []).length,
              })}
            </span>
          )}
        </div>
        <button
          onClick={load}
          title={t('dependencies.refresh')}
          className="pointer-events-auto p-1.5 rounded-lg bg-surface-2/80 border border-border-subtle text-text-tertiary hover:text-text-primary backdrop-blur-sm transition-colors"
        >
          <RefreshCw size={13} />
        </button>
      </div>

      <p className="absolute top-11 left-3 right-3 text-2xs text-text-tertiary pointer-events-none">
        {data?.description || t('dependencies.editorialNote')}
      </p>

      {loading && (
        <div className="absolute inset-0 flex items-center justify-center bg-surface-1/60 backdrop-blur-xs">
          <Loader2 size={20} className="animate-spin text-brand-400" />
        </div>
      )}

      {error && (
        <div className="absolute top-16 left-1/2 -translate-x-1/2 flex items-center gap-2 px-3 py-2 rounded-lg bg-red-500/10 border border-red-500/30 text-xs text-red-400">
          <AlertTriangle size={13} /> {error}
        </div>
      )}

      {hasCycles && !loading && (
        <div className="absolute top-16 left-3 right-3 flex items-start gap-2 px-3 py-2 rounded-lg bg-red-500/10 border border-red-500/30 text-xs text-red-400 pointer-events-none">
          <AlertTriangle size={13} className="mt-0.5 shrink-0" />
          <div>
            <span className="font-semibold">{t('dependencies.cycleWarning')}</span>{' '}
            {(data?.cycles ?? []).map((c, i) => (
              <span key={i} className="font-mono">
                {c.join(' → ')}
                {i < (data?.cycles?.length ?? 0) - 1 ? '; ' : ''}
              </span>
            ))}
          </div>
        </div>
      )}

      {selected && data && (
        <div className="absolute bottom-3 left-3 w-72 max-w-[80%] p-3 rounded-lg bg-surface-2/90 border border-border-subtle backdrop-blur-sm text-xs">
          <div className="flex items-center justify-between mb-2">
            <span className="font-mono font-semibold text-text-primary">{selected}</span>
            <button onClick={() => setSelected(null)} className="text-text-tertiary hover:text-text-primary">
              ×
            </button>
          </div>
          {selectedDeps.length === 0 ? (
            <p className="text-text-tertiary">{t('dependencies.noRelated')}</p>
          ) : (
            <ul className="space-y-1">
              {selectedDeps.map((d, i) => (
                <li key={i} className="text-text-secondary">
                  <span className="font-mono text-brand-400">{d.fromRuleId}</span>
                  <span className="text-text-tertiary"> → </span>
                  <span className="font-mono text-brand-400">{d.toRuleId}</span>
                  <div className="text-2xs text-text-tertiary">{d.reason}</div>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      <div className="absolute bottom-3 right-3 flex flex-col gap-1 text-2xs text-text-tertiary bg-surface-2/80 border border-border-subtle rounded-lg px-2.5 py-2 backdrop-blur-sm pointer-events-none">
        <div className="flex items-center gap-1.5">
          <span className="w-3 h-3 rounded-sm border-2 border-red-500" /> {t('dependencies.legendInCycle')}
        </div>
        <div className="flex items-center gap-1.5">
          <span className="w-3 h-3 rounded-sm border border-border-strong" /> {t('dependencies.legendRule')}
        </div>
      </div>
    </div>
  )
}

function shortReason(r: string): string {
  if (!r) return ''
  return r.length > 24 ? r.slice(0, 23) + '…' : r
}
