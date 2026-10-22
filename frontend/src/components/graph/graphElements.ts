import type {Block, Subflow} from '@/types'
import {resolveTypeLabel, stripBlockKeywords} from '@/lib/blocks'
import type {LayoutEdge} from './flow/layout'

export const BLOCK_NODE_WIDTH = 210
export const BLOCK_NODE_HEIGHT = 88
export const BLOCK_NODE_TYPE = 'block'

/** A block node's own data, as a type so React Flow accepts it as node data. */
export type BlockNodeData = {
  label: string
  type: string
  subflowId?: string
  hasChildren: boolean
  variables: string[]
  lineNumber?: number
  /** Findings on this block, filled in by the view once the analysis report is in. */
  findingCount?: number
}

export interface BlockGraph {
  nodes: Array<{id: string; data: BlockNodeData}>
  edges: LayoutEdge[]
}

/**
 * The flow's blocks as a graph: one node per block, an entry edge into the first child and a
 * sequence edge between siblings. Ids are the block ids and the previous `edge-entry-…` /
 * `edge-seq-…` scheme, so selection and the e2e specs keep working.
 */
export function blocksToFlow(subflow: Subflow): BlockGraph {
  const nodes: BlockGraph['nodes'] = []
  const edges: LayoutEdge[] = []
  const visited = new Set<string>()

  function processBlock(block: Block, parentId: string | null, siblingIndex: number) {
    if (visited.has(block.id)) return
    visited.add(block.id)

    const typeLabel = resolveTypeLabel(block.type, block.name, block.rawType)
    const strippedName = stripBlockKeywords(block.type, block.name)

    const hideHeaderTypes = ['SWITCH', 'CASE', 'CONDITION']
    const shouldHideHeader = strippedName && hideHeaderTypes.includes(block.type)

    const label = shouldHideHeader
      ? strippedName
      : strippedName && strippedName.toLowerCase() !== typeLabel.toLowerCase()
        ? `${typeLabel}\n${strippedName}`
        : typeLabel

    nodes.push({
      id: block.id,
      data: {
        label,
        type: block.type,
        subflowId: block.subflowId,
        hasChildren: block.children.length > 0,
        variables: block.variables || [],
        lineNumber: block.lineNumber,
      },
    })

    if (parentId && siblingIndex === 0) {
      edges.push({id: `edge-entry-${parentId}-${block.id}`, source: parentId, target: block.id})
    }

    for (let i = 0; i < block.children.length; i++) {
      const child = block.children[i]
      const prev = block.children[i - 1]
      if (!child) continue
      processBlock(child, block.id, i)
      if (i > 0 && prev) {
        edges.push({id: `edge-seq-${prev.id}-${child.id}`, source: prev.id, target: child.id})
      }
    }
  }

  for (let i = 0; i < subflow.blocks.length; i++) {
    const block = subflow.blocks[i]
    const prev = subflow.blocks[i - 1]
    if (!block) continue
    processBlock(block, null, i)
    if (i > 0 && prev) {
      edges.push({id: `edge-seq-${prev.id}-${block.id}`, source: prev.id, target: block.id})
    }
  }

  return {nodes, edges}
}

export function countSubflowNodes(blocks: Block[]): number {
  let n = blocks.length
  for (const b of blocks) {
    if (b.children.length > 0) n += countSubflowNodes(b.children)
  }
  return n
}
