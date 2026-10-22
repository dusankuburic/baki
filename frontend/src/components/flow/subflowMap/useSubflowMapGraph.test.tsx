import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, act, waitFor, fireEvent} from '@testing-library/react'
import {useSubflowMapGraph} from './useSubflowMapGraph'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'

const getExecutionGraph = vi.fn()
vi.mock('@/api', () => ({
  analysisApi: {getExecutionGraph: (...a: unknown[]) => getExecutionGraph(...a)},
}))

const graph = (nodes: Array<{id: string; label: string}>, edges: Array<{source: string; target: string}> = []) => ({
  nodes: nodes.map((n, i) => ({id: n.id, label: n.label, type: 'subflow', blockCount: i, errorCount: 0, warnCount: 0})),
  edges,
})

type Api = ReturnType<typeof useSubflowMapGraph>

function mount(subflowId?: string) {
  const seen: {api: Api | null} = {api: null}
  const Harness = () => {
    const api = useSubflowMapGraph(subflowId)
    seen.api = api
    return (
      <div>
        <span data-testid="loading">{String(api.loading)}</span>
        {api.error && <span data-testid="error">{api.error}</span>}
        <span data-testid="matches">{api.matchCount ?? 'null'}</span>
        <input data-testid="search" value={api.searchQuery} onChange={e => api.setSearchQuery(e.target.value)} />
      </div>
    )
  }
  const utils = render(<Harness />)
  return {...utils, api: () => seen.api!}
}

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState({document: {id: 'flow-1', name: 'F'} as never})
  useAnalysisStore.setState({reports: new Map()})
  useUIStore.setState({
    graphZoom: 1,
    resolvedTheme: 'dark',
    subflowMapLayout: 'hierarchy',
    subflowMapDirection: 'LR',
    subflowMapRouting: 'corridor',
  })
})

