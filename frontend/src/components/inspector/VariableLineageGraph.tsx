import {useEffect, useRef, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import cytoscape from 'cytoscape'
import dagre from 'cytoscape-dagre'
import type {VariableEvent} from '@/types'
import {useFlowStore} from '@/stores/flowStore'
import {Info} from 'lucide-react'

cytoscape.use(dagre)

const EVENT_COLOR: Record<VariableEvent['type'], string> = {
  init: '#22c55e',
  mutate: '#f59e0b',
  read: '#3b82f6',
}

function dominantType(types: Set<VariableEvent['type']>): VariableEvent['type'] {
  if (types.has('init')) return 'init'
  if (types.has('mutate')) return 'mutate'
  return 'read'
}

type Props = {
  events: VariableEvent[]
}

export default function VariableLineageGraph({events}: Props) {
  const {t} = useTranslation('inspector')
  const navigateToBlock = useFlowStore(s => s.navigateToBlock)
  const containerRef = useRef<HTMLDivElement>(null)
  const cyRef = useRef<cytoscape.Core | null>(null)

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
    const edges = []
    const seen = new Set<string>()
    let edgeIdx = 0
    for (let i = 1; i < events.length; i++) {
      const src = events[i - 1]?.blockId
      const tgt = events[i]?.blockId
      if (src === undefined || tgt === undefined || src === tgt) continue
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

    instance
      .layout({name: 'dagre', rankDir: 'LR', nodeSep: 30, rankSep: 50, animate: false} as cytoscape.LayoutOptions)
      .run()
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
        <p className="text-[11px]">{t('lineage.noMatchingEvents')}</p>
      </div>
    )
  }

  return <div ref={containerRef} className="w-full h-44 rounded-lg border border-border-subtle bg-surface-1" />
}
