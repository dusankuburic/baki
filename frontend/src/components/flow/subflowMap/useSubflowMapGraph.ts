import {useCallback, useEffect, useMemo, useRef, useState} from 'react'
import type {Edge, EdgeMouseHandler, NodeMouseHandler} from '@xyflow/react'
import {analysisApi} from '@/api'
import {getErrorMessage} from '@/lib/getErrorMessage'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {arrowMarker, handleSides} from '@/components/graph/flow/edges'
import {useFlowColors} from '@/components/graph/flow/colors'
import type {GraphData} from '@/types'
import {enrichGraphData, type EnhancedGraphData, type EnhancedGraphEdge, type EnhancedGraphNode} from './model'
import {NODE_CARD_HEIGHT, NODE_CARD_WIDTH, layoutSubflowMap, mapEdgeId} from './layout'
import {SUBFLOW_CARD_TYPE, type SubflowCardNode} from './SubflowCard'

type NodeState = 'focused' | 'connected' | 'search-match' | 'dimmed' | undefined
type EdgeState = 'highlighted' | 'dimmed' | undefined

interface Highlight {
  nodes: Map<string, NodeState>
  edges: Map<string, EdgeState>
  matches: string[]
}

const NO_HIGHLIGHT: Highlight = {nodes: new Map(), edges: new Map(), matches: []}

/**
 * The whole graph, or — for the Local Map — one subflow with the subflows it calls and the ones
 * that call it.
 */
function scopeToSubflow(raw: GraphData | null | undefined, subflowId?: string): GraphData {
  const nodes = raw?.nodes ?? []
  const edges = raw?.edges ?? []
  if (!subflowId) return {nodes, edges}

  const connected = new Set<string>([subflowId])
  for (const edge of edges) {
    if (edge.source === subflowId || edge.target === subflowId) {
      connected.add(edge.source)
      connected.add(edge.target)
    }
  }
  return {
    nodes: nodes.filter(node => connected.has(node.id)),
    edges: edges.filter(edge => edge.source === subflowId || edge.target === subflowId),
  }
}

/**
 * Data, layout and highlighting for the subflow map (the Map and Local Map views).
 *
 * The hook owns no renderer: it returns React Flow nodes and edges, and every selection, search or
 * dimming state is a class name on them. Node `data` keeps its identity across those changes, so
 * highlighting re-renders the wrapper's class and never the card itself.
 */
