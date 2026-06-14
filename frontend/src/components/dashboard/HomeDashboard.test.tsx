import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useAnalysisStore} from '@/stores/analysisStore'
import HomeDashboard from './HomeDashboard'

const getHome = vi.fn()

vi.mock('@/api', () => ({
  dashboardApi: {
    getHome: (...a: unknown[]) => getHome(...a),
  },
  libraryApi: {
    getContent: vi.fn(),
  },
}))

vi.mock('recharts', async (importOriginal) => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => <div style={{width: 400, height: 200}}>{children}</div>,
  }
})

vi.mock('./home/useChartColors', () => ({
  useChartColors: () => ({
    success: '#22c55e', warning: '#eab308', error: '#ef4444',
    brand400: '#818cf8', brand500: '#6366f1', brand600: '#4f46d5',
    surface3: '#26262d', borderStrong: '#3f3f47', textTertiary: '#71717a',
  }),
  healthColor: (score: number, c: Record<string, string>) => {
    if (score >= 80) return c.success
    if (score >= 50) return c.warning
    return c.error
  },
}))

const emptyData = {
  greeting: {userDisplayName: 'Tester'},
  overview: {avgHealthScore: 0, healthAvailable: false, totalFlows: 0, totalSubflows: 0},
  tokenUsage: [], recentFlows: [],
  findings: {available: false, bySeverity: {}, byCategory: []},
  healthTrend: [], costByProvider: [], ruleFrequency: [], activity: [],
  complexity: [], security: {failedLogins24h: 0, credentialFindings: 0},
}

function renderHome() {
  return render(
    <ToastProvider>
      <HomeDashboard />
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  getHome.mockResolvedValue(emptyData)
  useAnalysisStore.setState({isAnalyzing: false})
})

describe('HomeDashboard', () => {
  it('fetches dashboard data on mount', async () => {
    renderHome()
    expect(getHome).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())
  })

  it('shows skeleton while loading', () => {
    getHome.mockReturnValue(new Promise(() => {}))
    const {container} = renderHome()
    expect(container.querySelector('.animate-pulse')).toBeTruthy()
  })

  it('shows error state with retry when fetch fails', async () => {
    getHome.mockRejectedValue(new Error('network down'))
    renderHome()
    expect(await screen.findByText(/Couldn't load your dashboard/)).toBeInTheDocument()
    expect(screen.getByText(/network down/)).toBeInTheDocument()
    expect(screen.getByText('Retry')).toBeInTheDocument()
  })

  it('re-fetches when analysis completes (isAnalyzing true→false)', async () => {
    renderHome()
    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(1))

    useAnalysisStore.setState({isAnalyzing: true})
    await new Promise(r => setTimeout(r, 10))
    useAnalysisStore.setState({isAnalyzing: false})

    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(2))
  })

  it('does NOT re-fetch when analysis starts (only on completion)', async () => {
    renderHome()
    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(1))

    useAnalysisStore.setState({isAnalyzing: true})

    await new Promise(r => setTimeout(r, 50))
    expect(getHome).toHaveBeenCalledTimes(1)
  })

  it('shows analyzing badge when isAnalyzing is true', async () => {
    renderHome()
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())

    useAnalysisStore.setState({isAnalyzing: true})
    await waitFor(() => expect(screen.getByText('Analyzing…')).toBeInTheDocument())

    useAnalysisStore.setState({isAnalyzing: false})
    await waitFor(() => expect(screen.queryByText('Analyzing…')).not.toBeInTheDocument())
  })
})
