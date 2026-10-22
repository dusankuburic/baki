import {useCallback, useEffect, useMemo, useRef, useState, type MouseEvent, type ReactNode} from 'react'
import {
  Background,
  BackgroundVariant,
  ReactFlow,
  ReactFlowProvider,
  useNodesInitialized,
  useReactFlow,
  type DefaultEdgeOptions,
  type Edge,
  type EdgeMouseHandler,
  type EdgeTypes,
  type FitViewOptions,
  type Node,
  type NodeMouseHandler,
  type NodeTypes,
} from '@xyflow/react'
import '@xyflow/react/dist/base.css'
import {useUIStore} from '@/stores/uiStore'
import {isLightTheme} from '@/lib/themeRegistry'
import {exportFlowPng} from './exportPng'

/** Above this many nodes React Flow mounts only what the viewport covers. */
const VIRTUALIZE_FROM = 200

/** Window events the main toolbar fires for whichever graph view is mounted. */
export const GRAPH_FIT_EVENT = 'graph:fit'
export const GRAPH_EXPORT_PNG_EVENT = 'graph:export-png'

export interface FlowCanvasProps {
  nodes: Node[]
  edges: Edge[]
  nodeTypes: NodeTypes
  edgeTypes?: EdgeTypes
  defaultEdgeOptions?: DefaultEdgeOptions
  className?: string
  minZoom?: number
  maxZoom?: number
  fitPadding?: NonNullable<FitViewOptions['padding']>
  /**
   * Refit the viewport when this changes — new data, another layout, a different subflow. The graph
   * stays hidden until its first fit has been applied, so an unfitted frame is never on screen.
   */
  fitSignal?: string
  /** React Flow's dot grid, for the views that draw one today. */
  dots?: boolean
  /** Mirror the zoom into `uiStore.graphZoom`, and follow the zoom buttons in the main toolbar. */
  syncZoom?: boolean
  /** When set, this view answers the toolbar's Export PNG with a file of this name. */
  exportFileName?: () => string
  /** Pan and zoom onto these nodes whenever `focusToken` changes (the map's search submit). */
  focusIds?: string[]
  focusToken?: number
  onNodeClick?: NodeMouseHandler
  onNodeDoubleClick?: NodeMouseHandler
  onEdgeClick?: EdgeMouseHandler
  onPaneClick?: (event: MouseEvent) => void
  /** Overlays that belong inside the flow: a minimap, a panel. */
  children?: ReactNode
}

/**
 * The one place React Flow is configured.
 *
 * Nodes are DOM, and pan/zoom is a single CSS transform on the viewport element, so the container's
 * size feeds nothing but `fitView` — which runs on mount and when `fitSignal` changes, never on a
 * resize. A pane-divider drag or a window resize therefore repaints nothing: the pane reveals or
 * hides DOM that is already on screen. That is the whole reason this replaced the canvas renderer,
 * so keep it that way: do not refit from a ResizeObserver.
 */
export default function FlowCanvas(props: FlowCanvasProps) {
  return (
    <ReactFlowProvider>
      <FlowCanvasInner {...props} />
    </ReactFlowProvider>
  )
}

