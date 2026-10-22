import {memo, useCallback, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {Position, type Node, type NodeMouseHandler, type NodeProps} from '@xyflow/react'
import {Info} from 'lucide-react'
import FlowCanvas from '@/components/graph/flow/FlowCanvas'
import FlowHandles from '@/components/graph/flow/FlowHandles'
import {arrowMarker, flowEdgeTypes} from '@/components/graph/flow/edges'
import {layoutDagre} from '@/components/graph/flow/layout'
import {useFlowColors} from '@/components/graph/flow/colors'
import type {VariableEvent} from '@/types'
import {useFlowStore} from '@/stores/flowStore'

const EVENT_COLOR: Record<VariableEvent['type'], string> = {
  init: '#22c55e',
  mutate: '#f59e0b',
  read: '#3b82f6',
}

const DOT_SIZE = 22
const LINEAGE_NODE_TYPE = 'lineageDot'

type LineageDotData = {color: string; label: string; blockId: string}
type LineageDotNode = Node<LineageDotData, typeof LINEAGE_NODE_TYPE>

const LineageDot = memo(function LineageDot({data}: NodeProps<LineageDotNode>) {
  return (
    <div className="flow-node relative" style={{width: DOT_SIZE, height: DOT_SIZE}}>
      <FlowHandles source={Position.Right} target={Position.Left} />
      <div
        className="flow-card h-full w-full rounded-full border-2 border-white/15"
        style={{backgroundColor: data.color}}
      />
      <span className="absolute top-full left-1/2 -translate-x-1/2 pt-0.5 text-[9px] text-text-tertiary">
        {data.label}
      </span>
    </div>
  )
})

const nodeTypes = {[LINEAGE_NODE_TYPE]: LineageDot}

function dominantType(types: Set<VariableEvent['type']>): VariableEvent['type'] {
  if (types.has('init')) return 'init'
  if (types.has('mutate')) return 'mutate'
  return 'read'
}

type Props = {
  events: VariableEvent[]
}

export default function VariableLineageGraph({events}: Props) {
  const {t} = useTranslation('inspector')
  const navigateToBlock = useFlowStore(s => s.navigateToBlock)
  const colors = useFlowColors()

  const graph = useMemo(() => {
    if (events.length === 0) return {nodes: [], edges: []}
    const byBlock = new Map<string, {types: Set<VariableEvent['type']>; line: number}>()
    for (const event of events) {
      const entry = byBlock.get(event.blockId)
      if (entry) entry.types.add(event.type)
      else byBlock.set(event.blockId, {types: new Set([event.type]), line: event.line})
    }

    const nodes = [...byBlock.entries()].map(([blockId, info]) => ({
      id: blockId,
      width: DOT_SIZE,
      height: DOT_SIZE,
      data: {color: EVENT_COLOR[dominantType(info.types)], label: `L${info.line}`, blockId},
    }))

    const edges: Array<{id: string; source: string; target: string}> = []
    const seen = new Set<string>()
    for (let i = 1; i < events.length; i++) {
      const source = events[i - 1]?.blockId
      const target = events[i]?.blockId
      if (source === undefined || target === undefined || source === target) continue
      const key = `${source}->${target}`
      if (seen.has(key)) continue
      seen.add(key)
      edges.push({id: key, source, target})
    }

    return {nodes, edges}
  }, [events])

  const layout = useMemo(() => layoutDagre(graph.nodes, graph.edges, {rankdir: 'LR', nodesep: 30, ranksep: 50}), [graph])

  const nodes = useMemo<LineageDotNode[]>(
    () =>
      graph.nodes.map(node => ({
        id: node.id,
        type: LINEAGE_NODE_TYPE,
        position: layout.positions.get(node.id) ?? {x: 0, y: 0},
        width: DOT_SIZE,
        height: DOT_SIZE,
        data: node.data,
      })),
    [graph, layout],
  )

  const edges = useMemo(
    () =>
      graph.edges.map(edge => ({
        ...edge,
        type: 'floating',
        style: {stroke: colors.edgeMuted, strokeWidth: 1.5},
        markerEnd: arrowMarker(colors.edgeMuted, 0.8),
      })),
    [graph, colors],
  )

  const onNodeClick = useCallback<NodeMouseHandler>(
    (_, node) => {
      const blockId = (node as LineageDotNode).data.blockId
      if (blockId) navigateToBlock(blockId)
    },
    [navigateToBlock],
  )

  if (events.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-6 opacity-40">
        <Info size={20} className="mb-1.5" />
        <p className="text-[11px]">{t('lineage.noMatchingEvents')}</p>
      </div>
    )
  }

  return (
    <FlowCanvas
      className="h-44 w-full rounded-lg border border-border-subtle bg-surface-1"
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      edgeTypes={flowEdgeTypes}
      minZoom={0.3}
      maxZoom={3}
      fitPadding="30px"
      fitSignal={nodes.map(node => node.id).join('|')}
      onNodeClick={onNodeClick}
    />
  )
}