export function useSubflowMapGraph(subflowId?: string) {
  const [data, setData] = useState<EnhancedGraphData | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [searchQuery, setSearchQuery] = useState('')
  const [focusedNodeId, setFocusedNodeId] = useState<string | null>(null)
  const [focusedEdgeId, setFocusedEdgeId] = useState<string | null>(null)
  const [dataVersion, setDataVersion] = useState(0)
  const [manualFits, setManualFits] = useState(0)
  const [focusToken, setFocusToken] = useState(0)
  const [focusIds, setFocusIds] = useState<string[]>([])

  const flowDoc = useFlowStore(s => s.document)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const subflowMapLayout = useUIStore(s => s.subflowMapLayout)
  const subflowMapDirection = useUIStore(s => s.subflowMapDirection)
  const subflowMapRouting = useUIStore(s => s.subflowMapRouting)
  const report = useAnalysisStore(s => (flowDoc ? s.reports.get(flowDoc.id) : undefined))
  const findings = report?.findings
  const colors = useFlowColors()

  const loadToken = useRef(0)

  const loadGraph = useCallback(async () => {
    if (!flowDoc) return
    const token = loadToken.current + 1
    loadToken.current = token
    setLoading(true)
    setError(null)
    try {
      const raw = await analysisApi.getExecutionGraph()
      if (token !== loadToken.current) return
      setData(enrichGraphData(scopeToSubflow(raw, subflowId), findings))
      setDataVersion(v => v + 1)
    } catch (err) {
      if (token === loadToken.current) setError(getErrorMessage(err))
    } finally {
      if (token === loadToken.current) setLoading(false)
    }
  }, [flowDoc, subflowId, findings])

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void loadGraph()
  }, [flowDoc?.id, report?.generatedAt, subflowId]) // eslint-disable-line react-hooks/exhaustive-deps

  const layout = useMemo(
    () => (data ? layoutSubflowMap(data, subflowMapLayout, subflowMapDirection, subflowMapRouting) : null),
    [data, subflowMapLayout, subflowMapDirection, subflowMapRouting],
  )

  const layered = subflowMapLayout === 'hierarchy'

  const baseNodes = useMemo<SubflowCardNode[]>(() => {
    if (!data || !layout) return []
    const sides = handleSides(layered ? subflowMapDirection : 'TB')
    return data.nodes.map(node => ({
      id: node.id,
      type: SUBFLOW_CARD_TYPE,
      position: layout.positions.get(node.id) ?? {x: 0, y: 0},
      width: NODE_CARD_WIDTH,
      height: NODE_CARD_HEIGHT,
      data: {subflow: node, anchor: node.id === subflowId},
      ...sides,
    }))
  }, [data, layout, layered, subflowMapDirection, subflowId])

  const baseEdges = useMemo(() => {
    if (!data || !layout) return []
    const type = !layered ? 'floating' : subflowMapRouting === 'corridor' ? 'polyline' : 'default'
    return data.edges.map(edge => {
      const id = mapEdgeId(edge.source, edge.target)
      const points = layout.points.get(id)
      return {
        id,
        source: edge.source,
        target: edge.target,
        type,
        data: points ? {points} : undefined,
        disabled: edge.isFullyDisabled,
        // Cytoscape mapped the call count onto the line width; keep the same range.
        width: 1.5 + (Math.min(6, Math.max(1, edge.weight)) - 1) * 0.6,
      }
    })
  }, [data, layout, layered, subflowMapRouting])

  const highlight = useMemo<Highlight>(() => {
    if (!data) return NO_HIGHLIGHT
    const query = searchQuery.trim().toLowerCase()

    if (query) {
      const matches = data.nodes.filter(n => (n.label ?? '').toLowerCase().includes(query)).map(n => n.id)
      const matched = new Set(matches)
      const nodes = new Map<string, NodeState>()
      for (const node of data.nodes) nodes.set(node.id, matched.has(node.id) ? 'search-match' : 'dimmed')
      const edges = new Map<string, EdgeState>()
      for (const edge of data.edges) edges.set(mapEdgeId(edge.source, edge.target), 'dimmed')
      return {nodes, edges, matches}
    }

    if (focusedNodeId) {
      const neighbours = new Set<string>()
      const touching = new Set<string>()
      for (const edge of data.edges) {
        if (edge.source === focusedNodeId || edge.target === focusedNodeId) {
          neighbours.add(edge.source === focusedNodeId ? edge.target : edge.source)
          touching.add(mapEdgeId(edge.source, edge.target))
        }
      }
      const nodes = new Map<string, NodeState>()
      for (const node of data.nodes) {
        nodes.set(
          node.id,
          node.id === focusedNodeId ? 'focused' : neighbours.has(node.id) ? 'connected' : 'dimmed',
        )
      }
      const edges = new Map<string, EdgeState>()
      for (const edge of data.edges) {
        const id = mapEdgeId(edge.source, edge.target)
        edges.set(id, touching.has(id) ? 'highlighted' : 'dimmed')
      }
      return {nodes, edges, matches: []}
    }

    if (focusedEdgeId) {
      const focused = data.edges.find(e => mapEdgeId(e.source, e.target) === focusedEdgeId)
      if (!focused) return NO_HIGHLIGHT
      const ends = new Set([focused.source, focused.target])
      const nodes = new Map<string, NodeState>()
      for (const node of data.nodes) nodes.set(node.id, ends.has(node.id) ? 'connected' : 'dimmed')
      const edges = new Map<string, EdgeState>()
      for (const edge of data.edges) {
        const id = mapEdgeId(edge.source, edge.target)
        edges.set(id, id === focusedEdgeId ? 'highlighted' : 'dimmed')
      }
      return {nodes, edges, matches: []}
    }

    return NO_HIGHLIGHT
  }, [data, searchQuery, focusedNodeId, focusedEdgeId])

  const nodes = useMemo<SubflowCardNode[]>(
    () =>
      baseNodes.map(node => {
        const state = highlight.nodes.get(node.id)
        const classes = ['flow-node']
        if (state) classes.push(`flow-node--${state}`)
        if (node.data.anchor) classes.push('flow-node--anchor')
        return {...node, className: classes.join(' ')}
      }),
    [baseNodes, highlight],
  )

  const edges = useMemo<Edge[]>(
    () =>
      baseEdges.map(({disabled, width, ...edge}) => {
        const state = highlight.edges.get(edge.id)
        const stroke = state === 'highlighted' ? colors.brand : disabled ? colors.edgeMuted : colors.edge
        return {
          ...edge,
          className: state === 'dimmed' ? 'flow-edge--dimmed' : undefined,
          style: {
            stroke,
            strokeWidth: state === 'highlighted' ? width + 1 : width,
            strokeDasharray: disabled ? '6 4' : undefined,
            opacity: disabled && state !== 'highlighted' ? 0.65 : 1,
          },
          markerEnd: arrowMarker(stroke),
        }
      }),
    [baseEdges, highlight, colors],
  )

  const clearFocus = useCallback(() => {
    setFocusedNodeId(null)
    setFocusedEdgeId(null)
  }, [])

  const onNodeClick = useCallback<NodeMouseHandler>((_, node) => {
    setFocusedNodeId(node.id)
    setFocusedEdgeId(null)
  }, [])

  const onNodeDoubleClick = useCallback<NodeMouseHandler>(
    (_, node) => selectSubflow(node.id),
    [selectSubflow],
  )

  const onEdgeClick = useCallback<EdgeMouseHandler>((_, edge) => {
    setFocusedEdgeId(edge.id)
    setFocusedNodeId(null)
  }, [])

  const fitToScreen = useCallback(() => setManualFits(n => n + 1), [])

  const handleSearch = useCallback(
    (event: React.FormEvent) => {
      event.preventDefault()
      if (highlight.matches.length === 0) return
      setFocusIds(highlight.matches)
      setFocusToken(token => token + 1)
    },
    [highlight.matches],
  )

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') clearFocus()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [clearFocus])

  const focusedNode = useMemo<EnhancedGraphNode | null>(
    () => (focusedNodeId ? data?.nodes.find(n => n.id === focusedNodeId) ?? null : null),
    [data, focusedNodeId],
  )

  const focusedEdge = useMemo<EnhancedGraphEdge | null>(
    () => (focusedEdgeId ? data?.edges.find(e => mapEdgeId(e.source, e.target) === focusedEdgeId) ?? null : null),
    [data, focusedEdgeId],
  )

  return {
    nodes,
    edges,
    loading,
    error,
    retry: loadGraph,
    searchQuery,
    setSearchQuery,
    matchCount: searchQuery.trim() ? highlight.matches.length : null,
    handleSearch,
    focusedNode,
    focusedEdge,
    clearFocus,
    fitToScreen,
    onNodeClick,
    onNodeDoubleClick,
    onEdgeClick,
    focusIds,
    focusToken,
    /** Changes when the graph should be refitted: new data, another layout, the fit button. */
    fitSignal: `${dataVersion}:${subflowMapLayout}:${subflowMapDirection}:${subflowMapRouting}:${manualFits}`,
  }
}
