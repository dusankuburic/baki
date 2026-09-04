import type {ReactNode} from 'react'
import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import type {Finding, AnalysisReport, FlowDocument} from '@/types'

const analyzeFlow = vi.fn()
const deduplicate = vi.fn()
const getDiff = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    analyzeFlow: (...a: unknown[]) => analyzeFlow(...a),
    analyzeFlowById: (...a: unknown[]) => analyzeFlow(...a),
    deduplicate: (...a: unknown[]) => deduplicate(...a),
    getDiff: (...a: unknown[]) => getDiff(...a),
    exportHTML: vi.fn().mockResolvedValue(''),
    listFindingStatuses: vi.fn().mockResolvedValue([]),
    getBaseline: vi.fn().mockResolvedValue(null),
    baselineDrift: vi.fn().mockResolvedValue(null),
  },
  flowApi: {
    suppressFindingsBatch: vi.fn().mockResolvedValue(undefined),
    listSnapshots: vi.fn().mockResolvedValue({
      snapshots: [{id: 'snap-1', label: 'before fix', createdAt: new Date().toISOString(), bytes: 100}],
    }),
    restoreSnapshot: vi.fn(),
  },
}))

// react-virtuoso renders nothing in jsdom (zero-height scroller), so stub it with
// a passthrough that renders every row. The tests assert on findings content, not
// on windowing behaviour.
vi.mock('react-virtuoso', () => ({
  Virtuoso: ({
    data = [],
    itemContent,
    computeItemKey,
  }: {
    data?: unknown[]
    itemContent: (i: number, item: unknown) => ReactNode
    computeItemKey?: (i: number, item: unknown) => string | number
  }) => (
    <div>
      {data.map((item, i) => (
        <div key={computeItemKey ? computeItemKey(i, item) : i}>{itemContent(i, item)}</div>
      ))}
    </div>
  ),
}))

const mockDoc = {
  id: 'flow-1',
  name: 'Test',
  filePath: '/test.txt',
  subflows: [{id: 'sf1', name: 'Main', blocks: []}],
  metadata: {fileSize: 1000, totalBlocks: 10, totalSubflows: 1},
} as unknown as FlowDocument

const f1: Finding = {
  id: 'f1',
  ruleId: 'dead-code',
  severity: 'info',
  category: 'Style',
  title: 'Dead code',
  description: 'Unused block',
  blockId: 'b1',
  subflowId: 'sf1',
}
const f1dup: Finding = {
  id: 'f1d',
  ruleId: 'dead-code',
  severity: 'info',
  category: 'Style',
  title: 'Dead code',
  description: 'Unused block',
  blockId: 'b1',
  subflowId: 'sf1',
}
const f2: Finding = {
  id: 'f2',
  ruleId: 'unhandled-error',
  severity: 'warning',
  category: 'Reliability',
  title: 'Unhandled error',
  description: 'No handler',
  blockId: 'b2',
  subflowId: 'sf1',
}

const mockReport = {
  flowId: 'flow-1',
  flowName: 'Test',
  generatedAt: '2024-01-01T00:00:00Z',
  findings: [f1, f1dup, f2],
  stats: {errors: 0, warnings: 1, info: 2, blocksAnalyzed: 10, rulesRun: 29},
  durationMs: 50,
  metrics: {
    healthScore: 75,
    totalBlocks: 10,
    totalVariables: 5,
    subflowCount: 1,
    subflows: [],
    maxCyclomatic: 1,
    avgCyclomatic: 1,
    maxCognitive: 1,
    avgCognitive: 1,
    variableDensity: 0.5,
  },
} as unknown as AnalysisReport

function setupStores() {
  useFlowStore.setState({
    document: mockDoc,
    selectedSubflowId: 'sf1',
  })
  useAnalysisStore.setState({
    reports: new Map([['flow-1', mockReport]]),
    isAnalyzing: false,
    severityFilter: new Set(['error', 'warning', 'info']),
    categoryFilter: new Set(['Security', 'Reliability', 'Performance', 'Style', 'Logic']),
    suppressedFindings: [],
    findingSearch: '',
  })
}

async function renderTab() {
  const FindingsTab = (await import('./FindingsTab')).default
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <FindingsTab />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  setupStores()
})

describe('re-analysis keeps the list mounted (U1.1)', () => {
  it('shows the stale findings list with a progress strip while analyzing, not the full spinner', async () => {
    useAnalysisStore.setState({isAnalyzing: true, progress: {current: 3, total: 10, ruleName: 'dead-code'}})
    await renderTab()
    // Stale report content still mounted...
    expect(await screen.findByText('Dead code')).toBeInTheDocument()
    // ...with the non-blocking strip above it. (ToastProvider also owns a
    // role="status" live region, so scope by the strip's aria-label.)
    const strip = screen.getByLabelText('Analysis in progress, 30%')
    expect(strip).toHaveTextContent('dead-code')
    // The full-pane spinner is NOT the view.
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
  })

  it('renders the strip as indeterminate when total is 0 (never "0%")', async () => {
    useAnalysisStore.setState({isAnalyzing: true, progress: {current: 0, total: 0, ruleName: ''}})
    await renderTab()
    const strip = screen.getByLabelText('Analysis in progress')
    expect(strip).toHaveTextContent('Analyzing…')
    expect(strip).not.toHaveTextContent('0%')
  })

  it('falls back to the full-pane runner when there is no report yet', async () => {
    useAnalysisStore.setState({reports: new Map(), isAnalyzing: true, progress: {current: 0, total: 0, ruleName: ''}})
    await renderTab()
    expect(screen.getByRole('progressbar')).toBeInTheDocument()
    expect(screen.queryByLabelText(/analysis in progress/i)).not.toBeInTheDocument()
  })

  it('does not start a second analysis while one is in flight', async () => {
    useAnalysisStore.setState({isAnalyzing: false})
    analyzeFlow.mockResolvedValue(new Promise(() => {})) // never settles
    await renderTab()
    const reanalyze = await screen.findByRole('button', {name: /re-?analy/i})
    fireEvent.click(reanalyze)
    fireEvent.click(reanalyze)
    await waitFor(() => expect(analyzeFlow).toHaveBeenCalledTimes(1))
  })
})

