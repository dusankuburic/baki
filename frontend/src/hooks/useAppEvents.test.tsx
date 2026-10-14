import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render} from '@testing-library/react'
import {useAppEvents} from './useAppEvents'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useGovernanceStore} from '@/stores/governanceStore'
import {useSettingsStore} from '@/stores/settingsStore'

let capturedCallback: ((ev: {name: string; data?: unknown}) => void) | null = null
let capturedGap: (() => void) | null = null

vi.mock('@/api/client', () => ({
  subscribeToEvents: vi.fn(async (cb: (ev: {name: string; data?: unknown}) => void) => {
    capturedCallback = cb
    return () => {
      capturedCallback = null
    }
  }),
  subscribeEventGap: vi.fn((cb: () => void) => {
    capturedGap = cb
    return () => {
      capturedGap = null
    }
  }),
}))

const mockGetContent = vi.fn()
const mockLoadFlowFromPath = vi.fn()
const mockLoadFlowFolder = vi.fn()

vi.mock('@/api', () => ({
  flowApi: {
    loadFlowFromPath: (...a: unknown[]) => mockLoadFlowFromPath(...a),
    loadFlowFolder: (...a: unknown[]) => mockLoadFlowFolder(...a),
  },
}))

vi.mock('@/api/library', () => ({
  libraryApi: {
    getContent: (...a: unknown[]) => mockGetContent(...a),
  },
}))

const mockUnreadCount = vi.fn()
vi.mock('@/api/governance', () => ({
  governanceApi: {
    unreadCount: (...a: unknown[]) => mockUnreadCount(...a),
    list: vi.fn().mockResolvedValue([]),
    markAllRead: vi.fn().mockResolvedValue({status: 'ok'}),
    dismiss: vi.fn().mockResolvedValue({status: 'ok'}),
    clear: vi.fn().mockResolvedValue({status: 'ok'}),
  },
}))

function TestComponent({openDocument}: {openDocument: (doc: unknown) => void}) {
  useAppEvents({openDocument})
  return null
}

const fullDoc = (id: string, name: string) => ({id, name, subflows: [{id: 'sf1', name: 'Main', blocks: []}]})

