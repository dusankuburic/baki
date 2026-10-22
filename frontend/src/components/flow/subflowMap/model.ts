import type {GraphData, GraphEdge, GraphNode, Finding} from '@/types'

export type SubflowRole = 'entry' | 'leaf' | 'coordinator' | 'isolated' | 'intermediate'

export interface EnhancedGraphNode extends GraphNode {
  role: SubflowRole
  depth: number
  inDegree: number
  outDegree: number
}

export interface EnhancedGraphEdge extends GraphEdge {
  callCount: number
  disabledCount: number
  enabledCount: number
  isFullyDisabled: boolean
  weight: number
}

export interface EnhancedGraphData {
  nodes: EnhancedGraphNode[]
  edges: EnhancedGraphEdge[]
}

export function determineSubflowRole(
  inDegree: number,
  outDegree: number,
  isMainCandidate: boolean,
): SubflowRole {
  if (inDegree === 0 && outDegree === 0) return 'isolated'
  if (isMainCandidate || inDegree === 0) return 'entry'
  if (outDegree === 0) return 'leaf'
  if (inDegree >= 2 && outDegree >= 2) return 'coordinator'
  return 'intermediate'
}

export function computeGraphDepths(
  nodes: GraphNode[],
  edges: GraphEdge[],
  inDegrees: Map<string, number>,
): Map<string, number> {
  const depths = new Map<string, number>()
  const adjacency = new Map<string, string[]>()

  for (const n of nodes) {
    adjacency.set(n.id, [])
  }
  for (const e of edges) {
    const list = adjacency.get(e.source)
    if (list) list.push(e.target)
  }

  // Seed BFS with entry nodes (inDegree === 0 or labeled 'Main'/'main')
  const queue: Array<{id: string; depth: number}> = []
  const entryNodes = nodes.filter(
    n => (inDegrees.get(n.id) ?? 0) === 0 || n.label.toLowerCase() === 'main',
  )

  const visited = new Set<string>()

  for (const entry of entryNodes) {
    if (!visited.has(entry.id)) {
      queue.push({id: entry.id, depth: 0})
      visited.add(entry.id)
      depths.set(entry.id, 0)
    }
  }

  while (queue.length > 0) {
    const {id, depth} = queue.shift()!
    const neighbors = adjacency.get(id) || []
    for (const nxt of neighbors) {
      const nextDepth = depth + 1
      const currentKnown = depths.get(nxt)
      if (currentKnown === undefined || nextDepth < currentKnown) {
        depths.set(nxt, nextDepth)
      }
      if (!visited.has(nxt)) {
        visited.add(nxt)
        queue.push({id: nxt, depth: nextDepth})
      }
    }
  }

  // Any unreached nodes get depth 0
  for (const n of nodes) {
    if (!depths.has(n.id)) {
      depths.set(n.id, 0)
    }
  }

  return depths
}

export function enrichGraphData(
  graphData: GraphData,
  findings?: readonly Finding[] | null,
): EnhancedGraphData {
  const inDegrees = new Map<string, number>()
  const outDegrees = new Map<string, number>()

  for (const n of graphData.nodes) {
    inDegrees.set(n.id, 0)
    outDegrees.set(n.id, 0)
  }

  for (const e of graphData.edges) {
    outDegrees.set(e.source, (outDegrees.get(e.source) ?? 0) + 1)
    inDegrees.set(e.target, (inDegrees.get(e.target) ?? 0) + 1)
  }

  // Client-side finding counts if findings provided
  const errCounts = new Map<string, number>()
  const warnCounts = new Map<string, number>()

  if (findings && findings.length > 0) {
    for (const f of findings) {
      if (!f.subflowId) continue
      if (f.severity === 'error') {
        errCounts.set(f.subflowId, (errCounts.get(f.subflowId) ?? 0) + 1)
      } else if (f.severity === 'warning') {
        warnCounts.set(f.subflowId, (warnCounts.get(f.subflowId) ?? 0) + 1)
      }
    }
  }

  const depths = computeGraphDepths(graphData.nodes, graphData.edges, inDegrees)

  const enhancedNodes: EnhancedGraphNode[] = graphData.nodes.map(node => {
    const inDeg = inDegrees.get(node.id) ?? 0
    const outDeg = outDegrees.get(node.id) ?? 0
    const isMain = node.label.toLowerCase() === 'main'
    const role = determineSubflowRole(inDeg, outDeg, isMain)
    const depth = depths.get(node.id) ?? 0

    const errorCount = findings ? (errCounts.get(node.id) ?? 0) : node.errorCount
    const warnCount = findings ? (warnCounts.get(node.id) ?? 0) : node.warnCount

    return {
      ...node,
      role,
      depth,
      inDegree: inDeg,
      outDegree: outDeg,
      errorCount,
      warnCount,
    }
  })

  const enhancedEdges: EnhancedGraphEdge[] = graphData.edges.map(edge => {
    const callCount = edge.callCount !== undefined && edge.callCount > 0 ? edge.callCount : 1
    const disabledCount = edge.disabledCount ?? 0
    const enabledCount = Math.max(0, callCount - disabledCount)
    const isFullyDisabled = enabledCount === 0 && disabledCount > 0
    const weight = Math.min(6, Math.max(1, enabledCount > 0 ? enabledCount : 1))

    return {
      ...edge,
      callCount,
      disabledCount,
      enabledCount,
      isFullyDisabled,
      weight,
    }
  })

  return {
    nodes: enhancedNodes,
    edges: enhancedEdges,
  }
}
