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

    const docData = {id: 'test-1', name: 'Test'}
    capturedCallback!({name: 'flow:loaded', data: docData})

    expect(openDocument).toHaveBeenCalledWith(docData)
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

    // A flow:loaded for a DIFFERENT flow arrives — must be ignored.
    capturedCallback!({name: 'flow:loaded', data: {id: 'stale-flow', name: 'Stale'}})
    expect(openDocument).not.toHaveBeenCalled()

    // A flow:loaded for the CURRENT flow (e.g. a collaborator re-parse) is still honored.
    capturedCallback!({name: 'flow:loaded', data: {id: 'current-flow', name: 'Current refreshed'}})
    expect(openDocument).toHaveBeenCalledTimes(1)
    expect(openDocument).toHaveBeenCalledWith(expect.objectContaining({id: 'current-flow'}))
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
