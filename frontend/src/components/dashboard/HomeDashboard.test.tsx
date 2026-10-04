import {describe, it, expect, vi, beforeEach, beforeAll} from 'vitest'
import {render, screen, waitFor, fireEvent} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useAnalyticsStore} from '@/stores/analyticsStore'
import {useOrgStore} from '@/stores/orgStore'
import {useSystemStore} from '@/stores/systemStore'
import {useUIStore} from '@/stores/uiStore'
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

vi.mock('recharts', async importOriginal => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => (
      <div style={{width: 400, height: 200}}>{children}</div>
    ),
  }
})

const emptyData = {
  greeting: {userDisplayName: 'Tester'},
  overview: {avgHealthScore: 0, healthAvailable: false, totalFlows: 0, totalSubflows: 0},
  tokenUsage: [],
  recentFlows: [],
  findings: {available: false, bySeverity: {}, byCategory: []},
  isCloud: false,
  healthTrend: [],
  costByProvider: [],
  ruleFrequency: [],
  activity: [],
  complexity: [],
  security: {available: true, failedLogins24h: 0, credentialFindings: 0, lockedAccounts: 0, admin: false},
  severityTrend: [],
  confidenceDist: {},
  healthBuckets: [],
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

// The chart cards are lazy (HomeDashboard imports them through this module);
// loading it once up front keeps the real recharts import's cost out of each
// test's wait budget.
beforeAll(async () => {
  await import('./home/HomeChartCards')
})

beforeEach(() => {
  vi.clearAllMocks()
  getHome.mockResolvedValue(emptyData)
  useAnalysisStore.getState().reset()
  useAnalyticsStore.getState().reset()
  useOrgStore.setState({organisations: [], activeOrgId: null})
  useSystemStore.setState({info: null, isLoaded: false})
  useUIStore.setState({mainPaneView: 'home'})
})

describe('HomeDashboard', () => {
  it('fetches dashboard data on mount', async () => {
    renderHome()
    expect(getHome).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())
  })

  it('hides the cloud-only cards in local mode (isCloud: false)', async () => {
    renderHome()
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())

    expect(screen.queryByText('Health trend')).toBeNull()
    expect(screen.queryByText('AI usage')).toBeNull()
    expect(screen.queryByText('Recent activity')).toBeNull()
    expect(screen.queryByText('Security posture')).toBeNull()
    expect(screen.queryByText('Open analytics')).toBeNull()

    expect(await screen.findByText('Top rules', {}, {timeout: 5000})).toBeInTheDocument()
    expect(screen.getByText('Recent flows')).toBeInTheDocument()
    expect(screen.getByText('Health distribution')).toBeInTheDocument()
    expect(screen.getByText('Flow complexity')).toBeInTheDocument()
  })

  it('shows the cloud cards and the analytics link in cloud mode (isCloud: true)', async () => {
    getHome.mockResolvedValue({
      ...emptyData,
      isCloud: true,
      healthTrend: [{date: '2023-01-01', avgHealth: 80, flowCount: 1}],
      costByProvider: [{provider: 'openai', cost: 1.0, tokensIn: 100, tokensOut: 100}],
      activity: [{action: 'auth.login', createdAt: new Date().toISOString()}],
    })
    renderHome()
    await waitFor(() => expect(screen.getByText(/Tester/)).toBeInTheDocument())

    expect(await screen.findByText('Health trend', {}, {timeout: 5000})).toBeInTheDocument()
    expect(screen.getByText('AI usage')).toBeInTheDocument()
    expect(screen.getByText('Recent activity')).toBeInTheDocument()
    expect(screen.getByText('Security posture')).toBeInTheDocument()
    expect(screen.getByText('Triage')).toBeInTheDocument()
    expect(screen.getByText('Open analytics')).toBeInTheDocument()
  })

  // Every cloud card links into the section of the Analytics page that
  // expands on it; the page picks the requested section up on mount.
  it('"Details" opens the analytics page at the card\'s section', async () => {
    getHome.mockResolvedValue({...emptyData, isCloud: true})
    renderHome()
    expect(await screen.findByText('Health trend', {}, {timeout: 5000})).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Open Health trend in analytics'}))
    expect(useUIStore.getState().mainPaneView).toBe('dashboard')
    expect(useAnalyticsStore.getState().pendingSection).toBe('analytics-trend')
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
