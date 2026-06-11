import {useRef, useEffect, useState, useCallback} from 'react'
import cytoscape from 'cytoscape'
// @ts-expect-error cytoscape-dagre has no types
import dagre from 'cytoscape-dagre'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {blocksToElements, countSubflowNodes} from './graphElements'
import {resolveGraphTokens, buildGraphStyle} from './graphStyle'
import type {GraphTokenColors} from './graphStyle'
import Minimap from './Minimap'
import {EmptyState} from '@/components/shared'

cytoscape.use(dagre)

const GRAPH_NODE_LIMIT = 2000

export default function GraphView({subflowId: subflowIdProp}: {subflowId?: string} = {}) {
  const containerRef = useRef<HTMLDivElement>(null)
  const cyRef = useRef<cytoscape.Core | null>(null)
  const isProgrammaticZoom = useRef(false)
  const [cy, setCy] = useState<cytoscape.Core | null>(null)
  const [graphTokens, setGraphTokens] = useState<GraphTokenColors>(() => resolveGraphTokens())

  // Renamed from `document` to avoid shadowing window.document.
  const flowDoc = useFlowStore(s => s.document)
  const selectedSubflowId = useFlowStore(s => s.selectedSubflowId)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const graphZoom = useUIStore(s => s.graphZoom)
  const setGraphZoom = useUIStore(s => s.setGraphZoom)
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  const selectedVariable = useUIStore(s => s.selectedVariable)

  const subflowId = subflowIdProp ?? selectedSubflowId
  const subflow = flowDoc?.subflows.find(s => s.id === subflowId) ?? flowDoc?.subflows[0]

  const syncSelectionToGraph = useCallback((blockId: string | null) => {
    if (!cyRef.current) return
    const c = cyRef.current
    c.nodes().unselect()
    if (blockId) {
      const node = c.getElementById(blockId)
      if (node.length > 0) node.select()
    }
  }, [])

  // Initialise Cytoscape once on mount.
  useEffect(() => {
    if (!containerRef.current) return

    const tokens = resolveGraphTokens()
    const instance = cytoscape({
      container: containerRef.current,
      elements: [],
      style: buildGraphStyle(tokens),
      minZoom: 0.2,
      maxZoom: 3,
    })

    instance.on('tap', 'node', (evt) => {
      selectBlock(evt.target.id())
    })

    instance.on('dblclick', 'node', (evt) => {
      const node = evt.target
      if (node.data('type') === 'SUBFLOW' || node.data('hasChildren')) {
        const subId = node.data('subflowId')
        if (subId) selectSubflow(subId)
      }
    })

    // Only propagate zoom events that the user initiated, not programmatic ones.
    instance.on('zoom', () => {
      if (!isProgrammaticZoom.current) setGraphZoom(instance.zoom())
    })

    cyRef.current = instance
    setCy(instance)

    return () => {
      instance.destroy()
      cyRef.current = null
      setCy(null)
    }
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // Reload elements whenever the displayed subflow changes.
  useEffect(() => {
    if (!cyRef.current || !subflow) return
    const c = cyRef.current

    const elements = blocksToElements(subflow)
    c.elements().remove()
    c.add(elements)

    if (elements.length > 0) {
      c.layout({
        name: 'dagre',
        rankDir: 'TB',
        nodeSep: 40,
        rankSep: 60,
        animate: false,
      } as any).run()
    }

    c.fit(undefined, 40)
    setGraphZoom(c.zoom())

    if (selectedBlockId) syncSelectionToGraph(selectedBlockId)
  }, [subflow?.id, flowDoc?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  // Keep graph selection in sync with the sidebar/inspector selection.
  useEffect(() => {
    syncSelectionToGraph(selectedBlockId)
  }, [selectedBlockId, syncSelectionToGraph])

  // Re-style when the app theme changes and keep tokens in sync for Minimap.
  useEffect(() => {
    const tokens = resolveGraphTokens()
    setGraphTokens(tokens)
    if (!cyRef.current) return
    cyRef.current.style().fromJson(buildGraphStyle(tokens) as any).update()
  }, [resolvedTheme])

  // Sync zoom from toolbar controls (±/fit buttons) to Cytoscape.
  // isProgrammaticZoom prevents the 'zoom' event from echoing the change back.
  useEffect(() => {
    if (!cyRef.current) return
    const c = cyRef.current
    if (Math.abs(c.zoom() - graphZoom) > 0.01) {
      isProgrammaticZoom.current = true
      c.zoom(graphZoom)
      c.center()
      isProgrammaticZoom.current = false
    }
  }, [graphZoom])

  // Fit-to-screen event dispatched by the toolbar's Maximize button.
  useEffect(() => {
    const handleFit = () => {
      if (!cyRef.current) return
      cyRef.current.fit(undefined, 40)
      // Update the zoom indicator to the actual post-fit zoom, not a hardcoded 1.
      setGraphZoom(cyRef.current.zoom())
    }
    window.addEventListener('graph:fit', handleFit)
    return () => window.removeEventListener('graph:fit', handleFit)
  }, [setGraphZoom])

  // Highlight blocks containing the globally selected variable
  useEffect(() => {
    if (!cyRef.current) return
    const cy = cyRef.current

    if (!selectedVariable) {
      cy.elements().removeClass('variable-highlighted variable-dimmed')
      return
    }

    cy.batch(() => {
      cy.elements().removeClass('variable-highlighted variable-dimmed')

      const nodes = cy.nodes()
      nodes.forEach(node => {
        const vars = node.data('variables') as string[] | undefined
        if (vars && vars.includes(selectedVariable)) {
          node.addClass('variable-highlighted')
        } else {
          node.addClass('variable-dimmed')
        }
      })

      cy.edges().forEach(edge => {
        const src = edge.source()
        const tgt = edge.target()
        if (!src.hasClass('variable-highlighted') && !tgt.hasClass('variable-highlighted')) {
          edge.addClass('variable-dimmed')
        }
      })
    })
  }, [selectedVariable, subflow?.id])

  // Annotate nodes with finding severity borders and a count badge in the
  // label ("⚠ n"), so density is readable at a glance without opening the tab.
  useEffect(() => {
    if (!cyRef.current || !flowDoc) return
    const cy = cyRef.current

    const report = useAnalysisStore.getState().reports.get(flowDoc.id)
    cy.batch(() => {
      // Reset classes and restore the pristine label (stashed in scratch so
      // repeated runs of this effect stay idempotent).
      cy.nodes().forEach(node => {
        node.removeClass('finding-error finding-warning finding-info')
        const base = node.scratch('_baseLabel') ?? node.data('fullLabel')
        node.scratch('_baseLabel', base)
        if (node.data('fullLabel') !== base) node.data('fullLabel', base)
      })
      if (!report) return

      const counts = new Map<string, number>()
      for (const f of report.findings) {
        const node = cy.getElementById(f.blockId)
        if (node.length > 0) {
          const cls = f.severity === 'error' ? 'finding-error'
            : f.severity === 'warning' ? 'finding-warning'
            : 'finding-info'
          node.addClass(cls)
          counts.set(f.blockId, (counts.get(f.blockId) ?? 0) + 1)
        }
      }
      for (const [blockId, count] of counts) {
        const node = cy.getElementById(blockId)
        node.data('fullLabel', `${node.scratch('_baseLabel')}\n⚠ ${count} issue${count !== 1 ? 's' : ''}`)
      }
    })
  }, [flowDoc, useAnalysisStore(s => s.reports), subflow?.id])

  if (!flowDoc) {
    return (
      <div className="flex-1 flex items-center justify-center bg-surface-0">
        <EmptyState title="No flow loaded" description="Open a flow file to see the graph view" />
      </div>
    )
  }

  // Count all nodes recursively, not just root blocks.
  const totalNodes = subflow ? countSubflowNodes(subflow.blocks) : 0
  if (totalNodes > GRAPH_NODE_LIMIT) {
    return (
      <div className="flex-1 flex items-center justify-center bg-surface-0 p-8 text-center">
        <div>
          <p className="text-lg font-medium text-text-primary mb-2">Large flow detected</p>
          <p className="text-sm text-text-secondary">
            This subflow has {totalNodes} blocks (limit: {GRAPH_NODE_LIMIT}).
            Use the block view for navigation and analysis.
          </p>
        </div>
      </div>
    )
  }

  if (subflow && subflow.blocks.length === 0) {
    return (
      <div className="flex-1 flex items-center justify-center bg-surface-0">
        <EmptyState title="Empty subflow" description="This subflow has no blocks to display" />
      </div>
    )
  }

  return (
    <div className="flex-1 relative bg-surface-0">
      <div ref={containerRef} className="absolute inset-0" />
      <Minimap cy={cy} tokens={graphTokens} />
    </div>
  )
}