describe('snapshot restore (U1.4)', () => {
  it('re-analyzes after restoring a snapshot', async () => {
    analyzeFlow.mockResolvedValue({...mockReport, generatedAt: '2024-01-02T00:00:00Z'})
    const restored = {...mockDoc, name: 'Restored'}
    const restoreSnapshot = (await import('@/api')).flowApi.restoreSnapshot as ReturnType<typeof vi.fn>
    restoreSnapshot.mockResolvedValue({document: restored})
    await renderTab()

    // Open the undo popover (the Undo2 icon button) and click the row.
    const undoBtn = await screen.findByRole('button', {name: /undo a fix/i})
    fireEvent.click(undoBtn)
    const row = await screen.findByText('before fix')
    fireEvent.click(row)
    await waitFor(() => expect(restoreSnapshot).toHaveBeenCalledWith('flow-1', 'snap-1'))
    // Doc swapped + re-analysis fired (the stale-list refresh contract).
    await waitFor(() => expect(analyzeFlow).toHaveBeenCalled())
    expect(useFlowStore.getState().document?.name).toBe('Restored')
  })
})

describe('FindingsTab', () => {
  it('renders all findings by default', async () => {
    await renderTab()
    await waitFor(() => {
      expect(screen.getByText('Unhandled error')).toBeInTheDocument()
    })
  })

  it('hides duplicate findings when Layers toggle is clicked', async () => {
    deduplicate.mockResolvedValue({
      deduplicated: [f1, f2],
      groups: [
        {blockId: 'b1', findings: [f1, f1dup], primary: f1, duplicateCount: 1},
        {blockId: 'b2', findings: [f2], primary: f2, duplicateCount: 0},
      ],
      originalCount: 3,
      dedupedCount: 2,
    })

    await renderTab()
    await waitFor(() => expect(screen.getByText('Unhandled error')).toBeInTheDocument())

    const layersBtn = screen.getByLabelText('Toggle duplicate grouping')
    fireEvent.click(layersBtn)

    await waitFor(() => {
      expect(deduplicate).toHaveBeenCalledTimes(1)
    })

    await waitFor(() => {
      expect(screen.getByText(/Grouped: 2 unique findings/)).toBeInTheDocument()
    })
  })

  it('restores all findings when Layers toggle is clicked again', async () => {
    deduplicate.mockResolvedValue({
      deduplicated: [f1, f2],
      groups: [
        {blockId: 'b1', findings: [f1, f1dup], primary: f1, duplicateCount: 1},
        {blockId: 'b2', findings: [f2], primary: f2, duplicateCount: 0},
      ],
      originalCount: 3,
      dedupedCount: 2,
    })

    await renderTab()
    await waitFor(() => expect(screen.getByText('Unhandled error')).toBeInTheDocument())

    const layersBtn = screen.getByLabelText('Toggle duplicate grouping')
    fireEvent.click(layersBtn)
    await waitFor(() => expect(screen.getByText(/Grouped/)).toBeInTheDocument())

    fireEvent.click(layersBtn)
    await waitFor(() => expect(screen.queryByText(/Grouped/)).not.toBeInTheDocument())
  })

  it('shows empty state when no findings', async () => {
    useAnalysisStore.setState({
      reports: new Map([
        [
          'flow-1',
          {...mockReport, findings: [], stats: {errors: 0, warnings: 0, info: 0, blocksAnalyzed: 10, rulesRun: 29}},
        ] as [string, AnalysisReport],
      ]),
    })

    await renderTab()
    await waitFor(() => {
      expect(screen.getByText(/No findings/)).toBeInTheDocument()
    })
  })

  // The search input is debounced: typing updates the field immediately but
  // the (expensive) store-driven filter only runs after the debounce elapses.
  describe('search debounce', () => {
    it('does not filter before the debounce elapses', async () => {
      await renderTab()
      await waitFor(() => expect(screen.getByText('Unhandled error')).toBeInTheDocument())

      // Fake timers AFTER mount so waitFor's polling isn't frozen; only the
      // debounce setTimeout should be under fake control.
      vi.useFakeTimers()
      const input = screen.getByPlaceholderText('Search findings...')
      fireEvent.change(input, {target: {value: 'zzz-no-match'}})
      // Input shows the typed value immediately...
      expect(input).toHaveValue('zzz-no-match')
      // ...but the store (which drives the filter) hasn't been written yet,
      // so the non-matching list is still rendered.
      expect(useAnalysisStore.getState().findingSearch).toBe('')
      expect(screen.getByText('Unhandled error')).toBeInTheDocument()

      vi.advanceTimersByTime(200)
      vi.useRealTimers()
      await waitFor(() => expect(useAnalysisStore.getState().findingSearch).toBe('zzz-no-match'))
    })

    it('applies the query after the debounce elapses', async () => {
      await renderTab()
      await waitFor(() => expect(screen.getByText('Unhandled error')).toBeInTheDocument())

      fireEvent.change(screen.getByPlaceholderText('Search findings...'), {target: {value: 'unhandled'}})
      await waitFor(() => expect(useAnalysisStore.getState().findingSearch).toBe('unhandled'), {timeout: 1000})
      // Still matches; the non-matching finding is filtered out.
      await waitFor(() => expect(screen.queryByText('Dead code')).not.toBeInTheDocument())
    })
  })
})
