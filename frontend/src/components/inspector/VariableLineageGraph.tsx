import {useEffect, useRef, useMemo} from 'react'
import cytoscape from 'cytoscape'
// @ts-expect-error cytoscape-dagre has no types
import dagre from 'cytoscape-dagre'
import type {VariableEvent} from '@/types'
import {useFlowStore} from '@/stores/flowStore'
import {Info} from 'lucide-react'

cytoscape.use(dagre)

// EVENT_COLOR maps an event type to a node accent color. The color matches the
// timeline chips in VariableLineageInInspector so the graph + list agree.
const EVENT_COLOR: Record<VariableEvent['type'], string> = {
  init: '#22c55e', // green
  mutate: '#f59e0b', // amber
  read: '#3b82f6', // blue
}

// dominantType picks the strongest event type on a block that hosts several
// events (init beats mutate beats read — an init is the defining touch).
function dominantType(types: Set<VariableEvent['type']>): VariableEvent['type'] {
  if (types.has('init')) return 'init'
  if (types.has('mutate')) return 'mutate'
  return 'read'
}

type Props = {
  events: VariableEvent[]
}

/**
 * Variable lineage rendered as a Cytoscape DAG. Each node is a distinct block
 * the variable passes through (collapsing repeated events on the same block),
 * colored by the dominant event type (init/mutate/read). Edges follow event
 * order, so the variable's journey across blocks/subflows reads left-to-right.
 *
 * Replaces the earlier linear SVG: that view laid every event in one row, so a
 * variable touched many times on a few blocks produced a long redundant strip
 * with no cross-subflow structure. This Cytoscape view mirrors GraphView's
 * mount/destroy lifecycle and supports pan/zoom + click-to-navigate.
 */
export default function VariableLineageGraph({events}: Props) {
  const navigateToBlock = useFlowStore(s => s.navigateToBlock)
  const containerRef = useRef<HTMLDivElement>(null)
  const cyRef = useRef<cytoscape.Core | null>(null)

  // Build Cytoscape elements: one node per distinct blockId, edges in event
  // order (skipping self-loops where consecutive events hit the same block).
  const elements = useMemo(() => {
    if (events.length === 0) return []
    const byBlock = new Map<string, {types: Set<VariableEvent['type']>; line: number; subflowId: string}>()
    for (const e of events) {
      let entry = byBlock.get(e.blockId)
      if (!entry) {
        entry = {types: new Set(), line: e.line, subflowId: e.subflowId}
        byBlock.set(e.blockId, entry)
      }
      entry.types.add(e.type)
    }
    const nodes = [...byBlock.entries()].map(([blockId, info]) => ({
      group: 'nodes' as const,
      data: {
        id: blockId,
        blockId,
        line: info.line,
        subflowId: info.subflowId,
        color: EVENT_COLOR[dominantType(info.types)],
        label: `L${info.line}`,
      },
    }))
    // Edges: connect event[i].blockId → event[i+1].blockId, deduped + no self.
    const edges = []
    const seen = new Set<string>()
    let edgeIdx = 0
    for (let i = 1; i < events.length; i++) {
      const src = events[i - 1].blockId
      const tgt = events[i].blockId
      if (src === tgt) continue
      const key = `${src}->${tgt}`
      if (seen.has(key)) continue
      seen.add(key)
      edges.push({group: 'edges' as const, data: {id: `e${edgeIdx++}`, source: src, target: tgt}})
    }
    return [...nodes, ...edges]
  }, [events])

  useEffect(() => {
    if (!containerRef.current || elements.length === 0) return

    const instance = cytoscape({
      container: containerRef.current,
      elements,
      minZoom: 0.3,
      maxZoom: 3,
      style: [
        {
          selector: 'node',
          style: {
            'background-color': 'data(color)',
            label: 'data(label)',
            color: '#9ca3af',
            'font-size': 9,
            'text-valign': 'bottom',
            'text-margin-y': 4,
            width: 22,
            height: 22,
            'border-width': 2,
            'border-color': '#ffffff22',
          },
        },
        {
          selector: 'edge',
          style: {
            'curve-style': 'bezier',
            'target-arrow-shape': 'triangle',
            'line-color': '#6b728055',
            'target-arrow-color': '#6b728055',
            width: 1.5,
          },
        },
      ],
    })

    instance.on('tap', 'node', evt => {
      const blockId = evt.target.data('blockId')
      if (blockId) navigateToBlock(blockId)
    })

    instance.layout({name: 'dagre', rankDir: 'LR', nodeSep: 30, rankSep: 50, animate: false} as cytoscape.LayoutOptions).run()
    instance.fit(undefined, 30)

    cyRef.current = instance
    return () => {
      instance.destroy()
      cyRef.current = null
    }
  }, [elements, navigateToBlock])

  if (events.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-6 opacity-40">
        <Info size={20} className="mb-1.5" />
        <p className="text-[11px]">No events match the active filters.</p>
      </div>
    )
  }

  return <div ref={containerRef} className="w-full h-44 rounded-lg border border-border-subtle bg-surface-1" />
}
