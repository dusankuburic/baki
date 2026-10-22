import type {SubflowMapLayout, SubflowMapDirection, SubflowMapRouting} from '@/stores/uiStore'
import {layoutClusters, layoutDagre, layoutRadial, type GraphLayout} from '@/components/graph/flow/layout'
import type {EnhancedGraphData} from './model'

export const NODE_CARD_WIDTH = 248
export const NODE_CARD_HEIGHT = 76

/** Edge ids are derived from their ends: the API gives one edge per subflow call pair. */
export function mapEdgeId(source: string, target: string): string {
  return `${source}->${target}`
}

/**
 * Positions for the subflow map, in the layout the user picked.
 *
 * Hierarchy keeps dagre's bend points so the corridor routing can follow them; radial and cluster
 * route straight between card borders, so they return none.
 */
export function layoutSubflowMap(
  data: EnhancedGraphData,
  layout: SubflowMapLayout,
  direction: SubflowMapDirection,
  routing: SubflowMapRouting,
): GraphLayout {
  const nodes = data.nodes.map(node => ({
    id: node.id,
    width: NODE_CARD_WIDTH,
    height: NODE_CARD_HEIGHT,
    depth: node.depth,
  }))
  const edges = data.edges.map(edge => ({
    id: mapEdgeId(edge.source, edge.target),
    source: edge.source,
    target: edge.target,
  }))

  if (layout === 'radial') return layoutRadial(nodes, {gap: 70})
  if (layout === 'cluster') return layoutClusters(nodes, edges, {rankdir: direction, gap: 90})

  const laid = layoutDagre(nodes, edges, {
    rankdir: direction,
    nodesep: direction === 'LR' ? 60 : 70,
    ranksep: direction === 'LR' ? 100 : 80,
  })
  return routing === 'corridor' ? laid : {positions: laid.positions, points: new Map()}
}
