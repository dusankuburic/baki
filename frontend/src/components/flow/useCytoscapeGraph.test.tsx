import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, act, waitFor, fireEvent} from '@testing-library/react'
import {useCytoscapeGraph} from './useCytoscapeGraph'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'

// ---- cytoscape mock: records calls so the tests assert lifecycle behaviour
// (create/destroy, element swap, race guards) without a real canvas. ----
// Minimal cytoscape collection with the chainable methods the hook uses.
class MockCollection {
  items: Array<MockElement>
  constructor(items: MockElement[] = []) {
    this.items = items
  }
  get length() {
    return this.items.length
  }
  filter(pred: (el: MockElement) => boolean) {
    return new MockCollection(this.items.filter(pred))
  }
  not(other: MockCollection) {
    return new MockCollection(this.items.filter(el => !other.items.includes(el)))
  }
  addClass(_cls: string) {
    return this
  }
  removeClass(_cls: string) {
    return this
  }
  select() {
    return this
  }
  unselect() {
    return this
  }
  remove() {
    return this
  }
}

class MockElement {
  isNode: boolean
  dataStore: Record<string, unknown>
  constructor(isNode: boolean, data: Record<string, unknown>) {
    this.isNode = isNode
    this.dataStore = data
  }
  data(key: string) {
    return this.dataStore[key]
  }
  remove() {}
}

const mockCy = {
  _listeners: {} as Record<string, Array<(e?: unknown) => void>>,
  on(event: string, cb: (e?: unknown) => void) {
    ;(this._listeners[event] ??= []).push(cb)
  },
  batch(fn: () => void) {
    fn()
  },
  elements() {
    return new MockCollection(this._elements)
  },
  nodes() {
    return new MockCollection(this._elements.filter(e => e.isNode))
  },
  add(els: unknown) {
    this._added = els
    for (const el of els as Array<{data: Record<string, unknown>}>) {
      // Dedupe by id like real cytoscape: the hook's two mount-time loads
      // (instance effect + data effect) both add the same elements.
      if (el.data.id && this._elements.some(e => e.dataStore.id === el.data.id)) continue
      this._elements.push(new MockElement(!!el.data.id, el.data))
    }
  },
  layout(_opts: unknown) {
    return {run: () => {}}
  },
  fit() {},
  zoom() {
    return 1
  },
  center() {},
  destroy() {
    this._destroyed = true
  },
  animate() {},
  _elements: [] as MockElement[],
  _added: null as unknown,
  _destroyed: false,
}

const cytoscapeFactory = vi.fn((..._args: unknown[]) => mockCy)
vi.mock('cytoscape', () => ({
  default: Object.assign(
    vi.fn((...args: unknown[]) => cytoscapeFactory(...(args as []))),
    {use: vi.fn()},
  ),
}))
vi.mock('cytoscape-dagre', () => ({default: {}}))

const getExecutionGraph = vi.fn()
vi.mock('@/api', () => ({
  analysisApi: {getExecutionGraph: (...a: unknown[]) => getExecutionGraph(...a)},
}))

const graph = (nodes: Array<{id: string; label: string}>, edges: Array<{source: string; target: string}> = []) => ({
  nodes: nodes.map((n, i) => ({id: n.id, label: n.label, blockCount: i, errorCount: 0, warnCount: 0})),
  edges,
})

// The hook's mount effect bails without a real container element, so mount it
// inside a harness div like ExecutionGraphView does.
// The harness renders the hook's observable state into the DOM exactly like
// ExecutionGraphView does (loading/error overlays) — DOM assertions then go
// through RTL's normal act-wrapped update flow.
function mount(subflowId?: string) {
  const Harness = () => {
    const api = useCytoscapeGraph(subflowId)
    return (
      <div>
        <div ref={api.containerRef} data-testid="cy-container" />
        <span data-testid="cy-loading">{String(api.loading)}</span>
        {api.error && <span data-testid="cy-error">{api.error}</span>}
        <span data-testid="cy-match-count">{api.matchCount ?? 'null'}</span>
        <input
          data-testid="cy-search"
          value={api.searchQuery}
          onChange={e => api.setSearchQuery(e.target.value)}
        />
      </div>
    )
  }
  const utils = render(<Harness />)
  return {
    ...utils,
    loadingEl: () => utils.getByTestId('cy-loading'),
    errorEl: () => utils.queryByTestId('cy-error'),
    matchCountEl: () => utils.getByTestId('cy-match-count'),
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  mockCy._listeners = {}
  mockCy._elements = []
  mockCy._added = null
  mockCy._destroyed = false
  useFlowStore.setState({document: {id: 'flow-1', name: 'F'} as never})
  useAnalysisStore.setState({reports: new Map()})
  useUIStore.setState({graphZoom: 1})
  // The hook needs a real DOM node for containerRef; renderHook mounts a div.
})

