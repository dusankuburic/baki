import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act, waitFor} from '@testing-library/react'
import {useAutoAnalyze} from './useAutoAnalyze'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSettingsStore} from '@/stores/settingsStore'
import type {FlowDocument} from '@/types'

const analyzeFlow = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    analyzeFlow: (...a: unknown[]) => analyzeFlow(...a),
  },
}))

vi.mock('@/lib/logger', () => ({
  logger: {warn: vi.fn(), error: vi.fn()},
}))

const doc = {id: 'flow-1', name: 'Flow', subflows: []} as unknown as FlowDocument

describe('useAutoAnalyze', () => {
  beforeEach(() => {
    analyzeFlow.mockReset()
    useFlowStore.setState({document: doc, libraryFlowId: null})
    useAnalysisStore.setState({reports: new Map(), isAnalyzing: false, analyzingGen: 0})
    useSettingsStore.setState({
      settings: {
        ...useSettingsStore.getState().settings,
        analysis: {...useSettingsStore.getState().settings.analysis, autoAnalyzeOnOpen: true},
      },
      isLoaded: true,
    })
  })

  it('fires exactly once when the analysis request keeps failing', async () => {
    analyzeFlow.mockRejectedValue(new Error('forbidden'))

    const {rerender} = renderHook(() => useAutoAnalyze())

    await waitFor(() => expect(analyzeFlow).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(useAnalysisStore.getState().isAnalyzing).toBe(false))

    await act(async () => {
      rerender()
      await Promise.resolve()
    })

    expect(analyzeFlow).toHaveBeenCalledTimes(1)
  })

  it('stores the report on success and does not refire', async () => {
    analyzeFlow.mockResolvedValue({flowId: 'flow-1', findings: []})

    renderHook(() => useAutoAnalyze())

    await waitFor(() => expect(analyzeFlow).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(useAnalysisStore.getState().reports.has('flow-1')).toBe(true))

    expect(useAnalysisStore.getState().isAnalyzing).toBe(false)
  })
})
