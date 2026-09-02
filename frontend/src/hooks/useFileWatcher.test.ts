import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useFileWatcher} from './useFileWatcher'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {FlowDocument} from '@/types'

const getSourceMeta = vi.fn()
const reimport = vi.fn()
const analyzeFlow = vi.fn()

vi.mock('@/api', () => ({
  flowApi: {
    getSourceMeta: (...a: unknown[]) => getSourceMeta(...a),
    reimport: (...a: unknown[]) => reimport(...a),
  },
  analysisApi: {
    analyzeFlow: (...a: unknown[]) => analyzeFlow(...a),
    analyzeFlowById: (...a: unknown[]) => analyzeFlow(...a),
  },
}))

let tauri = true
vi.mock('@/platform/guards', () => ({
  isTauri: () => tauri,
}))

vi.mock('@/lib/logger', () => ({
  logger: {warn: vi.fn(), error: vi.fn()},
}))

const localDoc = {id: 'f1', name: 'F', filePath: '/tmp/x.txt', subflows: []} as unknown as FlowDocument
const meta = (over: Partial<{size: number; modTime: string; files: number}> = {}) => ({
  size: 100,
  modTime: '2026-01-01T00:00:00Z',
  files: 1,
  ...over,
})

function advance(ms: number) {
  return act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
    await Promise.resolve()
  })
}

describe('useFileWatcher', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    tauri = true
    vi.clearAllMocks()
    getSourceMeta.mockResolvedValue(meta())
    reimport.mockResolvedValue({...localDoc})
    analyzeFlow.mockResolvedValue({findings: []})
    useFlowStore.setState({document: localDoc} as never)
  })
  afterEach(() => vi.useRealTimers())

  it('seeds the baseline without reloading', async () => {
    renderHook(() => useFileWatcher(localDoc))
    await advance(5000)
    expect(getSourceMeta).toHaveBeenCalled()
    expect(reimport).not.toHaveBeenCalled()
  })

  it('reimports after a settled external change (two changed polls)', async () => {
    renderHook(() => useFileWatcher(localDoc))
    await advance(5000) // seed

    getSourceMeta.mockResolvedValue(meta({size: 200, modTime: '2026-01-02T00:00:00Z'}))
    await advance(4500) // first changed poll → settling
    expect(reimport).not.toHaveBeenCalled()
    await advance(4500) // second changed poll → reload
    expect(reimport).toHaveBeenCalledWith('f1')
    expect(analyzeFlow).toHaveBeenCalled()
    expect(useFlowStore.getState().document).toEqual(localDoc)
  })

  it('ignores a transient blip (change then revert before settle)', async () => {
    renderHook(() => useFileWatcher(localDoc))
    await advance(5000)
    getSourceMeta.mockResolvedValueOnce(meta({size: 999}))
    await advance(4500)
    getSourceMeta.mockResolvedValue(meta()) // back to baseline
    await advance(9000)
    expect(reimport).not.toHaveBeenCalled()
  })

  it('does not watch cloud docs or web builds', async () => {
    const cloud = {id: 'c1', name: 'C', filePath: '', subflows: []} as unknown as FlowDocument
    const {rerender} = renderHook(({d}: {d: FlowDocument | null}) => useFileWatcher(d), {
      initialProps: {d: cloud},
    })
    await advance(9000)
    tauri = false
    rerender({d: localDoc})
    await advance(9000)
    expect(getSourceMeta).not.toHaveBeenCalled()
  })

  it('re-seeds when the document object changes (own writes are not external)', async () => {
    const {rerender} = renderHook(({d}: {d: FlowDocument | null}) => useFileWatcher(d), {
      initialProps: {d: localDoc},
    })
    await advance(5000)
    // The app itself replaced the doc (fix/save) AND the file changed.
    getSourceMeta.mockResolvedValue(meta({size: 300}))
    rerender({d: {...localDoc} as FlowDocument})
    await advance(4500)
    expect(reimport).not.toHaveBeenCalled() // re-seeded, not treated as external
  })

  it('keeps polling through a failed reimport without looping', async () => {
    reimport.mockRejectedValue(new Error('busy'))
    renderHook(() => useFileWatcher(localDoc))
    await advance(5000)
    getSourceMeta.mockResolvedValue(meta({size: 200, modTime: 'x'}))
    await advance(4500)
    await advance(4500)
    expect(reimport).toHaveBeenCalledTimes(1)
    // Re-seeded baseline: the next unchanged poll is quiet.
    getSourceMeta.mockResolvedValue(meta({size: 200, modTime: 'x'}))
    await advance(4500)
    expect(reimport).toHaveBeenCalledTimes(1)
  })

  it('clears the analysis isAnalyzing flag after reload', async () => {
    renderHook(() => useFileWatcher(localDoc))
    await advance(5000)
    getSourceMeta.mockResolvedValue(meta({size: 200, modTime: 'x2'}))
    await advance(4500)
    await advance(4500)
    // The reload's finally-clause runs on microtasks chained after mocked
    // promises — advanceTimersByTimeAsync(0) flushes them under fake timers
    // (a real setTimeout would never fire while they're mocked).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(useAnalysisStore.getState().isAnalyzing).toBe(false)
  })
})
