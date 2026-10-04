import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useAnalyticsStore} from '@/stores/analyticsStore'
import {useOrgStore} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import HomeDashboard from './HomeDashboard'

// The charts chunk never arrives: the recharts-backed cards load through
// `./home/HomeChartCards`, and this factory keeps that import pending for the
// whole file. Everything that is NOT a chart must paint regardless.
vi.mock('./home/HomeChartCards', () => new Promise<never>(() => {}))

const getHome = vi.fn()

vi.mock('@/api', () => ({
  dashboardApi: {
    getHome: (...a: unknown[]) => getHome(...a),
  },
  libraryApi: {
    getContent: vi.fn(),
  },
}))

const data = {
  greeting: {userDisplayName: 'Tester'},
  overview: {avgHealthScore: 72, healthAvailable: true, totalFlows: 3, totalSubflows: 5},
  tokenUsage: [],
  recentFlows: [{id: 'r1', name: 'Billing flow', healthScore: 80, updatedAt: new Date().toISOString()}],
  findings: {available: true, bySeverity: {error: 2}, byCategory: [{category: 'security', count: 2}]},
  isCloud: false,
  healthTrend: [{date: '2023-01-01', avgHealth: 80, flowCount: 1}],
  costByProvider: [],
  ruleFrequency: [{ruleId: 'r', ruleName: 'Rule', count: 1, severity: 'error'}],
  activity: [{action: 'auth.login', createdAt: new Date().toISOString()}],
  complexity: [{flowId: 'r1', flowName: 'Billing flow', blockCount: 10, findingCount: 2, healthScore: 80}],
  security: {available: true, failedLogins24h: 0, credentialFindings: 0, lockedAccounts: 0, admin: false},
  severityTrend: [],
  confidenceDist: {},
  healthBuckets: [{bucket: '80-100', count: 1}],
  fixability: {available: 1, total: 2, autoFixableRules: 1, totalRules: 2},
  workflow: {available: true, funnel: {open: 1}, mttrHours: 1, resolvedCount: 1, staleCount: 0},
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
  getHome.mockResolvedValue(data)
  useAnalysisStore.getState().reset()
  useAnalyticsStore.getState().reset()
  useOrgStore.setState({organisations: [], activeOrgId: null})
  useUIStore.setState({mainPaneView: 'home'})
})

describe('HomeDashboard with the charts chunk still loading', () => {
  it('paints the header, KPI row and recent flows (local mode), with a skeleton per chart card', async () => {
    renderHome()
    expect(await screen.findByText(/Tester/)).toBeInTheDocument()
    // Chart-free content is on screen...
    expect(screen.getByText('Recent flows')).toBeInTheDocument()
    expect(screen.getByText('Billing flow')).toBeInTheDocument()
    // ...the chart cards are not, and each holds its slot with a skeleton:
    // findings by category, top rules, health distribution, flow complexity.
    expect(screen.queryByText('Top rules')).toBeNull()
    expect(screen.queryByText('Health distribution')).toBeNull()
    expect(screen.queryByText('Flow complexity')).toBeNull()
    const skeletons = screen.getAllByTestId('home-card-skeleton')
    expect(skeletons).toHaveLength(4)
    expect(skeletons.map(s => s.className)).toEqual(
      expect.arrayContaining([
        expect.stringContaining('home-span-5'),
        expect.stringContaining('home-span-7'),
        expect.stringContaining('home-span-12'),
      ]),
    )
  })

  it('paints recent flows, activity and security (cloud mode), with a skeleton per chart card', async () => {
    getHome.mockResolvedValue({...data, isCloud: true})
    renderHome()
    expect(await screen.findByText(/Tester/)).toBeInTheDocument()
    expect(screen.getByText('Recent flows')).toBeInTheDocument()
    expect(screen.getByText('Billing flow')).toBeInTheDocument()
    expect(screen.getByText('Recent activity')).toBeInTheDocument()
    expect(screen.getByText('Security posture')).toBeInTheDocument()
    expect(screen.getByText('Open analytics')).toBeInTheDocument()
    // Health trend, findings by category, top rules, triage, AI usage.
    expect(screen.queryByText('Health trend')).toBeNull()
    expect(screen.queryByText('AI usage')).toBeNull()
    expect(screen.queryByText('Triage')).toBeNull()
    expect(screen.getAllByTestId('home-card-skeleton')).toHaveLength(5)
  })
})
