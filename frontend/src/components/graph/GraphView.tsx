import {useCallback, useMemo} from 'react'
import {MiniMap, type Edge, type Node, type NodeMouseHandler} from '@xyflow/react'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {usePresenceStore} from '@/stores/presenceStore'
import {
  BLOCK_NODE_HEIGHT,
  BLOCK_NODE_TYPE,
  BLOCK_NODE_WIDTH,
  blocksToFlow,
  countSubflowNodes,
  type BlockNodeData,
} from './graphElements'
import BlockNode, {type BlockFlowNode} from './BlockNode'
import FlowCanvas from './flow/FlowCanvas'
import {layoutDagre} from './flow/layout'
import {arrowMarker} from './flow/edges'
import {blockColor, useFlowColors} from './flow/colors'
import {EmptyState} from '@/components/shared'

const GRAPH_NODE_LIMIT = 800

const nodeTypes = {[BLOCK_NODE_TYPE]: BlockNode}

export default function GraphView({subflowId: subflowIdProp}: {subflowId?: string} = {}) {
  const flowDoc = useFlowStore(s => s.document)
  const selectedSubflowId = useFlowStore(s => s.selectedSubflowId)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const selectedVariable = useUIStore(s => s.selectedVariable)
  const presenceUsers = usePresenceStore(s => s.users)
  const colors = useFlowColors()

  const subflowId = subflowIdProp ?? selectedSubflowId
  const subflow = flowDoc?.subflows.find(s => s.id === subflowId) ?? flowDoc?.subflows[0]
  const report = useAnalysisStore(s => (flowDoc ? s.reports.get(flowDoc.id) : undefined))

  const graph = useMemo(() => (subflow ? blocksToFlow(subflow) : {nodes: [], edges: []}), [subflow])

  const layout = useMemo(
    () =>
      layoutDagre(
        graph.nodes.map(node => ({id: node.id, width: BLOCK_NODE_WIDTH, height: BLOCK_NODE_HEIGHT})),
        graph.edges,
        {rankdir: 'TB', nodesep: 40, ranksep: 60},
      ),
    [graph],
  )

  /** Findings per block, and the worst severity on each — the Cytoscape `finding-*` classes. */
  const findings = useMemo(() => {
    const counts = new Map<string, number>()
    const severities = new Map<string, 'error' | 'warning' | 'info'>()
    for (const finding of report?.findings ?? []) {
      counts.set(finding.blockId, (counts.get(finding.blockId) ?? 0) + 1)
      const current = severities.get(finding.blockId)
      const next = finding.severity === 'error' ? 'error' : finding.severity === 'warning' ? 'warning' : 'info'
      if (!current || next === 'error' || (next === 'warning' && current === 'info')) {
        severities.set(finding.blockId, next)
      }
    }
    return {counts, severities}
  }, [report])

  const baseNodes = useMemo<BlockFlowNode[]>(
    () =>
      graph.nodes.map(node => ({
        id: node.id,
        type: BLOCK_NODE_TYPE,
        position: layout.positions.get(node.id) ?? {x: 0, y: 0},
        width: BLOCK_NODE_WIDTH,
        height: BLOCK_NODE_HEIGHT,
        data: {...node.data, findingCount: findings.counts.get(node.id)},
      })),
    [graph, layout, findings],
  )

  const remoteSelected = useMemo(() => {
    const ids = new Set<string>()
    for (const user of Object.values(presenceUsers)) {
      if (user.selectedBlockId) ids.add(user.selectedBlockId)
    }
    return ids
  }, [presenceUsers])

  const nodes = useMemo<BlockFlowNode[]>(
    () =>
      baseNodes.map(node => {
        const classes = ['flow-node']
        const severity = findings.severities.get(node.id)
        if (severity) classes.push(`flow-node--finding-${severity}`)
        if (selectedVariable) {
          classes.push(
            node.data.variables.includes(selectedVariable)
              ? 'flow-node--variable-highlighted'
              : 'flow-node--dimmed',
          )
        }
        if (remoteSelected.has(node.id)) classes.push('flow-node--remote-selected')
        if (node.id === selectedBlockId) classes.push('flow-node--selected')
        return {...node, className: classes.join(' ')}
      }),
    [baseNodes, findings, selectedVariable, remoteSelected, selectedBlockId],
  )

  /** With a variable selected, an edge stays lit only while one of its ends is. */
  const edges = useMemo<Edge[]>(() => {
    const lit = selectedVariable
      ? new Set(
          graph.nodes.filter(node => node.data.variables.includes(selectedVariable)).map(node => node.id),
        )
      : null
    return graph.edges.map(edge => ({
      ...edge,
      className: lit && !lit.has(edge.source) && !lit.has(edge.target) ? 'flow-edge--dimmed' : undefined,
      style: {stroke: colors.edge, strokeWidth: 2},
      markerEnd: arrowMarker(colors.edge),
    }))
  }, [graph, selectedVariable, colors])

  const onNodeClick = useCallback<NodeMouseHandler>((_, node) => selectBlock(node.id), [selectBlock])

  const onNodeDoubleClick = useCallback<NodeMouseHandler>(
    (_, node) => {
      const data = (node as BlockFlowNode).data
      if (data.type === 'SUBFLOW' || data.hasChildren) {
        if (data.subflowId) selectSubflow(data.subflowId)
      }
    },
    [selectSubflow],
  )

  const exportFileName = useCallback(() => {
    const safe = (flowDoc?.name ?? 'flow').replace(/[^a-z0-9-_]+/gi, '_').slice(0, 40)
    return `${safe}-graph.png`
  }, [flowDoc?.name])

  const minimapNodeColor = useCallback(
    (node: Node) => blockColor(colors, (node.data as BlockNodeData | undefined)?.type),
    [colors],
  )

  if (!flowDoc) {
    return (
      <div className="flex-1 flex items-center justify-center bg-surface-0">
        <EmptyState title="No flow loaded" description="Open a flow file to see the graph view" />
      </div>
    )
  }

  const totalNodes = subflow ? countSubflowNodes(subflow.blocks) : 0
  if (totalNodes > GRAPH_NODE_LIMIT) {
    return (
      <div className="flex-1 flex items-center justify-center bg-surface-0 p-8 text-center">
        <div>
          <p className="text-lg font-medium text-text-primary mb-2">Large flow detected</p>
          <p className="text-sm text-text-secondary">
            This subflow has {totalNodes} blocks (limit: {GRAPH_NODE_LIMIT}). Use the block view for navigation and
            analysis.
          </p>
        </div>
      </div>
    )
  }

  if (subflow && subflow.blocks.length === 0) {
    return (
      <div className="flex-1 flex items-center justify-center bg-surface-0">
        <EmptyState title="Empty subflow" description="This subflow has no blocks to display" />
      </div>
    )
  }

  return (
    <div className="flex-1 relative bg-surface-0">
      <FlowCanvas
        className="absolute inset-0"
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        minZoom={0.2}
        maxZoom={3}
        fitPadding="40px"
        fitSignal={`${flowDoc.id}:${subflow?.id ?? ''}`}
        syncZoom
        exportFileName={exportFileName}
        onNodeClick={onNodeClick}
        onNodeDoubleClick={onNodeDoubleClick}
      >
        <MiniMap
          pannable
          zoomable
          nodeColor={minimapNodeColor}
          nodeStrokeWidth={2}
          style={{width: 160, height: 100}}
        />
      </FlowCanvas>
    </div>
  )
}