describe('useSubflowMapGraph', () => {
  it('turns the execution graph into positioned React Flow nodes and edges', async () => {
    getExecutionGraph.mockResolvedValue(
      graph([{id: 'sf1', label: 'Main'}, {id: 'sf2', label: 'Helper'}], [{source: 'sf1', target: 'sf2'}]),
    )
    const t = mount()

    await waitFor(() => expect(t.getByTestId('loading')).toHaveTextContent('false'))
    const {nodes, edges} = t.api()

    expect(nodes.map(n => n.id)).toEqual(['sf1', 'sf2'])
    expect(nodes[0]!.data.subflow.label).toBe('Main')
    expect(nodes[0]!.width).toBeGreaterThan(0)
    // Laid out left to right, so the second card sits clear of the first.
    expect(nodes[1]!.position.x).toBeGreaterThan(nodes[0]!.position.x)
    expect(edges).toHaveLength(1)
    expect(edges[0]).toMatchObject({id: 'sf1->sf2', source: 'sf1', target: 'sf2'})
  })

  it('keeps only the subflow neighbourhood and marks the anchor card', async () => {
    getExecutionGraph.mockResolvedValue(
      graph(
        [{id: 'sf1', label: 'Main'}, {id: 'sf2', label: 'Helper'}, {id: 'sf3', label: 'Unrelated'}],
        [
          {source: 'sf1', target: 'sf2'},
          {source: 'sf2', target: 'sf3'},
        ],
      ),
    )
    const t = mount('sf1')

    await waitFor(() => expect(t.getByTestId('loading')).toHaveTextContent('false'))
    const {nodes} = t.api()

    expect(nodes.map(n => n.id)).toEqual(['sf1', 'sf2'])
    expect(nodes.find(n => n.id === 'sf1')!.data.anchor).toBe(true)
    expect(nodes.find(n => n.id === 'sf1')!.className).toContain('flow-node--anchor')
    expect(nodes.find(n => n.id === 'sf2')!.data.anchor).toBe(false)
  })

  it('surfaces fetch errors and clears the loading flag', async () => {
    getExecutionGraph.mockRejectedValue(new Error('backend down'))
    const t = mount()

    expect(await t.findByTestId('error')).toHaveTextContent('backend down')
    await waitFor(() => expect(t.getByTestId('loading')).toHaveTextContent('false'))
  })

  it('ignores a response that arrives after the view is gone', async () => {
    let resolveSlow!: (value: unknown) => void
    getExecutionGraph.mockReturnValue(new Promise(resolve => (resolveSlow = resolve)))
    const t = mount()
    t.unmount()

    resolveSlow(graph([{id: 'sf1', label: 'Main'}]))
    await act(async () => {
      await Promise.resolve()
    })
    // Nothing to assert on screen; the test fails on a React "state update after unmount" warning.
    expect(getExecutionGraph).toHaveBeenCalledTimes(1)
  })

  it('dims everything but the search matches, and clears when the query empties', async () => {
    getExecutionGraph.mockResolvedValue(graph([{id: 'sf1', label: 'Billing'}, {id: 'sf2', label: 'Payroll'}]))
    const t = mount()
    await waitFor(() => expect(t.getByTestId('loading')).toHaveTextContent('false'))

    fireEvent.change(t.getByTestId('search'), {target: {value: 'bill'}})
    await waitFor(() => expect(t.getByTestId('matches')).toHaveTextContent('1'))

    let nodes = t.api().nodes
    expect(nodes.find(n => n.id === 'sf1')!.className).toContain('flow-node--search-match')
    expect(nodes.find(n => n.id === 'sf2')!.className).toContain('flow-node--dimmed')

    fireEvent.change(t.getByTestId('search'), {target: {value: ''}})
    await waitFor(() => expect(t.getByTestId('matches')).toHaveTextContent('null'))
    nodes = t.api().nodes
    expect(nodes.every(n => !n.className?.includes('dimmed'))).toBe(true)
  })

  it('focuses a card with its neighbourhood and clears on escape', async () => {
    getExecutionGraph.mockResolvedValue(
      graph(
        [{id: 'sf1', label: 'Main'}, {id: 'sf2', label: 'Helper'}, {id: 'sf3', label: 'Lonely'}],
        [{source: 'sf1', target: 'sf2'}],
      ),
    )
    const t = mount()
    await waitFor(() => expect(t.getByTestId('loading')).toHaveTextContent('false'))

    act(() => t.api().onNodeClick({} as never, {id: 'sf1'} as never))

    const byId = (id: string) => t.api().nodes.find(n => n.id === id)!
    expect(byId('sf1').className).toContain('flow-node--focused')
    expect(byId('sf2').className).toContain('flow-node--connected')
    expect(byId('sf3').className).toContain('flow-node--dimmed')
    expect(t.api().edges[0]!.className).toBeUndefined()
    expect(t.api().focusedNode?.id).toBe('sf1')

    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape'}))
    })
    expect(t.api().focusedNode).toBeNull()
    expect(byId('sf3').className).not.toContain('dimmed')
  })

  it('re-lays out on a layout change without fetching again, and asks for a refit', async () => {
    getExecutionGraph.mockResolvedValue(
      graph([{id: 'sf1', label: 'Main'}, {id: 'sf2', label: 'Helper'}], [{source: 'sf1', target: 'sf2'}]),
    )
    const t = mount()
    await waitFor(() => expect(t.getByTestId('loading')).toHaveTextContent('false'))

    const before = t.api().nodes.map(n => ({...n.position}))
    const fitBefore = t.api().fitSignal

    act(() => useUIStore.setState({subflowMapLayout: 'radial'}))

    expect(getExecutionGraph).toHaveBeenCalledTimes(1)
    expect(t.api().fitSignal).not.toBe(fitBefore)
    expect(t.api().nodes.map(n => n.position)).not.toEqual(before)
    // Radial routes straight between card borders.
    expect(t.api().edges[0]!.type).toBe('floating')
  })

  it('bumps the fit signal when the fit button is pressed', async () => {
    getExecutionGraph.mockResolvedValue(graph([{id: 'sf1', label: 'Main'}]))
    const t = mount()
    await waitFor(() => expect(t.getByTestId('loading')).toHaveTextContent('false'))

    const before = t.api().fitSignal
    act(() => t.api().fitToScreen())
    expect(t.api().fitSignal).not.toBe(before)
  })
})
