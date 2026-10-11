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
        const child = block.children[i]
        const prev = block.children[i - 1]
        if (!child) continue
        processBlock(child, block.id, i)
        if (i > 0 && prev) {
          elements.push({
            group: 'edges',
            data: {
              id: `edge-seq-${prev.id}-${child.id}`,
              source: prev.id,
              target: child.id,
            },
          })
        }
      }
    }
  }

  for (let i = 0; i < subflow.blocks.length; i++) {
    const block = subflow.blocks[i]
    const prev = subflow.blocks[i - 1]
    if (!block) continue
    processBlock(block, null, i)
    if (i > 0 && prev) {
      elements.push({
        group: 'edges',
        data: {
          id: `edge-seq-${prev.id}-${block.id}`,
          source: prev.id,
          target: block.id,
        },
      })
    }
  }

  return elements
}

export function countSubflowNodes(blocks: Block[]): number {
  let n = blocks.length
  for (const b of blocks) {
    if (b.children.length > 0) n += countSubflowNodes(b.children)
  }
  return n
}
