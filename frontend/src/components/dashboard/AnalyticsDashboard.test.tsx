import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import AnalyticsDashboard from './AnalyticsDashboard'
import {ToastProvider} from '@/components/shared/Toast'

const getDashboard = vi.fn()
const batchAnalyze = vi.fn()
const fileOpenDirectory = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    getDashboard: (...a: unknown[]) => getDashboard(...a),
    batchAnalyze: (...a: unknown[]) => batchAnalyze(...a),
  },
}))

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({fileOpenDirectory: (...a: unknown[]) => fileOpenDirectory(...a)}),
}))

function renderDash() {
  return render(
    <ToastProvider>
      <AnalyticsDashboard />
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('AnalyticsDashboard', () => {
  it('shows the empty state when nothing has been analyzed', async () => {
    getDashboard.mockResolvedValue({
      totalFlowsAnalyzed: 0, totalFindings: 0,
      findingsBySeverity: {}, findingsByCategory: {}, findingsByRule: {},
      avgHealthScore: 0, topProblemFlows: [],
    })
    renderDash()
    expect(await screen.findByText(/No analyses yet/)).toBeInTheDocument()
  })

  it('renders stats, rule bars, and problem flows', async () => {
    getDashboard.mockResolvedValue({
      totalFlowsAnalyzed: 3, totalFindings: 12,
      findingsBySeverity: {error: 2, warning: 7, info: 3},
      findingsByCategory: {Reliability: 8, Style: 4},
      findingsByRule: {'missing-timeout': 5, 'unused-variable': 4},
      avgHealthScore: 81.4,
      topProblemFlows: [{flowId: 'f1', flowName: 'Invoices.txt', findingCount: 9, healthScore: 62}],
    })
    renderDash()
    expect(await screen.findByText('Flows Analyzed')).toBeInTheDocument()
    expect(screen.getByText('12')).toBeInTheDocument()
    expect(screen.getByText('missing-timeout')).toBeInTheDocument()
    expect(screen.getByText('Invoices.txt')).toBeInTheDocument()
    expect(screen.getByText('7 warning')).toBeInTheDocument()
  })

  it('runs batch analysis and renders per-file error rows', async () => {
    getDashboard.mockResolvedValue({
      totalFlowsAnalyzed: 0, totalFindings: 0,
      findingsBySeverity: {}, findingsByCategory: {}, findingsByRule: {},
      avgHealthScore: 0, topProblemFlows: [],
    })
    fileOpenDirectory.mockResolvedValue('C:/flows')
    batchAnalyze.mockResolvedValue({
      results: [
        {
          flowId: 'a', flowName: 'good.txt',
          report: {flowId: 'a', findings: [], stats: {errors: 1, warnings: 2, info: 0}, metrics: {healthScore: 84}},
        },
        {flowId: '', flowName: 'broken.txt', report: null, error: 'no flow content found'},
      ],
      totalFlows: 2, totalFindings: 3, totalErrors: 1, totalWarnings: 2, totalInfo: 0,
      avgHealthScore: 84, durationMs: 5,
    })

    renderDash()
    fireEvent.click(await screen.findByText('Analyze Folder…'))

    await waitFor(() => {
      expect(batchAnalyze).toHaveBeenCalledWith('C:/flows')
    })
    expect(await screen.findByText('good.txt')).toBeInTheDocument()
    expect(screen.getByText('broken.txt')).toBeInTheDocument()
    expect(screen.getByText(/no flow content found/)).toBeInTheDocument()
  })
})
