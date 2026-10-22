import {memo} from 'react'
import {
  BaseEdge,
  MarkerType,
  Position,
  getStraightPath,
  useInternalNode,
  type Edge,
  type EdgeMarker,
  type EdgeProps,
  type InternalNode,
  type Node,
  type XYPosition,
} from '@xyflow/react'

/** Arrowhead for an edge of a given colour. React Flow makes one `<marker>` def per colour. */
export function arrowMarker(color: string, scale = 1): EdgeMarker {
  return {type: MarkerType.ArrowClosed, color, width: 16 * scale, height: 16 * scale}
}

const distance = (a: XYPosition, b: XYPosition) => Math.hypot(b.x - a.x, b.y - a.y)

/** The point `away` units from `from` on the way to `to`. */
function towards(from: XYPosition, to: XYPosition, away: number): XYPosition {
  const length = distance(from, to)
  if (length === 0) return from
  const ratio = Math.min(1, away / length)
  return {x: from.x + (to.x - from.x) * ratio, y: from.y + (to.y - from.y) * ratio}
}

/**
 * An SVG path through `points` with the corners rounded off — the corridor routing the subflow map
 * draws from dagre's bend points.
 */
export function roundedPolylinePath(points: XYPosition[], radius = 14): string {
  if (points.length < 2) return ''
  const first = points[0]!
  const parts = [`M ${first.x},${first.y}`]

  for (let i = 1; i < points.length - 1; i++) {
    const previous = points[i - 1]!
    const corner = points[i]!
    const next = points[i + 1]!
    const into = towards(corner, previous, Math.min(radius, distance(previous, corner) / 2))
    const out = towards(corner, next, Math.min(radius, distance(corner, next) / 2))
    parts.push(`L ${into.x},${into.y}`, `Q ${corner.x},${corner.y} ${out.x},${out.y}`)
  }

  const last = points[points.length - 1]!
  parts.push(`L ${last.x},${last.y}`)
  return parts.join(' ')
}

export type PolylineEdgeData = {points?: XYPosition[]; radius?: number}
export type PolylineEdgeType = Edge<PolylineEdgeData, 'polyline'>

/**
 * Edge that follows dagre's own routing: it keeps clear of the nodes between its ends, which is
 * what the map's "corridor" routing looked like when Cytoscape drew it with round segments.
 * Without points it falls back to a straight line, so a layout that does not route still works.
 */
export const PolylineEdge = memo(function PolylineEdge({
  sourceX,
  sourceY,
  targetX,
  targetY,
  markerEnd,
  style,
  data,
  interactionWidth,
}: EdgeProps<PolylineEdgeType>) {
  const source = {x: sourceX, y: sourceY}
  const target = {x: targetX, y: targetY}
  const bends = (data?.points ?? []).slice(1, -1)
  const path = roundedPolylinePath([source, ...bends, target], data?.radius)
  return <BaseEdge path={path} markerEnd={markerEnd} style={style} interactionWidth={interactionWidth} />
})

/** Where a line from `from` to `to` leaves the rectangle centred on `from`. */
function boundaryPoint(rect: {center: XYPosition; width: number; height: number}, to: XYPosition): XYPosition {
  const dx = to.x - rect.center.x
  const dy = to.y - rect.center.y
  if (dx === 0 && dy === 0) return rect.center
  const scaleX = dx === 0 ? Number.POSITIVE_INFINITY : rect.width / 2 / Math.abs(dx)
  const scaleY = dy === 0 ? Number.POSITIVE_INFINITY : rect.height / 2 / Math.abs(dy)
  const scale = Math.min(scaleX, scaleY)
  return {x: rect.center.x + dx * scale, y: rect.center.y + dy * scale}
}

function rectOf(node: InternalNode<Node>) {
  const width = node.measured.width ?? node.width ?? 0
  const height = node.measured.height ?? node.height ?? 0
  const at = node.internals.positionAbsolute
  return {center: {x: at.x + width / 2, y: at.y + height / 2}, width, height}
}

/**
 * Straight edge between the two node borders, whichever side they face — the shape Cytoscape drew
 * for layouts with no fixed flow direction (radial, cluster, the lineage strip), where a fixed
 * handle on one side would send edges the long way round the card.
 */
export const FloatingEdge = memo(function FloatingEdge({
  source,
  target,
  markerEnd,
  style,
  interactionWidth,
}: EdgeProps) {
  const sourceNode = useInternalNode(source)
  const targetNode = useInternalNode(target)
  if (!sourceNode || !targetNode) return null

  const from = rectOf(sourceNode)
  const to = rectOf(targetNode)
  const start = boundaryPoint(from, to.center)
  const end = boundaryPoint(to, from.center)
  const [path] = getStraightPath({sourceX: start.x, sourceY: start.y, targetX: end.x, targetY: end.y})

  return <BaseEdge path={path} markerEnd={markerEnd} style={style} interactionWidth={interactionWidth} />
})

export const flowEdgeTypes = {polyline: PolylineEdge, floating: FloatingEdge}

/** Handle sides for a layered layout, matching Cytoscape's source/target endpoints. */
export function handleSides(direction: 'LR' | 'TB'): {sourcePosition: Position; targetPosition: Position} {
  return direction === 'LR'
    ? {sourcePosition: Position.Right, targetPosition: Position.Left}
    : {sourcePosition: Position.Bottom, targetPosition: Position.Top}
}
