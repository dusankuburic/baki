import {useRef, useEffect, useState} from 'react'
import cytoscape from 'cytoscape'
// @ts-expect-error cytoscape-dagre has no types
import dagre from 'cytoscape-dagre'
import {analysisApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'

cytoscape.use(dagre)

// useCytoscapeGraph owns the Cytoscape instance lifecycle (create/destroy,
// theme-derived styling), data loading (execution-graph fetch, element
// swap, layout), zoom sync with the toolbar, the window "graph:fit" event,
// and node search/highlight. Extracted from ExecutionGraphView so the
// component itself only wires the container ref and renders overlays.
export function useCytoscapeGraph(subflowId?: string) {
  const containerRef = useRef<HTMLDivElement>(null)
  const cyRef = useRef<cytoscape.Core | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [searchQuery, setSearchQuery] = useState('')
  const [matchCount, setMatchCount] = useState<number | null>(null)

  const flowDoc = useFlowStore(s => s.document)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  const graphZoom = useUIStore(s => s.graphZoom)
  const setGraphZoom = useUIStore(s => s.setGraphZoom)
  const report = useAnalysisStore(s => flowDoc ? s.reports.get(flowDoc.id) : undefined)

  const loadGraph = async () => {
    if (!flowDoc) return
    const inst = cyRef.current
    setLoading(true)
    setError(null)
    try {
      const data = await analysisApi.getExecutionGraph()
      if (data && cyRef.current && cyRef.current === inst) {
        let nodes = data.nodes
        let edges = data.edges

        if (subflowId) {
          const connected = new Set<string>()
          connected.add(subflowId)
          edges.forEach(e => {
            if (e.source === subflowId || e.target === subflowId) {
              connected.add(e.source)
              connected.add(e.target)
            }
          })
          nodes = nodes.filter(n => connected.has(n.id))
          edges = edges.filter(e => e.source === subflowId || e.target === subflowId)
        }

        const elements = [
          ...nodes.map(n => ({
            data: {
              id: n.id,
              label: n.label,
              blockCount: n.blockCount,
              errorCount: n.errorCount,
              warnCount: n.warnCount,
              isCenter: n.id === subflowId ? 'true' : 'false'
            }
          })),
          ...edges.map(e => ({
            data: { source: e.source, target: e.target }
          }))
        ]

        const cy = cyRef.current
        // Batch the remove+add so the whole swap restyles once, not per element.
        cy.batch(() => {
          cy.elements().remove()
          cy.add(elements)
        })

        cy.layout({
          name: 'dagre',
          rankDir: 'LR',
          nodeSep: subflowId ? 120 : 80,
          rankSep: subflowId ? 160 : 120,
          animate: true,
          animationDuration: 500
        } as cytoscape.LayoutOptions).run()

        cy.fit(undefined, 50)
        setGraphZoom(cy.zoom())
      }
    } catch (err) {
      if (cyRef.current === inst) setError(err instanceof Error ? err.message : String(err))
    } finally {
      if (cyRef.current === inst) setLoading(false)
    }
  }

  useEffect(() => {
    if (!containerRef.current) return

    const cs = getComputedStyle(document.documentElement)
    const cssVar = (v: string) => cs.getPropertyValue(v).trim()

    const instance = cytoscape({
      container: containerRef.current,
      style: [
        {
          selector: 'node',
          style: {
            'shape': 'round-rectangle',
            'width': '220px',
            'height': '80px',
            'background-color': cssVar('--surface-2'),
            'border-width': 2,
            'border-color': cssVar('--border-strong'),
            'label': 'data(label)',
            'color': cssVar('--text-primary'),
            'font-size': '14px',
            'font-weight': 'bold',
            'text-valign': 'center',
            'text-halign': 'center',
            'text-margin-y': -10,
            'padding': '10px'
          }
        },
        {
          selector: 'node[isCenter = "true"]',
          style: {
            'border-color': cssVar('--brand-500'),
            'border-width': 3,
            'background-color': cssVar('--surface-3')
          }
        },
        {
          selector: 'edge',
          style: {
            'width': 2,
            'line-color': cssVar('--border-strong'),
            'target-arrow-color': cssVar('--border-strong'),
            'target-arrow-shape': 'triangle',
            'curve-style': 'bezier'
          }
        },
        {
          selector: 'node:selected',
          style: {
            'border-color': cssVar('--brand-500'),
            'border-width': 3
          }
        },
        // Error indicator
        {
          selector: 'node[errorCount > 0]',
          style: {
            'border-color': cssVar('--error'),
            'border-width': 2
          }
        },
        // Warning indicator
        {
          selector: 'node[warnCount > 0][errorCount = 0]',
          style: {
            'border-color': cssVar('--warning'),
            'border-width': 2
          }
        },
        // Search highlight + dim
        {
          selector: 'node.search-match',
          style: {
            'border-color': cssVar('--brand-500'),
            'border-width': 4,
            'background-color': cssVar('--surface-3')
          }
        },
        {
          selector: '.search-dim',
          style: {
            'opacity': 0.2
          }
        }
      ],
      minZoom: 0.1,
      maxZoom: 2,
    })

    instance.on('zoom', () => {
      setGraphZoom(instance.zoom())
    })

    instance.on('dblclick', 'node', (evt) => {
      selectSubflow(evt.target.id())
    })

    cyRef.current = instance
    loadGraph()

    return () => {
      instance.destroy()
      cyRef.current = null
    }
  }, [resolvedTheme]) // eslint-disable-line react-hooks/exhaustive-deps
  // ↑ The Cytoscape instance and its style are derived only from the theme's
  // CSS vars (getComputedStyle), so it is created once per theme change rather
  // than recreated on every subflow switch. The loadGraph effect below handles
  // subflowId changes by swapping elements on the reused instance.

  // Sync zoom from toolbar
  useEffect(() => {
    if (!cyRef.current) return
    const cy = cyRef.current
    if (Math.abs(cy.zoom() - graphZoom) > 0.01) {
      cy.zoom(graphZoom)
      cy.center()
    }
  }, [graphZoom])

  // Listen for fit event
  useEffect(() => {
    const handleFit = () => {
      if (!cyRef.current) return
      cyRef.current.fit(undefined, 50)
      setGraphZoom(cyRef.current.zoom())
    }
    window.addEventListener('graph:fit', handleFit)
    return () => window.removeEventListener('graph:fit', handleFit)
  }, [setGraphZoom])

  // Re-load when document or report changes
  useEffect(() => {
    loadGraph()
  }, [flowDoc?.id, report?.generatedAt, subflowId]) // eslint-disable-line react-hooks/exhaustive-deps

  // Highlight matched nodes (and dim the rest) live as the query changes; re-runs
  // after a (re)load so highlights survive graph refreshes. Also drives the count.
  useEffect(() => {
    const cy = cyRef.current
    if (!cy) return
    const q = searchQuery.trim().toLowerCase()
    if (!q) {
      cy.elements().removeClass('search-match search-dim')
      setMatchCount(null)
      return
    }
    const matched = cy.nodes().filter(n => String(n.data('label') ?? '').toLowerCase().includes(q))
    cy.batch(() => {
      cy.elements().removeClass('search-match search-dim')
      matched.addClass('search-match')
      cy.elements().not(matched).addClass('search-dim')
    })
    setMatchCount(matched.length)
  }, [searchQuery, loading])

  const handleSearch = (e: React.FormEvent) => {
    e.preventDefault()
    if (!cyRef.current || !searchQuery) return
    const cy = cyRef.current
    const nodes = cy.nodes().filter(n =>
      String(n.data('label') ?? '').toLowerCase().includes(searchQuery.toLowerCase())
    )
    if (nodes.length > 0) {
      cy.nodes().unselect()
      nodes.select()
      cy.animate({
        center: { eles: nodes },
        zoom: 0.8,
        duration: 500
      })
    }
  }

  return {
    containerRef,
    loading,
    error,
    searchQuery,
    setSearchQuery,
    matchCount,
    handleSearch,
    retry: loadGraph,
  }
}
