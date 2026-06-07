import {useRef, useEffect, useState} from 'react'
import cytoscape from 'cytoscape'
// @ts-expect-error cytoscape-dagre has no types
import dagre from 'cytoscape-dagre'
import {analysisApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {Loader2, AlertCircle, AlertTriangle, Search} from 'lucide-react'

cytoscape.use(dagre)

export default function ExecutionGraphView({subflowId}: {subflowId?: string} = {}) {
  const containerRef = useRef<HTMLDivElement>(null)
  const cyRef = useRef<cytoscape.Core | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [searchQuery, setSearchQuery] = useState('')
  
  const flowDoc = useFlowStore(s => s.document)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  const graphZoom = useUIStore(s => s.graphZoom)
  const setGraphZoom = useUIStore(s => s.setGraphZoom)
  const report = useAnalysisStore(s => flowDoc ? s.reports.get(flowDoc.id) : undefined)

  const loadGraph = async () => {
    if (!flowDoc) return
    setLoading(true)
    setError(null)
    try {
      const data = await analysisApi.getExecutionGraph()
      if (data && cyRef.current) {
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
        cy.elements().remove()
        cy.add(elements)
        
        cy.layout({
          name: 'dagre',
          rankDir: 'LR',
          nodeSep: subflowId ? 120 : 80,
          rankSep: subflowId ? 160 : 120,
          animate: true,
          animationDuration: 500
        } as any).run()
        
        cy.fit(undefined, 50)
        setGraphZoom(cy.zoom())
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
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
            'border-color': '#6366f1',
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
            'border-color': '#6366f1',
            'border-width': 3
          }
        },
        // Error indicator
        {
          selector: 'node[errorCount > 0]',
          style: {
            'border-color': '#ef4444',
            'border-width': 2
          }
        },
        // Warning indicator
        {
          selector: 'node[warnCount > 0][errorCount = 0]',
          style: {
            'border-color': '#eab308',
            'border-width': 2
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
  }, [resolvedTheme, subflowId]) // eslint-disable-line react-hooks/exhaustive-deps

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

  const handleSearch = (e: React.FormEvent) => {
    e.preventDefault()
    if (!cyRef.current || !searchQuery) return
    const cy = cyRef.current
    const nodes = cy.nodes().filter(n => 
      n.data('label').toLowerCase().includes(searchQuery.toLowerCase())
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

  return (
    <div className="flex-1 relative bg-surface-0 overflow-hidden">
      {/* Search Overlay */}
      <form 
        onSubmit={handleSearch}
        className="absolute top-6 left-6 z-20 flex items-center gap-2 p-1 bg-surface-1/80 backdrop-blur-md border border-border-default rounded-lg shadow-xl"
      >
        <div className="pl-2 text-text-tertiary">
          <Search size={14} />
        </div>
        <input 
          type="text"
          placeholder="Find subflow..."
          className="bg-transparent border-none outline-none text-xs w-48 h-8 placeholder:text-text-tertiary"
          value={searchQuery}
          onChange={e => setSearchQuery(e.target.value)}
        />
      </form>

      {loading && (
        <div className="absolute inset-0 z-10 flex items-center justify-center bg-surface-0/50 backdrop-blur-sm">
          <Loader2 className="animate-spin text-brand-500" size={32} />
        </div>
      )}
      
      {error && (
        <div className="absolute inset-0 z-10 flex items-center justify-center p-4">
          <div className="bg-semantic-error/10 border border-semantic-error/20 rounded-lg p-6 max-w-md text-center">
            <AlertCircle className="text-semantic-error mx-auto mb-4" size={48} />
            <h3 className="text-lg font-bold text-text-primary mb-2">Graph Error</h3>
            <p className="text-sm text-text-secondary mb-4">{error}</p>
            <button 
              onClick={loadGraph}
              className="px-4 py-2 bg-semantic-error text-white rounded-md hover:bg-semantic-error/90 transition-colors"
            >
              Retry
            </button>
          </div>
        </div>
      )}

      <div ref={containerRef} className="absolute inset-0" />
      
      {/* Legend / Stats overlay */}
      {!loading && !error && (
        <div className="absolute bottom-6 left-6 p-4 bg-surface-1/80 backdrop-blur-md border border-border-default rounded-xl shadow-lg pointer-events-none animate-slide-up">
          <h4 className="text-2xs font-black uppercase tracking-widest text-text-tertiary mb-3">Subflow Map Legend</h4>
          <div className="space-y-2">
            {subflowId && (
              <div className="flex items-center gap-3">
                <div className="w-3 h-3 rounded border-[3px] border-[#6366f1] bg-[#f5f7ff] dark:bg-[#23232b]" />
                <span className="text-xs text-text-secondary font-medium">Current Subflow</span>
              </div>
            )}
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded bg-brand-500" />
              <span className="text-xs text-text-secondary font-medium">Subflow Node (Dbl-click to open)</span>
            </div>
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded border border-semantic-error bg-semantic-error/10" />
              <span className="text-xs text-text-secondary font-medium flex items-center gap-1">
                <AlertCircle size={10} className="text-semantic-error" /> Subflow with Errors
              </span>
            </div>
            <div className="flex items-center gap-3">
              <div className="w-3 h-3 rounded border border-semantic-warning bg-semantic-warning/10" />
              <span className="text-xs text-text-secondary font-medium flex items-center gap-1">
                <AlertTriangle size={10} className="text-semantic-warning" /> Subflow with Warnings
              </span>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
