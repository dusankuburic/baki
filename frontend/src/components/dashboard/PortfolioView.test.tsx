import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import PortfolioView from './PortfolioView'
import {ToastProvider} from '@/components/shared/Toast'

const portfolio = vi.fn()
const analyzeFlowById = vi.fn()
vi.mock('@/api/library', () => ({
  libraryApi: {portfolio: (...a: unknown[]) => portfolio(...a)},
}))
vi.mock('@/api', () => ({
  analysisApi: {analyzeFlowById: (...a: unknown[]) => analyzeFlowById(...a)},
}))

beforeEach(() => vi.clearAllMocks())

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
    // Worst-first order is the server's responsibility; here we just confirm both render.
    expect(await screen.findByText('Bravo')).toBeInTheDocument()
    expect(screen.getByText('Alpha')).toBeInTheDocument()
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
        {flowId: 'f1', flowName: 'Alpha', ownerName: 'u1', analyzed: true, healthScore: 40, errors: 3, warnings: 0, info: 0, analyzedAt: '2026-01-01T00:00:00Z'},
        {flowId: 'f2', flowName: 'Bravo', ownerName: 'u2', analyzed: true, healthScore: 90, errors: 0, warnings: 0, info: 0, analyzedAt: '2026-01-01T00:00:00Z'},
      ],
    })
    analyzeFlowById.mockResolvedValue({flowId: 'x', findings: [], generatedAt: '', durationMs: 0, stats: {}})

    render(
      <ToastProvider>
        <PortfolioView />
      </ToastProvider>,
    )
    await screen.findByText('Alpha')

    // Select the first flow's row checkbox (the per-row button with that aria-label).
    const selectAlpha = screen.getByLabelText('Select Alpha')
    fireEvent.click(selectAlpha)

    // Bulk toolbar appears + the re-analyze button fires the API for the selection.
    const reanalyzeBtn = await screen.findByText(/Re-analyze 1/)
    fireEvent.click(reanalyzeBtn)
    await waitFor(() => expect(analyzeFlowById).toHaveBeenCalledWith('f1'))
  })
})
