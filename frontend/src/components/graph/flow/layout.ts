import dagre from 'dagre'
import type {XYPosition} from '@xyflow/react'

/**
 * Graph layout for the React Flow views: everything here is pure geometry over ids and sizes, so it
 * runs in a test without a DOM and without a renderer.
 *
 * React Flow places a node by its top-left corner, dagre by its centre, so every layout returns
 * corners. `points` carries dagre's own routing polyline for the edges that draw it
 * (see `PolylineEdge`); layouts that route straight leave it empty.
 */
export interface LayoutNode {
  id: string
  width: number
  height: number
}

export interface LayoutEdge {
  id: string
  source: string
  target: string
}

export interface GraphLayout {
  positions: Map<string, XYPosition>
  points: Map<string, XYPosition[]>
}

export interface DagreOptions {
  rankdir?: 'LR' | 'TB'
  nodesep?: number
  ranksep?: number
  margin?: number
}

const EMPTY_LAYOUT = (): GraphLayout => ({positions: new Map(), points: new Map()})

/** Layered layout. `points` holds dagre's bend points, which the corridor routing draws. */
export function layoutDagre(
  nodes: LayoutNode[],
  edges: LayoutEdge[],
  {rankdir = 'TB', nodesep = 60, ranksep = 100, margin = 40}: DagreOptions = {},
): GraphLayout {
  if (nodes.length === 0) return EMPTY_LAYOUT()

  const g = new dagre.graphlib.Graph()
  g.setGraph({rankdir, nodesep, ranksep, marginx: margin, marginy: margin})
  g.setDefaultEdgeLabel(() => ({}))

  for (const node of nodes) g.setNode(node.id, {width: node.width, height: node.height})
  const ids = new Set(nodes.map(n => n.id))
  const routed = edges.filter(e => ids.has(e.source) && ids.has(e.target))
  for (const edge of routed) g.setEdge(edge.source, edge.target)

  dagre.layout(g)

  const positions = new Map<string, XYPosition>()
  for (const node of nodes) {
    const laid = g.node(node.id)
    positions.set(
      node.id,
      laid ? {x: laid.x - node.width / 2, y: laid.y - node.height / 2} : {x: 0, y: 0},
    )
  }

  const points = new Map<string, XYPosition[]>()
  for (const edge of routed) {
    const laid = g.edge(edge.source, edge.target)
    if (laid && Array.isArray(laid.points) && laid.points.length > 2) {
      points.set(
        edge.id,
        laid.points.map(p => ({x: p.x, y: p.y})),
      )
    }
  }

  return {positions, points}
}

export interface RadialNode extends LayoutNode {
  /** Distance from an entry node, as computed by `enrichGraphData`. */
  depth: number
}

/**
 * Concentric rings by depth — the deterministic stand-in for Cytoscape's `concentric` layout. Each
 * ring is wide enough that its nodes do not touch, and at least one node height clear of the ring
 * inside it.
 */
export function layoutRadial(nodes: RadialNode[], {gap = 60}: {gap?: number} = {}): GraphLayout {
  if (nodes.length === 0) return EMPTY_LAYOUT()

  const rings = new Map<number, RadialNode[]>()
  for (const node of nodes) {
    const ring = rings.get(node.depth)
    if (ring) ring.push(node)
    else rings.set(node.depth, [node])
  }

  const positions = new Map<string, XYPosition>()
  let radius = 0
  let previousDepth = 0

  for (const depth of [...rings.keys()].sort((a, b) => a - b)) {
    const ring = rings.get(depth)!
    const widest = Math.max(...ring.map(n => n.width))
    const tallest = Math.max(...ring.map(n => n.height))

    if (ring.length === 1 && radius === 0) {
      const only = ring[0]!
      positions.set(only.id, {x: -only.width / 2, y: -only.height / 2})
      radius = Math.max(tallest, widest) / 2 + gap
      previousDepth = depth
      continue
    }

    // Wide enough that the ring's nodes sit side by side without touching, and clear of the
    // previous ring. Gaps in the depth numbering widen the step rather than crowd the ring.
    const spread = (ring.length * (widest + gap)) / (2 * Math.PI)
    radius = Math.max(spread, radius + tallest / 2 + gap * Math.max(1, depth - previousDepth))
    previousDepth = depth

    ring.forEach((node, index) => {
      const angle = (index / ring.length) * 2 * Math.PI - Math.PI / 2
      positions.set(node.id, {
        x: Math.cos(angle) * radius - node.width / 2,
        y: Math.sin(angle) * radius - node.height / 2,
      })
    })

    radius += tallest / 2
  }

  return {positions, points: new Map()}
}

