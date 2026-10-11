import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, act} from '@testing-library/react'
import {Profiler} from 'react'
import {ToastProvider} from '@/components/shared/Toast'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {AnalysisReport} from '@/types'
import Sidebar from './Sidebar'

const flowTreeProps = vi.fn()
vi.mock('@/components/sidebar/FlowTree', () => ({
  default: (p: unknown) => {
    flowTreeProps(p)
    return null
  },
}))
vi.mock('@/components/sidebar/FileHeader', () => ({default: () => null}))
vi.mock('@/components/sidebar/FileList', () => ({default: () => null}))
vi.mock('@/components/sidebar/SearchBar', () => ({default: () => null}))
vi.mock('@/components/sidebar/FilterChips', () => ({default: () => null}))
vi.mock('@/components/sidebar/VariablesTab', () => ({default: () => null}))
vi.mock('@/components/sidebar/LibraryTab', () => ({default: () => null}))
vi.mock('@/components/sidebar/SidebarToolbar', () => ({default: () => null}))
vi.mock('@/components/sidebar/hooks/useFileOpen', () => ({
  useFileOpen: () => ({
    recentFiles: [],
    isLoading: false,
    handleNewFlow: vi.fn(),
    handleOpenFile: vi.fn(),
    handleOpenFolder: vi.fn(),
    handleSelectFolderFile: vi.fn(),
    handleLoadRecent: vi.fn(),
    handleRemoveRecent: vi.fn(),
    handleClearRecent: vi.fn(),
    handleRevealFile: vi.fn(),
    handleReloadFile: vi.fn(),
  }),
}))
vi.mock('@/components/sidebar/hooks/useSidebarSearch', () => ({
  useSidebarSearch: () => ({
    searchQuery: '',
    handleQueryChange: vi.fn(),
    searchResults: [],
    matchedBlockIds: new Set<string>(),
    searchHighlightsMap: new Map(),
    activeResultIndex: -1,
    nextResult: vi.fn(),
    prevResult: vi.fn(),
  }),
}))
vi.mock('@/api', () => ({analysisApi: {}}))

const report = (blockId: string): AnalysisReport =>
  ({findings: [{id: `${blockId}-finding`, blockId, severity: 'error', ruleId: 'r'}]}) as unknown as AnalysisReport

const initialFlowState = useFlowStore.getState()

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState(initialFlowState, true)
  useAnalysisStore.getState().reset()
  useFlowStore.setState({document: {id: 'f1', name: 'Flow', subflows: []} as never})
})

describe('Sidebar analysis-report subscription', () => {
  it("re-renders for this document's report only, not for another flow's", () => {
    const commits = vi.fn()
    render(
      <ToastProvider>
        <Profiler id="sidebar" onRender={commits}>
          <Sidebar />
        </Profiler>
      </ToastProvider>,
    )
    const before = commits.mock.calls.length
    expect(before).toBeGreaterThan(0)

    act(() => useAnalysisStore.getState().setReport('f2', report('b-other')))
    expect(commits).toHaveBeenCalledTimes(before)

    act(() => useAnalysisStore.getState().setReport('f1', report('b1')))
    expect(commits.mock.calls.length).toBeGreaterThan(before)
    const last = flowTreeProps.mock.calls.at(-1)?.[0] as {findingCounts?: Map<string, number>}
    expect(last.findingCounts?.get('b1')).toBe(1)
  })
})
