import type {ReactNode} from 'react'
import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {AnalysisDiff, Finding, FlowDocument} from '@/types'

vi.mock('@/api', () => ({
  analysisApi: {
    getRelatedFindings: vi.fn().mockResolvedValue([]),
    listFindingStatuses: vi.fn().mockResolvedValue([]),
    setFindingStatus: vi.fn().mockResolvedValue({}),
  },
  flowApi: {
    previewFix: vi.fn(),
    applyFix: vi.fn(),
    suppressInSource: vi.fn(),
  },
}))

// react-virtuoso measures nothing in jsdom. This stand-in renders only the
// first WINDOW rows — the way a real viewport shows a handful — and reports
// the row count, so both the row model and the "only a window is mounted"
// contract can be asserted. Before the row model, every finding was a
// mounted FindingCard regardless of any virtualizer.
const WINDOW = 30
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
    <div data-testid="virtuoso" data-row-count={data.length}>
      {data.slice(0, WINDOW).map((item, i) => (
        <div key={computeItemKey ? computeItemKey(i, item) : i}>{itemContent(i, item)}</div>
      ))}
    </div>
  ),
}))

import AnalysisDiffView from './AnalysisDiffView'

const mockDoc = {
  id: 'flow-1',
  name: 'Test',
  filePath: '/test.txt',
  subflows: [{id: 'sf1', name: 'Main', blocks: []}],
  metadata: {fileSize: 1000, totalBlocks: 10, totalSubflows: 1},
} as unknown as FlowDocument

function finding(id: string): Finding {
  return {
    id,
    ruleId: 'unhandled-error',
    severity: 'warning',
    category: 'Reliability',
    title: 'Unhandled error',
    description: 'No handler',
    blockId: `b-${id}`,
    subflowId: 'sf1',
  }
}

function findings(prefix: string, n: number): Finding[] {
  return Array.from({length: n}, (_, i) => finding(`${prefix}-${i}`))
}

function makeDiff(added: Finding[], removed: Finding[] = [], persisted: Finding[] = []): AnalysisDiff {
  return {
    flowId: 'flow-1',
    added,
    removed,
    persisted,
    addedCount: added.length,
    removedCount: removed.length,
    persistedCount: persisted.length,
    hasPrevious: true,
  }
}

function renderDiff(diff: AnalysisDiff, onFixWithAI?: (f: Finding) => void) {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <AnalysisDiffView diff={diff} blockLookup={new Map()} onBack={() => {}} onFixWithAI={onFixWithAI} />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

// Every FindingCard renders exactly one "Jump" action.
const mountedCards = () => screen.queryAllByText('Jump').length
const rowCount = () => Number(screen.getByTestId('virtuoso').getAttribute('data-row-count'))
// Section headers are the buttons that carry a count; locate one by its count.
const headerWithCount = (count: number) => screen.getByText(String(count)).closest('button') as HTMLButtonElement

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState({document: mockDoc, readOnly: false})
  useAnalysisStore.setState({
    reports: new Map(),
    suppressedFindings: [],
    suppressedKeys: new Set(),
    triageMap: new Map(),
  })
})

describe('AnalysisDiffView virtualisation', () => {
  it('3,000 new findings: the header counts all of them while only a viewport of cards is mounted', () => {
    renderDiff(makeDiff(findings('new', 3000)))

    // Exact count in the section header (and the summary strip).
    expect(headerWithCount(3000)).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('+3000 / −0 / =0')).toBeInTheDocument()
    // Row model: 3 headers + 3,000 cards + the "none" row of the open, empty
    // Fixed section (Persisted starts collapsed, so no row for it).
    expect(rowCount()).toBe(3004)
    // Only the window is mounted, not one card per finding.
    expect(mountedCards()).toBeGreaterThan(0)
    expect(mountedCards()).toBeLessThanOrEqual(WINDOW)
  })

  it('collapsing a section drops its cards from the row model and keeps its count', () => {
    renderDiff(makeDiff(findings('new', 3)))
    expect(mountedCards()).toBe(3)

    fireEvent.click(headerWithCount(3))

    expect(headerWithCount(3)).toHaveAttribute('aria-expanded', 'false')
    expect(mountedCards()).toBe(0)
    expect(rowCount()).toBe(4) // 3 headers + the empty Fixed section's "none" row
  })

  it('persisted findings start collapsed and open on click', () => {
    renderDiff(makeDiff([], [], findings('old', 2)))
    expect(headerWithCount(2)).toHaveAttribute('aria-expanded', 'false')
    expect(mountedCards()).toBe(0)

    fireEvent.click(headerWithCount(2))

    expect(headerWithCount(2)).toHaveAttribute('aria-expanded', 'true')
    expect(mountedCards()).toBe(2)
  })

  it('offers Fix with AI on new findings but not on fixed (removed) ones', () => {
    renderDiff(makeDiff(findings('new', 1), findings('fixed', 1)), vi.fn())
    expect(mountedCards()).toBe(2)
    expect(screen.getAllByText('Fix with AI')).toHaveLength(1)
  })
})
