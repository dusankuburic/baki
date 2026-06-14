import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import type {Finding, AnalysisReport, FlowDocument} from '@/types'

const analyzeFlow = vi.fn()
const deduplicate = vi.fn()
const getDiff = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    analyzeFlow: (...a: unknown[]) => analyzeFlow(...a),
    deduplicate: (...a: unknown[]) => deduplicate(...a),
    getDiff: (...a: unknown[]) => getDiff(...a),
    exportHTML: vi.fn().mockResolvedValue(''),
  },
}))

const mockDoc = {
  id: 'flow-1', name: 'Test', filePath: '/test.txt',
  subflows: [{id: 'sf1', name: 'Main', blocks: []}],
  metadata: {fileSize: 1000, totalBlocks: 10, totalSubflows: 1},
} as unknown as FlowDocument

const f1: Finding = {id: 'f1', ruleId: 'dead-code', severity: 'info', category: 'Style', title: 'Dead code', description: 'Unused block', blockId: 'b1', subflowId: 'sf1'}
const f1dup: Finding = {id: 'f1d', ruleId: 'dead-code', severity: 'info', category: 'Style', title: 'Dead code', description: 'Unused block', blockId: 'b1', subflowId: 'sf1'}
const f2: Finding = {id: 'f2', ruleId: 'unhandled-error', severity: 'warning', category: 'Reliability', title: 'Unhandled error', description: 'No handler', blockId: 'b2', subflowId: 'sf1'}

const mockReport = {
  flowId: 'flow-1', flowName: 'Test',
  generatedAt: '2024-01-01T00:00:00Z',
  findings: [f1, f1dup, f2],
  stats: {errors: 0, warnings: 1, info: 2, blocksAnalyzed: 10, rulesRun: 29},
  durationMs: 50,
  metrics: {healthScore: 75, totalBlocks: 10, totalVariables: 5, subflowCount: 1, subflows: [], maxCyclomatic: 1, avgCyclomatic: 1, maxCognitive: 1, avgCognitive: 1, variableDensity: 0.5},
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
      <FindingsTab />
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  setupStores()
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
      reports: new Map([['flow-1', {...mockReport, findings: [], stats: {errors: 0, warnings: 0, info: 0, blocksAnalyzed: 10, rulesRun: 29}}] as [string, AnalysisReport]]),
    })

    await renderTab()
    await waitFor(() => {
      expect(screen.getByText(/No findings/)).toBeInTheDocument()
    })
  })
})