function FlowCanvasInner({
  nodes,
  edges,
  nodeTypes,
  edgeTypes,
  defaultEdgeOptions,
  className,
  minZoom = 0.1,
  maxZoom = 2.5,
  fitPadding = '50px',
  fitSignal,
  dots = false,
  syncZoom = false,
  exportFileName,
  focusIds,
  focusToken,
  onNodeClick,
  onNodeDoubleClick,
  onEdgeClick,
  onPaneClick,
  children,
}: FlowCanvasProps) {
  const wrapperRef = useRef<HTMLDivElement>(null)
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  const initialized = useNodesInitialized()
  const {fitView, fitBounds, getNodes, getNodesBounds} = useReactFlow()
  const [ready, setReady] = useState(false)
  const fittedFor = useRef<string | null>(null)
  const focusedFor = useRef<number | undefined>(undefined)

  const fit = useCallback(() => {
    void fitView({padding: fitPadding})
  }, [fitView, fitPadding])

  /**
   * Every view here lays its nodes out itself and gives each one a fixed `width`/`height`, so the
   * sizes are known before anything is on screen. Passing them as `measured` too means React Flow
   * never has to wait for a DOM measurement to come back through `onNodesChange` (which these
   * read-only graphs do not have): the first render is already laid out and fittable, and the
   * nodes keep their size when a selection rebuilds the array.
   */
  const flowNodes = useMemo(
    () =>
      nodes.map(node =>
        node.measured || node.width === undefined || node.height === undefined
          ? node
          : {...node, measured: {width: node.width, height: node.height}},
      ),
    [nodes],
  )

  useEffect(() => {
    if (!initialized && nodes.length > 0) return
    const signal = fitSignal ?? ''
    if (nodes.length > 0 && fittedFor.current !== signal) {
      fittedFor.current = signal
      fit()
    }
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setReady(true)
  }, [initialized, nodes.length, fitSignal, fit])

  useEffect(() => {
    window.addEventListener(GRAPH_FIT_EVENT, fit)
    return () => window.removeEventListener(GRAPH_FIT_EVENT, fit)
  }, [fit])

  useEffect(() => {
    if (focusToken === undefined || focusedFor.current === focusToken) return
    focusedFor.current = focusToken
    const wanted = new Set(focusIds ?? [])
    if (wanted.size === 0) return
    const bounds = getNodesBounds(getNodes().filter(node => wanted.has(node.id)))
    if (bounds.width === 0 && bounds.height === 0) return
    // `fitBounds` takes padding as a share of the viewport, not pixels.
    void fitBounds(bounds, {duration: 400, padding: 0.2})
  }, [focusToken, focusIds, getNodes, getNodesBounds, fitBounds])

  useEffect(() => {
    if (!exportFileName) return
    const onExport = () => {
      const drawn = getNodes()
      if (drawn.length === 0) return
      void exportFlowPng(wrapperRef.current, getNodesBounds(drawn), exportFileName())
    }
    window.addEventListener(GRAPH_EXPORT_PNG_EVENT, onExport)
    return () => window.removeEventListener(GRAPH_EXPORT_PNG_EVENT, onExport)
  }, [exportFileName, getNodes, getNodesBounds])

  const onMove = useGraphZoomSync(syncZoom)

  return (
    <div
      ref={wrapperRef}
      className={`${className ?? ''} ${ready ? '' : 'opacity-0'}`.trim()}
      data-flow-ready={ready ? 'true' : 'false'}
    >
      <ReactFlow
        nodes={flowNodes}
        edges={edges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        defaultEdgeOptions={defaultEdgeOptions}
        colorMode={isLightTheme(resolvedTheme) ? 'light' : 'dark'}
        minZoom={minZoom}
        maxZoom={maxZoom}
        fitView
        fitViewOptions={{padding: fitPadding}}
        onlyRenderVisibleElements={nodes.length > VIRTUALIZE_FROM}
        nodesDraggable={false}
        nodesConnectable={false}
        edgesFocusable={false}
        zoomOnDoubleClick={false}
        deleteKeyCode={null}
        selectionKeyCode={null}
        multiSelectionKeyCode={null}
        onNodeClick={onNodeClick}
        onNodeDoubleClick={onNodeDoubleClick}
        onEdgeClick={onEdgeClick}
        onPaneClick={onPaneClick}
        onMove={onMove}
        proOptions={{hideAttribution: false}}
      >
        {dots && <Background variant={BackgroundVariant.Dots} gap={20} size={1} />}
        {children}
      </ReactFlow>
    </div>
  )
}

/**
 * Keep `uiStore.graphZoom` and the viewport in step: the store is what the main toolbar's zoom
 * buttons and percentage readout work with. A zoom the user made writes the store; a store value
 * that differs from the viewport (the toolbar pressed +, or another view left a zoom behind) is
 * applied to the viewport.
 */
function useGraphZoomSync(enabled: boolean): (() => void) | undefined {
  const graphZoom = useUIStore(s => s.graphZoom)
  const setGraphZoom = useUIStore(s => s.setGraphZoom)
  const {getZoom, zoomTo} = useReactFlow()

  useEffect(() => {
    if (!enabled) return
    if (Math.abs(getZoom() - graphZoom) > 0.01) void zoomTo(graphZoom, {duration: 0})
  }, [enabled, graphZoom, getZoom, zoomTo])

  const onMove = useCallback(() => {
    const zoom = getZoom()
    if (Math.abs(zoom - useUIStore.getState().graphZoom) > 0.001) setGraphZoom(zoom)
  }, [getZoom, setGraphZoom])

  return enabled ? onMove : undefined
}
