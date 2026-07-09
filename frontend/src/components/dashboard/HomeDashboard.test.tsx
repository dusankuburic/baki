import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useOrgStore} from '@/stores/orgStore'
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
    brand400: '#818cf8', brand500: '#5b61ef', brand600: '#4f46d5',
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
  isCloud: false,
  healthTrend: [], costByProvider: [], ruleFrequency: [], activity: [],
  complexity: [], security: {failedLogins24h: 0, credentialFindings: 0, lockedAccounts: 0},
  severityTrend: [], confidenceDist: {}, healthBuckets: [],
  fixability: {available: 0, total: 0, autoFixableRules: 0, totalRules: 0},
  workflow: {available: false, funnel: {}, mttrHours: 0, resolvedCount: 0, staleCount: 0},
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
  useAnalysisStore.getState().reset()
  useOrgStore.setState({organisations: [], activeOrgId: null})
})

describe('HomeDashboard', () => {
  it('fetches dashboard data on mount', async () => {
    renderHome()
    expect(getHome).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())
  })

  it('hides advanced cards in local mode (isCloud: false)', async () => {
    renderHome()
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())
    
    // Cards that should be hidden
    expect(screen.queryByText(/Health Score Trend/)).toBeNull()
    expect(screen.queryByText(/AI Cost by Provider/)).toBeNull()
    expect(screen.queryByText(/AI Token Usage/)).toBeNull()
    expect(screen.queryByText(/Recent Activity/)).toBeNull()
    expect(screen.queryByText(/Security Posture/)).toBeNull()

    // Cards that should still be visible
    expect(screen.getByText(/Findings by Rule/)).toBeInTheDocument()
    expect(screen.getByText(/Recent Flows/)).toBeInTheDocument()
  })

  it('shows advanced cards in cloud mode (isCloud: true)', async () => {
    getHome.mockResolvedValue({
      ...emptyData,
      isCloud: true,
      healthTrend: [{date: '2023-01-01', avgHealth: 80, flowCount: 1}],
      costByProvider: [{provider: 'openai', cost: 1.0, tokensIn: 100, tokensOut: 100}],
      activity: [{action: 'auth.login', createdAt: new Date().toISOString()}],
      security: {failedLogins24h: 0, credentialFindings: 0, lockedAccounts: 0},
    })
    renderHome()
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())
    
    expect(screen.getByText(/Health Score Trend/)).toBeInTheDocument()
    expect(screen.getByText(/AI Cost by Provider/)).toBeInTheDocument()
    expect(screen.getByText(/Recent Activity/)).toBeInTheDocument()
    expect(screen.getByText(/Security Posture/)).toBeInTheDocument()
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

  it('re-fetches when activeOrgId changes', async () => {
    renderHome()
    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(1))

    useOrgStore.setState({activeOrgId: 'org-2'})
    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(2))
  })

  it('shows error UI when org-switch fetch fails after prior success', async () => {
    getHome.mockResolvedValueOnce(emptyData)
    renderHome()
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())

    getHome.mockRejectedValueOnce(new Error('org B down'))
    useOrgStore.setState({activeOrgId: 'org-2'})

    expect(await screen.findByText(/Couldn't load your dashboard/)).toBeInTheDocument()
    expect(screen.getByText(/org B down/)).toBeInTheDocument()
    expect(screen.getByText('Retry')).toBeInTheDocument()
  })

  it('shows toast (not error UI) when background refresh fails after data loaded', async () => {
    renderHome()
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())

    getHome.mockRejectedValueOnce(new Error('blip'))
    useAnalysisStore.setState({isAnalyzing: true})
    await new Promise(r => setTimeout(r, 10))
    useAnalysisStore.setState({isAnalyzing: false})

    await waitFor(() => expect(getHome).toHaveBeenCalledTimes(2))
    await new Promise(r => setTimeout(r, 50))

    expect(screen.getByText(/Tester/)).toBeInTheDocument()
    expect(screen.queryByText(/Couldn't load your dashboard/)).not.toBeInTheDocument()
  })
})