beforeEach(() => {
  capturedCallback = null
  capturedGap = null
  vi.clearAllMocks()
  mockUnreadCount.mockResolvedValue({count: 0})
  useFlowStore.setState({document: null})
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

  it('refetches the document from the library and calls openDocument (cloud summary)', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    const docData = fullDoc('test-1', 'Test')
    mockGetContent.mockResolvedValue(docData)
    capturedCallback!({name: 'flow:loaded', data: {id: 'test-1', name: 'Test'}})

    await vi.waitFor(() => expect(openDocument).toHaveBeenCalledWith(docData))
    expect(mockGetContent).toHaveBeenCalledWith('test-1')
    expect(mockLoadFlowFromPath).not.toHaveBeenCalled()
  })

  it('refetches a local single-file flow via load-path', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    const docData = fullDoc('local-1', 'Local')
    mockLoadFlowFromPath.mockResolvedValue(docData)
    capturedCallback!({
      name: 'flow:loaded',
      data: {id: 'local-1', name: 'Local', filePath: 'C:/flows/Main.txt'},
    })

    await vi.waitFor(() => expect(openDocument).toHaveBeenCalledWith(docData))
    expect(mockLoadFlowFromPath).toHaveBeenCalledWith('C:/flows/Main.txt')
    expect(mockGetContent).not.toHaveBeenCalled()
  })

  it('refetches a local folder flow via load-folder', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    const docData = fullDoc('folder-1', 'Folder')
    mockLoadFlowFolder.mockResolvedValue(docData)
    capturedCallback!({
      name: 'flow:loaded',
      data: {id: 'folder-1', name: 'Folder', filePath: 'C:/flows', isFolder: true},
    })

    await vi.waitFor(() => expect(openDocument).toHaveBeenCalledWith(docData))
    expect(mockLoadFlowFolder).toHaveBeenCalledWith('C:/flows')
    expect(mockLoadFlowFromPath).not.toHaveBeenCalled()
  })

  it('drops the event when the refetch fails instead of crashing', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    mockGetContent.mockRejectedValue(new Error('flow deleted'))
    capturedCallback!({name: 'flow:loaded', data: {id: 'gone-1', name: 'Gone'}})

    await new Promise(r => setTimeout(r, 10))
    expect(openDocument).not.toHaveBeenCalled()
  })

  it('drops a malformed flow:loaded payload instead of hijacking the editor', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({name: 'flow:loaded', data: {html: '<html>err</html>'}})
    capturedCallback!({name: 'flow:loaded', data: {id: '', name: 'x'}})
    await new Promise(r => setTimeout(r, 10))
    expect(openDocument).not.toHaveBeenCalled()
    expect(mockGetContent).not.toHaveBeenCalled()
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

  it('does not hijack the editor with a flow:loaded for a different flow', async () => {
    const openDocument = vi.fn()
    useFlowStore.setState({document: {id: 'current-flow', name: 'Current'} as never})
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({
      name: 'flow:loaded',
      data: {id: 'stale-flow', name: 'Stale'},
    })
    await new Promise(r => setTimeout(r, 10))
    expect(openDocument).not.toHaveBeenCalled()
    expect(mockGetContent).not.toHaveBeenCalled()
  })

  it('ignores a flow:loaded echo for the flow already open (no refetch loop)', async () => {
    const openDocument = vi.fn()
    useFlowStore.setState({document: {id: 'current-flow', name: 'Current'} as never})
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({
      name: 'flow:loaded',
      data: {id: 'current-flow', name: 'Current refreshed'},
    })
    await new Promise(r => setTimeout(r, 10))
    expect(openDocument).not.toHaveBeenCalled()
    expect(mockGetContent).not.toHaveBeenCalled()
    expect(mockLoadFlowFromPath).not.toHaveBeenCalled()
  })

  it('does not hijack the editor when a different flow is opened during the refetch', async () => {
    const openDocument = vi.fn()
    useFlowStore.setState({document: null})
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    let resolveFetch!: (doc: unknown) => void
    mockGetContent.mockReturnValue(
      new Promise(r => {
        resolveFetch = r
      }),
    )
    capturedCallback!({name: 'flow:loaded', data: {id: 'flow-a', name: 'A'}})
    useFlowStore.setState({document: {id: 'flow-b', name: 'B'} as never})
    resolveFetch(fullDoc('flow-a', 'A'))

    await new Promise(r => setTimeout(r, 10))
    expect(openDocument).not.toHaveBeenCalled()
  })

  it('refreshes the governance badge on a governance:alert event', async () => {
    mockUnreadCount.mockResolvedValue({count: 3})
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)
    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({name: 'governance:alert', data: {flowId: 'f1', type: 'drift'}})
    await new Promise(r => setTimeout(r, 10))

    expect(useGovernanceStore.getState().unreadCount).toBe(3)
  })

  it('refetches settings once after a burst of settings:changed events', async () => {
    const loadFromBackend = vi.fn().mockResolvedValue(undefined)
    const original = useSettingsStore.getState().loadFromBackend
    useSettingsStore.setState({loadFromBackend})
    try {
      render(<TestComponent openDocument={vi.fn()} />)
      await new Promise(r => setTimeout(r, 10))

      vi.useFakeTimers()
      capturedCallback!({name: 'settings:changed', data: {}})
      capturedCallback!({name: 'settings:changed', data: {}})
      vi.advanceTimersByTime(1000)
      expect(loadFromBackend).not.toHaveBeenCalled()
      capturedCallback!({name: 'settings:changed', data: {}})
      vi.advanceTimersByTime(1500)
      expect(loadFromBackend).toHaveBeenCalledTimes(1)
    } finally {
      vi.useRealTimers()
      useSettingsStore.setState({loadFromBackend: original})
    }
  })
})

describe('useAppEvents replay gap', () => {
  it('refetches what the missed events would have updated, and stops listening on unmount', async () => {
    mockUnreadCount.mockResolvedValue({count: 5})
    const loadFromBackend = vi.fn().mockResolvedValue(undefined)
    const original = useSettingsStore.getState().loadFromBackend
    useSettingsStore.setState({loadFromBackend})
    try {
      const {unmount} = render(<TestComponent openDocument={vi.fn()} />)
      await new Promise(r => setTimeout(r, 10))
      expect(capturedGap).not.toBeNull()

      vi.useFakeTimers()
      capturedGap!()
      capturedGap!() // a burst is one refetch
      vi.advanceTimersByTime(1600)
      expect(loadFromBackend).toHaveBeenCalledTimes(1)
      vi.useRealTimers()
      await new Promise(r => setTimeout(r, 10))
      expect(useGovernanceStore.getState().unreadCount).toBe(5)

      unmount()
      expect(capturedGap).toBeNull()
    } finally {
      vi.useRealTimers()
      useSettingsStore.setState({loadFromBackend: original})
    }
  })
})
