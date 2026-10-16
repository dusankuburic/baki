import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, within} from '@testing-library/react'
import AnalyticsDashboard from './AnalyticsDashboard'
import {ToastProvider} from '@/components/shared/Toast'

import {useSystemStore} from '@/stores/systemStore'

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
  getDashboard.mockReset()
  batchAnalyze.mockReset()
  fileOpenDirectory.mockReset()
  useSystemStore.setState({
    isLoaded: true,
    info: {
      version: '1.0.0',
      platform: 'win32',
      arch: 'x64',
      buildDate: '',
      gitCommit: '',
      capabilities: {sessionAnalytics: true},
    },
  })
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
      totalFlowsAnalyzed: 0,
      totalFindings: 0,
      findingsBySeverity: {},
      findingsByCategory: {},
      findingsByRule: {},
      avgHealthScore: 0,
      topProblemFlows: [],
    })
    renderDash()
    expect(await screen.findByText(/No analyses yet/)).toBeInTheDocument()
  })

  it('renders stats, rule bars, and problem flows', async () => {
    getDashboard.mockResolvedValue({
      totalFlowsAnalyzed: 3,
      totalFindings: 12,
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

  it('shows a notice when session analytics are disabled and does not fetch', () => {
    useSystemStore.setState({
      info: {
        version: '1.0.0',
        platform: 'win32',
        arch: 'x64',
        buildDate: '',
        gitCommit: '',
        capabilities: {sessionAnalytics: false},
      },
    })
    renderDash()
    expect(screen.getByText(/not available in this mode/)).toBeInTheDocument()
    expect(getDashboard).not.toHaveBeenCalled()
  })

  it('runs batch analysis and renders per-file error rows', async () => {
    getDashboard.mockResolvedValue({
      totalFlowsAnalyzed: 0,
      totalFindings: 0,
      findingsBySeverity: {},
      findingsByCategory: {},
      findingsByRule: {},
      avgHealthScore: 0,
      topProblemFlows: [],
    })
    fileOpenDirectory.mockResolvedValue('C:/flows')
    batchAnalyze.mockResolvedValue({
      results: [
        {
          flowId: 'a',
          flowName: 'good.txt',
          report: {flowId: 'a', findings: [], stats: {errors: 1, warnings: 2, info: 0}, metrics: {healthScore: 84}},
        },
        {flowId: '', flowName: 'broken.txt', report: null, error: 'no flow content found'},
      ],
      totalFlows: 2,
      totalFindings: 3,
      totalErrors: 1,
      totalWarnings: 2,
      totalInfo: 0,
      avgHealthScore: 84,
      durationMs: 5,
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

  it("shows the folder's library findings across flows, grouped apart from the per-flow rows", async () => {
    getDashboard.mockResolvedValue({
      totalFlowsAnalyzed: 0,
      totalFindings: 0,
      findingsBySeverity: {},
      findingsByCategory: {},
      findingsByRule: {},
      avgHealthScore: 0,
      topProblemFlows: [],
    })
    fileOpenDirectory.mockResolvedValue('C:/flows')
    const finding = (flowId: string, flowName: string, subflowName: string, other: [string, string]) => ({
      id: '',
      ruleId: 'duplicate-subflow-across-flows',
      severity: 'info',
      title: 'Subflow duplicated across flows',
      description: `Subflow "${subflowName}" has the same steps as a subflow of another flow.`,
      blockId: '',
      subflowId: '',
      suggestion: "Move the shared steps into one desktop flow and call it with 'Run desktop flow'.",
      stage: 'preview',
      flowId,
      flowName,
      subflowName,
      related: [{flowId: other[0], flowName: other[1], subflowName: other[0] === 'b' ? 'SignIn' : 'Login'}],
    })
    batchAnalyze.mockResolvedValue({
      results: [
        {
          flowId: 'a',
          flowName: 'invoices',
          report: {flowId: 'a', findings: [], stats: {errors: 0, warnings: 0, info: 0}},
        },
        {
          flowId: 'b',
          flowName: 'orders',
          report: {flowId: 'b', findings: [], stats: {errors: 0, warnings: 0, info: 0}},
        },
      ],
      totalFlows: 2,
      totalFindings: 0,
      totalErrors: 0,
      totalWarnings: 0,
      totalInfo: 0,
      avgHealthScore: 100,
      durationMs: 5,
      library: {
        findings: [
          finding('a', 'invoices', 'Login', ['b', 'orders']),
          finding('b', 'orders', 'SignIn', ['a', 'invoices']),
        ],
        flows: 2,
        rulesRun: 1,
        generatedAt: '2026-10-14T00:00:00Z',
        durationMs: 1,
      },
    })

    renderDash()
    fireEvent.click(await screen.findByText('Analyze Folder…'))

    const region = await screen.findByRole('region', {name: 'Across flows'})
    expect(within(region).getByText('Subflow duplicated across flows')).toBeInTheDocument()
    expect(within(region).getByText('invoices › Login')).toBeInTheDocument()
    expect(within(region).getByText('orders › SignIn')).toBeInTheDocument()
    expect(within(region).getByText('Also in orders › SignIn')).toBeInTheDocument()
    expect(within(region).getByText('Preview')).toBeInTheDocument()
  })

  it('post-batch refresh does not flash loading skeleton', async () => {
    getDashboard.mockResolvedValue({
      totalFlowsAnalyzed: 3,
      totalFindings: 5,
      findingsBySeverity: {error: 1, warning: 2, info: 2},
      findingsByCategory: {Style: 5},
      findingsByRule: {},
      avgHealthScore: 70,
      topProblemFlows: [],
    })
    fileOpenDirectory.mockResolvedValue('C:/flows')
    batchAnalyze.mockResolvedValue({
      results: [],
      totalFlows: 0,
      totalFindings: 0,
      totalErrors: 0,
      totalWarnings: 0,
      totalInfo: 0,
      avgHealthScore: 0,
      durationMs: 1,
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
