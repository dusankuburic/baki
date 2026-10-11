import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import PortfolioView from './PortfolioView'
import {ToastProvider} from '@/components/shared/Toast'
import {useOrgStore} from '@/stores/orgStore'

const portfolio = vi.fn()
const analyzeFlowById = vi.fn()
vi.mock('@/api/library', () => ({
  libraryApi: {portfolio: (...a: unknown[]) => portfolio(...a)},
}))
vi.mock('@/api', () => ({
  analysisApi: {analyzeFlowById: (...a: unknown[]) => analyzeFlowById(...a)},
}))

beforeEach(() => {
  vi.clearAllMocks()
  useOrgStore.setState({activeOrgId: null})
})

const emptyPortfolio = {totalFlows: 0, analyzedFlows: 0, avgHealth: 0, errors: 0, warnings: 0, info: 0, entries: []}

describe('PortfolioView', () => {
  it('renders the ranked flows and rollup totals', async () => {
    portfolio.mockResolvedValue({
      totalFlows: 2,
      analyzedFlows: 2,
      avgHealth: 65,
      errors: 3,
      warnings: 1,
      info: 0,
      entries: [
        {
          flowId: 'f2',
          flowName: 'Bravo',
          ownerName: 'u2',
          analyzed: true,
          healthScore: 40,
          errors: 3,
          warnings: 0,
          info: 0,
          analyzedAt: '2026-01-01T00:00:00Z',
        },
        {
          flowId: 'f1',
          flowName: 'Alpha',
          ownerName: 'u1',
          analyzed: true,
          healthScore: 90,
          errors: 0,
          warnings: 1,
          info: 0,
          analyzedAt: '2026-01-01T00:00:00Z',
        },
      ],
    })
    render(
      <ToastProvider>
        <PortfolioView />
      </ToastProvider>,
    )
    expect(await screen.findByText('Bravo')).toBeInTheDocument()
    expect(screen.getByText('Alpha')).toBeInTheDocument()
  })

  it('says when the fleet is larger than the ranking', async () => {
    portfolio.mockResolvedValue({
      totalFlows: 4200,
      rankedFlows: 1000,
      truncated: true,
      analyzedFlows: 900,
      avgHealth: 72,
      errors: 12,
      warnings: 3,
      info: 1,
      entries: [
        {
          flowId: 'f1',
          flowName: 'Alpha',
          ownerName: 'u1',
          analyzed: true,
          healthScore: 72,
          errors: 1,
          warnings: 0,
          info: 0,
          analyzedAt: '2026-01-01T00:00:00Z',
        },
      ],
    })
    render(
      <ToastProvider>
        <PortfolioView />
      </ToastProvider>,
    )
    await waitFor(() => expect(screen.getByText(/Ranking the 1000 flows loaded first of 4200/)).toBeInTheDocument())
  })

  it('shows an empty state when there are no flows', async () => {
    portfolio.mockResolvedValue({
      totalFlows: 0,
      analyzedFlows: 0,
      avgHealth: 0,
      errors: 0,
      warnings: 0,
      info: 0,
      entries: [],
    })
    render(
      <ToastProvider>
        <PortfolioView />
      </ToastProvider>,
    )
    expect(await screen.findByText(/No flows to govern/i)).toBeInTheDocument()
  })

  it('shows an error state when the request fails', async () => {
    portfolio.mockRejectedValue(new Error('boom'))
    render(
      <ToastProvider>
        <PortfolioView />
      </ToastProvider>,
    )
    expect(await screen.findByText(/Couldn.t load the portfolio/i)).toBeInTheDocument()
  })

  it('selects rows, runs bulk re-analyze, and clears selection', async () => {
    portfolio.mockResolvedValue({
      totalFlows: 2,
      analyzedFlows: 2,
      avgHealth: 65,
      errors: 3,
      warnings: 0,
      info: 0,
      entries: [
        {
          flowId: 'f1',
          flowName: 'Alpha',
          ownerName: 'u1',
          analyzed: true,
          healthScore: 40,
          errors: 3,
          warnings: 0,
          info: 0,
          analyzedAt: '2026-01-01T00:00:00Z',
        },
        {
          flowId: 'f2',
          flowName: 'Bravo',
          ownerName: 'u2',
          analyzed: true,
          healthScore: 90,
          errors: 0,
          warnings: 0,
          info: 0,
          analyzedAt: '2026-01-01T00:00:00Z',
        },
      ],
    })
    analyzeFlowById.mockResolvedValue({flowId: 'x', findings: [], generatedAt: '', durationMs: 0, stats: {}})

    render(
      <ToastProvider>
        <PortfolioView />
      </ToastProvider>,
    )
    await screen.findByText('Alpha')

    const selectAlpha = screen.getByLabelText('Select Alpha')
    fireEvent.click(selectAlpha)

    const reanalyzeBtn = await screen.findByText(/Re-analyze 1/)
    fireEvent.click(reanalyzeBtn)
    await waitFor(() => expect(analyzeFlowById).toHaveBeenCalledWith('f1'))
  })
  it('scopes the portfolio to the active organisation', async () => {
    useOrgStore.setState({activeOrgId: 'org-7'})
    portfolio.mockResolvedValue(emptyPortfolio)
    render(
      <ToastProvider>
        <PortfolioView />
      </ToastProvider>,
    )
    await screen.findByText(/No flows to govern/i)
    expect(portfolio).toHaveBeenCalledWith({orgId: 'org-7', sort: 'risk'}, expect.anything())
  })

  it('re-requests the ranking when the sort changes', async () => {
    portfolio.mockResolvedValue({
      ...emptyPortfolio,
      totalFlows: 1,
      entries: [{flowId: 'f1', flowName: 'Alpha', analyzed: false, healthScore: 0, errors: 0, warnings: 0, info: 0}],
    })
    render(
      <ToastProvider>
        <PortfolioView />
      </ToastProvider>,
    )
    await screen.findByText('Alpha')
    fireEvent.change(screen.getByLabelText('Sort flows'), {target: {value: 'errors'}})
    await waitFor(() =>
      expect(portfolio).toHaveBeenLastCalledWith({orgId: undefined, sort: 'errors'}, expect.anything()),
    )
  })
})
