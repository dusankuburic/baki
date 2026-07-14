import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, waitFor} from '@testing-library/react'
import {useFileDrop} from './useFileDrop'
import {flowApi} from '@/api'
import {getPlatformCapabilities} from '@/platform/guards'
import type {PlatformCapabilities} from '@/platform/guards'

function caps(fileSystem: boolean): PlatformCapabilities {
  return {
    fileSystem,
    nativeDialogs: fileSystem,
    clipboard: true,
    notifications: true,
    systemTray: fileSystem,
    nativeWindow: fileSystem,
  }
}

type DragDropEvent = {payload: {type: 'enter' | 'over' | 'leave' | 'drop'; paths?: string[]}}
type DragDropHandler = (event: DragDropEvent) => void

let capturedHandler: DragDropHandler | null = null
const unlistenMock = vi.fn()

vi.mock('@tauri-apps/api/webview', () => ({
  getCurrentWebview: () => ({
    onDragDropEvent: (handler: DragDropHandler) => {
      capturedHandler = handler
      return Promise.resolve(unlistenMock)
    },
  }),
}))

vi.mock('@/platform/guards', () => ({
  getPlatformCapabilities: vi.fn(),
}))

vi.mock('@/api', () => ({
  flowApi: {
    loadFlowFromPath: vi.fn(),
    uploadFlow: vi.fn(),
  },
}))

vi.mock('@/components/shared', () => ({
  useToast: () => ({success: vi.fn(), error: vi.fn(), warning: vi.fn()}),
}))

beforeEach(() => {
  vi.clearAllMocks()
  capturedHandler = null
})

describe('useFileDrop — Tauri native drag-drop', () => {
  it('subscribes to the native webview drag-drop stream only in Tauri', async () => {
    vi.mocked(getPlatformCapabilities).mockReturnValue(caps(false))
    renderHook(() => useFileDrop(vi.fn()))
    await waitFor(() => expect(capturedHandler).toBeNull())
  })

  it('opens the dropped file via the native event and clears dragOver', async () => {
    vi.mocked(getPlatformCapabilities).mockReturnValue(caps(true))
    const doc = {id: 'doc1'} as never
    vi.mocked(flowApi.loadFlowFromPath).mockResolvedValue(doc)
    const openDocument = vi.fn()

    const {result} = renderHook(() => useFileDrop(openDocument))
    await waitFor(() => expect(capturedHandler).not.toBeNull())

    capturedHandler!({payload: {type: 'enter', paths: ['/tmp/Main.txt']}})
    await waitFor(() => expect(result.current.dragOver).toBe(true))

    capturedHandler!({payload: {type: 'drop', paths: ['/tmp/Main.txt']}})
    await waitFor(() => expect(openDocument).toHaveBeenCalledWith(doc))
    expect(result.current.dragOver).toBe(false)
    expect(flowApi.loadFlowFromPath).toHaveBeenCalledWith('/tmp/Main.txt')
  })

  it('clears dragOver on leave without opening anything', async () => {
    vi.mocked(getPlatformCapabilities).mockReturnValue(caps(true))
    const openDocument = vi.fn()
    const {result} = renderHook(() => useFileDrop(openDocument))
    await waitFor(() => expect(capturedHandler).not.toBeNull())

    capturedHandler!({payload: {type: 'over'}})
    await waitFor(() => expect(result.current.dragOver).toBe(true))
    capturedHandler!({payload: {type: 'leave'}})
    await waitFor(() => expect(result.current.dragOver).toBe(false))
    expect(openDocument).not.toHaveBeenCalled()
  })

  it('unsubscribes on unmount', async () => {
    vi.mocked(getPlatformCapabilities).mockReturnValue(caps(true))
    const {unmount} = renderHook(() => useFileDrop(vi.fn()))
    await waitFor(() => expect(capturedHandler).not.toBeNull())
    unmount()
    expect(unlistenMock).toHaveBeenCalled()
  })
})
