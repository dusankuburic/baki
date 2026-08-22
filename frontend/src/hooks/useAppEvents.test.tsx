import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render} from '@testing-library/react'
import {useAppEvents} from './useAppEvents'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useGovernanceStore} from '@/stores/governanceStore'

let capturedCallback: ((ev: {name: string; data?: unknown}) => void) | null = null

vi.mock('@/api/client', () => ({
  subscribeToEvents: vi.fn(async (cb: (ev: {name: string; data?: unknown}) => void) => {
    capturedCallback = cb
    return () => {
      capturedCallback = null
    }
  }),
}))

const mockUnreadCount = vi.fn()
vi.mock('@/api/governance', () => ({
  governanceApi: {
    unreadCount: (...a: unknown[]) => mockUnreadCount(...a),
    list: vi.fn().mockResolvedValue([]),
    markRead: vi.fn().mockResolvedValue({status: 'ok'}),
    markAllRead: vi.fn().mockResolvedValue({status: 'ok'}),
    dismiss: vi.fn().mockResolvedValue({status: 'ok'}),
    clear: vi.fn().mockResolvedValue({status: 'ok'}),
  },
}))

function TestComponent({openDocument}: {openDocument: (doc: unknown) => void}) {
  useAppEvents({openDocument})
  return null
}

beforeEach(() => {
  capturedCallback = null
  vi.clearAllMocks()
  mockUnreadCount.mockResolvedValue({count: 0})
  useGovernanceStore.setState({alerts: [], unreadCount: 0, panelOpen: false})
})

describe('useAppEvents', () => {
  it('forwards analysis:progress events to the analysis store', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    expect(capturedCallback).not.toBeNull()
    capturedCallback!({
      name: 'analysis:progress',
      data: {current: 5, total: 10, ruleName: 'dead-code'},
    })

    const progress = useAnalysisStore.getState().progress
    expect(progress.current).toBe(5)
    expect(progress.total).toBe(10)
    expect(progress.ruleName).toBe('dead-code')
  })

  it('forwards flow:parse-progress to the flow store', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({
      name: 'flow:parse-progress',
      data: {percent: 42},
    })

    expect(useFlowStore.getState().isParsing).toBe(true)
    expect(useFlowStore.getState().parseProgress).toBe(42)
  })

  it('calls openDocument when flow:loaded event arrives', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    const docData = {id: 'test-1', name: 'Test', subflows: [{id: 'sf1', name: 'Main', blocks: []}]}
    capturedCallback!({name: 'flow:loaded', data: docData})
    // Envelope validation awaits the (memoized, zod-importing) schema factory.
    await vi.waitFor(() => expect(openDocument).toHaveBeenCalledWith(docData))
  })

  it('drops a malformed flow:loaded payload instead of hijacking the editor', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    // Not a flow document (no subflows / no id) — e.g. a proxy error page or
    // a different event's data shape. Must be rejected at the boundary.
    capturedCallback!({name: 'flow:loaded', data: {html: '<html>err</html>'}})
    capturedCallback!({name: 'flow:loaded', data: {id: '', name: 'x', subflows: []}})
    // Give the async validation a chance to (wrongly) run before asserting it
    // never reached openDocument.
    await new Promise(r => setTimeout(r, 10))
    expect(openDocument).not.toHaveBeenCalled()
  })

  it('forwards flow:load-error to the flow store', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({
      name: 'flow:load-error',
      data: {error: 'file not readable'},
    })

    expect(useFlowStore.getState().parseError).toBe('file not readable')
  })

  it('ignores unknown event names', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({name: 'unknown:event', data: {foo: 'bar'}})

    expect(openDocument).not.toHaveBeenCalled()
  })

  // Regression: a flow:loaded event for a flow OTHER than the one currently open
  // must not hijack the editor. Such an event is stale/unsolicited (another tab,
  // an admin action, or a late event for a flow the user navigated away from).
  // The initial load (no document open) and a refresh of the current flow are
  // still honored.
  it('does not hijack the editor with a flow:loaded for a different flow', async () => {
    const openDocument = vi.fn()
    // Simulate a document already open in the editor.
    useFlowStore.setState({document: {id: 'current-flow', name: 'Current'} as never})
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    // A flow:loaded for a DIFFERENT flow arrives — must be ignored (the guard
    // runs synchronously before the async envelope validation).
    capturedCallback!({
      name: 'flow:loaded',
      data: {id: 'stale-flow', name: 'Stale', subflows: [{id: 'sf1', name: 'Main', blocks: []}]},
    })
    await new Promise(r => setTimeout(r, 10))
    expect(openDocument).not.toHaveBeenCalled()

    // A flow:loaded for the CURRENT flow (e.g. a collaborator re-parse) is still honored.
    capturedCallback!({
      name: 'flow:loaded',
      data: {id: 'current-flow', name: 'Current refreshed', subflows: [{id: 'sf1', name: 'Main', blocks: []}]},
    })
    await vi.waitFor(() => expect(openDocument).toHaveBeenCalledTimes(1))
    expect(openDocument).toHaveBeenCalledWith(expect.objectContaining({id: 'current-flow'}))
  })

  // Regression (async-validation race): a flow:loaded that passed the stale-
  // flow guard at arrival must not hijack the editor if the user opens a
  // DIFFERENT flow before the deferred validation completes — widest on the
  // first event while the zod chunk is still loading. The guard is re-checked
  // inside the continuation.
  it('does not hijack the editor when a different flow is opened during async validation', async () => {
    const openDocument = vi.fn()
    // No document open yet, so the arrival-time guard passes for any flow.
    useFlowStore.setState({document: null})
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    // Event for flow A arrives and passes the guard…
    capturedCallback!({
      name: 'flow:loaded',
      data: {id: 'flow-a', name: 'A', subflows: [{id: 'sf1', name: 'Main', blocks: []}]},
    })
    // …but before the async validation runs, the user opens flow B directly.
    useFlowStore.setState({document: {id: 'flow-b', name: 'B'} as never})

    await new Promise(r => setTimeout(r, 10))
    expect(openDocument).not.toHaveBeenCalled()
  })

  it('refreshes the governance badge on a governance:alert event', async () => {
    mockUnreadCount.mockResolvedValue({count: 3})
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)
    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({name: 'governance:alert', data: {flowId: 'f1', type: 'drift'}})
    // refreshUnread is async; let it settle.
    await new Promise(r => setTimeout(r, 10))

    expect(useGovernanceStore.getState().unreadCount).toBe(3)
  })
})
