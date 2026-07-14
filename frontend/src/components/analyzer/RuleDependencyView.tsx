import {useRef, useEffect, useState} from 'react'
import cytoscape from 'cytoscape'
// @ts-expect-error cytoscape-dagre has no types
import dagre from 'cytoscape-dagre'
import {Loader2, AlertTriangle, RefreshCw} from 'lucide-react'
import {analysisApi} from '@/api'
import {useUIStore} from '@/stores/uiStore'
import {logger} from '@/lib/logger'
import type {DependencyAnalysis} from '@/types'

cytoscape.use(dagre)

// RuleDependencyView renders the analyzer's rule-dependency DAG (finally wiring
// up the previously-dormant GET /api/analysis/dependencies endpoint). It shows
// how the 29 rules depend on each other, the topological evaluation order, and
// any cycles (an analyzer-internal health signal). Static: not flow-specific.
export default function RuleDependencyView() {
  const containerRef = useRef<HTMLDivElement>(null)
  const cyRef = useRef<cytoscape.Core | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [data, setData] = useState<DependencyAnalysis | null>(null)
  const [selected, setSelected] = useState<string | null>(null)
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)

  // The DAG is static (rule catalog structure, not flow-specific), so keep the
  // last fetch in a ref: theme-driven instance rebuilds re-render from it
  // instead of re-hitting the API. The Refresh button remains the explicit
  // re-fetch path.
  const dataRef = useRef<DependencyAnalysis | null>(null)

  // renderGraph paints a fetched DAG into the current cytoscape instance.
  const renderGraph = (dep: DependencyAnalysis) => {
    const cy = cyRef.current
    if (!cy) return
    // Nodes: every rule in the topo order (so isolated rules appear too),
    // plus any rule referenced by an edge not in the order (defensive).
    const nodeIds = new Set<string>(dep.topoOrder ?? [])
    for (const d of dep.dependencies ?? []) {
      nodeIds.add(d.fromRuleId)
      nodeIds.add(d.toRuleId)
    }
    const inCycle = new Set<string>((dep.cycles ?? []).flat())

    const elements = [
      ...Array.from(nodeIds).map(id => ({
        data: {id, label: id},
        classes: inCycle.has(id) ? 'cycle-node' : '',
      })),
      ...(dep.dependencies ?? []).map((d, i) => ({
        data: {id: `e${i}`, source: d.fromRuleId, target: d.toRuleId, reason: shortReason(d.reason)},
      })),
    ]

    cy.batch(() => {
      cy.elements().remove()
      cy.add(elements)
    })
    cy.layout({
      name: 'dagre',
      rankDir: 'TB',
      nodeSep: 50,
      rankSep: 70,
      animate: true,
      animationDuration: 400,
    } as cytoscape.LayoutOptions).run()
    cy.fit(undefined, 40)
  }

  // load fetches the rule-dependency DAG and renders it into the cytoscape
  // instance. Declared before the instance-creation effect so that effect can
  // call it for the initial paint without tripping no-use-before-define.
  const load = async () => {
    const inst = cyRef.current
    setLoading(true)
    setError(null)
    try {
      const dep = await analysisApi.getDependencies()
      setData(dep)
      dataRef.current = dep
      if (cyRef.current && cyRef.current === inst) renderGraph(dep)
    } catch (err) {
      if (cyRef.current === inst) {
        setError(err instanceof Error ? err.message : String(err))
        logger.warn('rule dependency view: load failed', err)
      }
    } finally {
      if (cyRef.current === inst) setLoading(false)
    }
  }

  // Build the cytoscape instance. Recreated on theme change so the CSS-var-
  // derived colors track the active theme (same approach as the other graph
  // views); the initial fetch runs once the instance is ready.
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
            shape: 'round-rectangle',
            width: '180px',
            height: '44px',
            'background-color': cssVar('--surface-2'),
            'border-width': 1.5,
            'border-color': cssVar('--border-strong'),
            label: 'data(label)',
            color: cssVar('--text-primary'),
            'font-size': '11px',
            'font-family': 'var(--font-mono)',
            'text-valign': 'center',
            'text-halign': 'center',
            'text-wrap': 'ellipsis',
            padding: '6px',
          },
        },
        {
          selector: 'edge',
          style: {
            width: 1.5,
            'line-color': cssVar('--border-strong'),
            'target-arrow-color': cssVar('--border-strong'),
            'target-arrow-shape': 'triangle',
            'curve-style': 'bezier',
            label: 'data(reason)',
            'font-size': '9px',
            color: cssVar('--text-tertiary'),
            'text-background-color': cssVar('--surface-1'),
            'text-background-padding': '2px',
            'text-background-opacity': 0.9,
            'text-rotation': 'autorotate',
          },
        },
        {
          selector: 'node:selected',
          style: {'border-color': cssVar('--brand-500'), 'border-width': 3},
        },
        {
          selector: '.cycle-node',
          style: {'border-color': cssVar('--error'), 'border-width': 2.5},
        },
      ],
      minZoom: 0.3,
      maxZoom: 2.5,
    })

    instance.on('tap', 'node', (evt: cytoscape.EventObject) => {
      setSelected(evt.target.id())
    })
    // Clicking empty canvas deselects.
    instance.on('tap', (evt: cytoscape.EventObject) => {
      if (evt.target === instance) setSelected(null)
    })

    cyRef.current = instance
    // First mount fetches; theme rebuilds re-render the cached DAG without a
    // redundant HTTP round trip (the data doesn't change with theme).
    if (dataRef.current) {
      renderGraph(dataRef.current)
    } else {
      load()
    }
    return () => {
      instance.destroy()
      cyRef.current = null
    }
  }, [resolvedTheme])

  const selectedDeps =
    data && selected ? (data.dependencies ?? []).filter(d => d.fromRuleId === selected || d.toRuleId === selected) : []
  const hasCycles = !!(data?.cycles && data.cycles.length > 0)

  return (
    <div className="relative w-full h-full bg-surface-1">
      <div ref={containerRef} className="absolute inset-0" />

      {/* Header overlay */}
      <div className="absolute top-3 left-3 right-3 flex items-center justify-between pointer-events-none">
        <div className="flex items-center gap-2 pointer-events-auto">
          <button
            onClick={() => setMainPaneView('dashboard')}
            className="text-xs px-2 py-1 rounded-lg bg-surface-2/80 border border-border-subtle text-text-secondary hover:text-text-primary backdrop-blur transition-colors"
          >
            ← Back
          </button>
          <h2 className="text-sm font-semibold text-text-primary">Rule Dependency Graph</h2>
          {data && (
            <span className="text-2xs text-text-tertiary font-mono">
              {(data.dependencies ?? []).length} edges · {(data.topoOrder ?? []).length} rules
            </span>
          )}
        </div>
        <button
          onClick={load}
          title="Refresh"
          className="pointer-events-auto p-1.5 rounded-lg bg-surface-2/80 border border-border-subtle text-text-tertiary hover:text-text-primary backdrop-blur transition-colors"
        >
          <RefreshCw size={13} />
        </button>
      </div>

      {loading && (
        <div className="absolute inset-0 flex items-center justify-center bg-surface-1/60 backdrop-blur-sm">
          <Loader2 size={20} className="animate-spin text-brand-400" />
        </div>
      )}

      {error && (
        <div className="absolute top-16 left-1/2 -translate-x-1/2 flex items-center gap-2 px-3 py-2 rounded-lg bg-red-500/10 border border-red-500/30 text-xs text-red-400">
          <AlertTriangle size={13} /> {error}
        </div>
      )}

      {/* Cycle warning */}
      {hasCycles && !loading && (
        <div className="absolute top-16 left-3 right-3 flex items-start gap-2 px-3 py-2 rounded-lg bg-red-500/10 border border-red-500/30 text-xs text-red-400 pointer-events-none">
          <AlertTriangle size={13} className="mt-0.5 shrink-0" />
          <div>
            <span className="font-semibold">Circular dependency detected:</span>{' '}
            {(data!.cycles ?? []).map((c, i) => (
              <span key={i} className="font-mono">
                {c.join(' → ')}
                {i < data!.cycles!.length - 1 ? '; ' : ''}
              </span>
            ))}
          </div>
        </div>
      )}

      {/* Side panel: selected node's relationships */}
      {selected && data && (
        <div className="absolute bottom-3 left-3 w-72 max-w-[80%] p-3 rounded-lg bg-surface-2/90 border border-border-subtle backdrop-blur text-xs">
          <div className="flex items-center justify-between mb-2">
            <span className="font-mono font-semibold text-text-primary">{selected}</span>
            <button
              onClick={() => {
                setSelected(null)
                cyRef.current?.$(':selected').unselect()
              }}
              className="text-text-tertiary hover:text-text-primary"
            >
              ×
            </button>
          </div>
          {selectedDeps.length === 0 ? (
            <p className="text-text-tertiary">No dependency edges.</p>
          ) : (
            <ul className="space-y-1">
              {selectedDeps.map((d, i) => (
                <li key={i} className="text-text-secondary">
                  <span className="font-mono text-brand-400">{d.fromRuleId}</span>
                  <span className="text-text-tertiary"> → </span>
                  <span className="font-mono text-brand-400">{d.toRuleId}</span>
                  <div className="text-2xs text-text-tertiary">{d.reason}</div>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {/* Legend */}
      <div className="absolute bottom-3 right-3 flex flex-col gap-1 text-2xs text-text-tertiary bg-surface-2/80 border border-border-subtle rounded-lg px-2.5 py-2 backdrop-blur pointer-events-none">
        <div className="flex items-center gap-1.5">
          <span className="w-3 h-3 rounded-sm border-2 border-red-500" /> in a cycle
        </div>
        <div className="flex items-center gap-1.5">
          <span className="w-3 h-3 rounded-sm border border-border-strong" /> rule
        </div>
      </div>
    </div>
  )
}

// shortReason trims a dependency reason for the edge label (full text is in the
// side panel when a connected node is tapped).
function shortReason(r: string): string {
  if (!r) return ''
  return r.length > 24 ? r.slice(0, 23) + '…' : r
}
