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

// AnalyticsDashboard is a desktop-only view; default the tests to desktop so the
// existing assertions exercise the live data path, and flip to web where needed.
const isTauriMock = vi.fn(() => true)
vi.mock('@/platform/guards', () => ({
  isTauri: () => isTauriMock(),
  isWeb: () => !isTauriMock(),
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
  getDashboard.mockReset()
  batchAnalyze.mockReset()
  fileOpenDirectory.mockReset()
  isTauriMock.mockReturnValue(true)
})

describe('AnalyticsDashboard', () => {
  it('shows loading skeleton during initial fetch', () => {
    getDashboard.mockReturnValue(new Promise(() => {}))
    const {container} = renderDash()
    const skeletons = container.querySelectorAll('.animate-pulse')
    expect(skeletons.length).toBeGreaterThan(0)
  })

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

  it('shows a desktop-only notice in web mode and does not fetch', () => {
    isTauriMock.mockReturnValue(false)
    renderDash()
    expect(screen.getByText(/desktop-only view/)).toBeInTheDocument()
    expect(getDashboard).not.toHaveBeenCalled()
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

  it('post-batch refresh does not flash loading skeleton', async () => {
    getDashboard.mockResolvedValue({
      totalFlowsAnalyzed: 3, totalFindings: 5,
      findingsBySeverity: {error: 1, warning: 2, info: 2},
      findingsByCategory: {Style: 5}, findingsByRule: {},
      avgHealthScore: 70, topProblemFlows: [],
    })
    fileOpenDirectory.mockResolvedValue('C:/flows')
    batchAnalyze.mockResolvedValue({
      results: [], totalFlows: 0, totalFindings: 0,
      totalErrors: 0, totalWarnings: 0, totalInfo: 0,
      avgHealthScore: 0, durationMs: 1,
    })

    const {container} = renderDash()
    await screen.findByText('Flows Analyzed')

    fireEvent.click(await screen.findByText('Analyze Folder…'))
    await waitFor(() => expect(batchAnalyze).toHaveBeenCalled())

    await new Promise(r => setTimeout(r, 50))
    expect(screen.getByText('Flows Analyzed')).toBeInTheDocument()
    expect(container.querySelectorAll('.animate-pulse').length).toBeLessThanOrEqual(2)
  })
})
