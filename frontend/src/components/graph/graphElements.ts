import type {Block, Subflow} from '@/types'
import type {ElementDefinition} from 'cytoscape'
import {resolveTypeLabel, stripBlockKeywords} from '@/lib/blocks'

export function blocksToElements(subflow: Subflow): ElementDefinition[] {
  const elements: ElementDefinition[] = []
  const visited = new Set<string>()

  function processBlock(block: Block, parentId: string | null, siblingIndex: number) {
    if (visited.has(block.id)) return
    visited.add(block.id)

    const typeLabel = resolveTypeLabel(block.type, block.name, block.rawType)
    const strippedName = stripBlockKeywords(block.type, block.name)

    // For specific control blocks, if we have a valid stripped name,
    // we omit the type header entirely to reduce redundancy as requested.
    const hideHeaderTypes = ['SWITCH', 'CASE', 'CONDITION']
    const shouldHideHeader = strippedName && hideHeaderTypes.includes(block.type)

    const fullLabel = shouldHideHeader
      ? strippedName
      : strippedName && strippedName.toLowerCase() !== typeLabel.toLowerCase()
        ? `${typeLabel}\n${strippedName}`
        : typeLabel

    elements.push({
      group: 'nodes',
      data: {
        id: block.id,
        name: block.name,
        typeLabel,
        strippedName,
        fullLabel,
        type: block.type,
        rawType: block.rawType,
        subflowId: block.subflowId,
        lineNumber: block.lineNumber,
        hasChildren: block.children.length > 0,
        variables: block.variables || [],
      },
    })

    // Edge from parent: only connect to the FIRST child (entry point into the
    // container). Subsequent children are reached via sequential sibling edges.
    // Connecting to all children produces a cluttered fan-out that dagre lays out
    // awkwardly with multiple incoming edges per child node.
    if (parentId && siblingIndex === 0) {
      elements.push({
        group: 'edges',
        data: {
          id: `edge-entry-${parentId}-${block.id}`,
          source: parentId,
          target: block.id,
        },
      })
    }

    if (block.children.length > 0) {
      for (let i = 0; i < block.children.length; i++) {
        processBlock(block.children[i], block.id, i)
        // Sequential edge between consecutive siblings (execution order).
        if (i > 0) {
          elements.push({
            group: 'edges',
            data: {
              id: `edge-seq-${block.children[i - 1].id}-${block.children[i].id}`,
              source: block.children[i - 1].id,
              target: block.children[i].id,
            },
          })
        }
      }
    }
  }

  for (let i = 0; i < subflow.blocks.length; i++) {
    processBlock(subflow.blocks[i], null, i)
    // Sequential edges between root-level blocks.
    if (i > 0) {
      elements.push({
        group: 'edges',
        data: {
          id: `edge-seq-${subflow.blocks[i - 1].id}-${subflow.blocks[i].id}`,
          source: subflow.blocks[i - 1].id,
          target: subflow.blocks[i].id,
        },
      })
    }
  }

  return elements
}

/** Counts all blocks in a subflow recursively (root + all descendants). */
export function countSubflowNodes(blocks: Block[]): number {
  let n = blocks.length
  for (const b of blocks) {
    if (b.children.length > 0) n += countSubflowNodes(b.children)
  }
  return n
}