afterEach(() => {
  // drain pending state updates
})

describe('useCytoscapeGraph', () => {
  it('creates one cytoscape instance on mount and destroys it on unmount', async () => {
    getExecutionGraph.mockResolvedValue(graph([{id: 'sf1', label: 'Main'}]))
    const {unmount} = mount()

    await waitFor(() => expect(cytoscapeFactory).toHaveBeenCalledTimes(1))
    expect(mockCy._destroyed).toBe(false)

    unmount()
    expect(mockCy._destroyed).toBe(true)
  })

  it('loads nodes and edges into the instance', async () => {
    getExecutionGraph.mockResolvedValue(
      graph(
        [
          {id: 'sf1', label: 'Main'},
          {id: 'sf2', label: 'Helper'},
        ],
        [{source: 'sf1', target: 'sf2'}],
      ),
    )
    mount()

    await waitFor(() => expect(mockCy._added).not.toBeNull())
    const added = mockCy._added as Array<{data: Record<string, unknown>}>
    expect(added.filter(e => e.data.id)).toHaveLength(2)
    expect(added.find(e => e.data.id === 'sf1')?.data.label).toBe('Main')
    expect(added.find(e => e.data.source === 'sf1')).toBeDefined()
  })

  it('filters to the subflow neighborhood when subflowId is given', async () => {
    getExecutionGraph.mockResolvedValue(
      graph(
        [
          {id: 'sf1', label: 'Main'},
          {id: 'sf2', label: 'Helper'},
          {id: 'sf3', label: 'Unrelated'},
        ],
        [
          {source: 'sf1', target: 'sf2'},
          {source: 'sf1', target: 'sf3'},
        ],
      ),
    )
    mount('sf1')

    await waitFor(() => expect(mockCy._added).not.toBeNull())
    const added = mockCy._added as Array<{data: Record<string, unknown>}>
    const ids = added.filter(e => e.data.id).map(e => e.data.id)
    // sf1 + its neighbors; sf3 is a neighbor of sf1 too (edge sf1→sf3).
    expect(ids).toContain('sf1')
    expect(ids).toContain('sf2')
    expect(ids).toContain('sf3')
    // The center marker is set on the focused subflow.
    expect(added.find(e => e.data.id === 'sf1')?.data.isCenter).toBe('true')
    expect(added.find(e => e.data.id === 'sf2')?.data.isCenter).toBe('false')
  })

  it('surfaces fetch errors and clears the loading flag', async () => {
    getExecutionGraph.mockRejectedValue(new Error('backend down'))
    const t = mount()

    expect(await t.findByTestId('cy-error')).toHaveTextContent('backend down')
    await waitFor(() => expect(t.loadingEl()).toHaveTextContent('false'))
  })

  it('ignores a stale response that resolves after the instance was replaced', async () => {
    // First mount: slow response. Unmount before it resolves → destroy + null ref.
    let resolveSlow!: (v: unknown) => void
    getExecutionGraph.mockReturnValue(new Promise(r => (resolveSlow = r)))
    const first = mount()
    await waitFor(() => expect(cytoscapeFactory).toHaveBeenCalledTimes(1))
    first.unmount()
    expect(mockCy._destroyed).toBe(true)

    // Resolve the stale request: must be dropped (no add on the dead instance).
    resolveSlow(graph([{id: 'sf1', label: 'Main'}]))
    await act(async () => {
      await Promise.resolve()
    })
    expect(mockCy._added).toBeNull()
  })

  it('clears search highlight classes when the query empties', async () => {
    getExecutionGraph.mockResolvedValue(graph([{id: 'sf1', label: 'Billing'}]))
    const t = mount()
    await waitFor(() => expect(t.loadingEl()).toHaveTextContent('false'))

    // Typing 'bill' matches the 'Billing' node (case-insensitive label match).
    fireEvent.change(t.getByTestId('cy-search'), {target: {value: 'bill'}})
    await waitFor(() => expect(t.matchCountEl()).toHaveTextContent('1'))

    // Clearing the query clears the match count.
    fireEvent.change(t.getByTestId('cy-search'), {target: {value: ''}})
    await waitFor(() => expect(t.matchCountEl()).toHaveTextContent('null'))
  })
})