/**
 * Connected components, each laid out with dagre and then packed into shelves — the deterministic
 * stand-in for Cytoscape's force-directed `cose` layout. Nothing settles over time, so the first
 * paint is the final one.
 */
export function layoutClusters(
  nodes: LayoutNode[],
  edges: LayoutEdge[],
  {gap = 80, ...dagreOptions}: DagreOptions & {gap?: number} = {},
): GraphLayout {
  if (nodes.length === 0) return EMPTY_LAYOUT()

  const components = connectedComponents(nodes, edges)
  const positions = new Map<string, XYPosition>()
  const points = new Map<string, XYPosition[]>()

  const laidOut = components.map(component => {
    const ids = new Set(component.map(n => n.id))
    const layout = layoutDagre(
      component,
      edges.filter(e => ids.has(e.source) && ids.has(e.target)),
      {...dagreOptions, margin: 0},
    )
    let width = 0
    let height = 0
    for (const node of component) {
      const at = layout.positions.get(node.id) ?? {x: 0, y: 0}
      width = Math.max(width, at.x + node.width)
      height = Math.max(height, at.y + node.height)
    }
    return {component, layout, width, height}
  })

  // Shelves roughly as wide as the whole graph is tall, so a flow of many small components reads
  // as a block rather than as one long row.
  const shelfWidth = Math.max(...laidOut.map(c => c.width), Math.sqrt(laidOut.reduce((sum, c) => sum + c.width * c.height, 0)) * 1.6)
  let shelfX = 0
  let shelfY = 0
  let shelfHeight = 0

  for (const {component, layout, width, height} of laidOut) {
    if (shelfX > 0 && shelfX + width > shelfWidth) {
      shelfX = 0
      shelfY += shelfHeight + gap
      shelfHeight = 0
    }
    for (const node of component) {
      const at = layout.positions.get(node.id) ?? {x: 0, y: 0}
      positions.set(node.id, {x: at.x + shelfX, y: at.y + shelfY})
    }
    shelfX += width + gap
    shelfHeight = Math.max(shelfHeight, height)
  }

  return {positions, points}
}

/**
 * Components in a stable order — largest first, ties broken by the first node's id — so the same
 * graph always packs the same way.
 */
function connectedComponents(nodes: LayoutNode[], edges: LayoutEdge[]): LayoutNode[][] {
  const parent = new Map<string, string>()
  for (const node of nodes) parent.set(node.id, node.id)

  const find = (id: string): string => {
    let root = id
    while (parent.get(root) !== root) root = parent.get(root)!
    let walk = id
    while (parent.get(walk) !== root) {
      const next = parent.get(walk)!
      parent.set(walk, root)
      walk = next
    }
    return root
  }

  for (const edge of edges) {
    if (!parent.has(edge.source) || !parent.has(edge.target)) continue
    const a = find(edge.source)
    const b = find(edge.target)
    if (a !== b) parent.set(a, b)
  }

  const groups = new Map<string, LayoutNode[]>()
  for (const node of nodes) {
    const root = find(node.id)
    const group = groups.get(root)
    if (group) group.push(node)
    else groups.set(root, [node])
  }

  return [...groups.values()].sort(
    (a, b) => b.length - a.length || (a[0]!.id < b[0]!.id ? -1 : 1),
  )
}
