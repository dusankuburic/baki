import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'

// Mock useChartColors so we don't depend on CSS custom properties in jsdom
vi.mock('./useChartColors', () => ({
  useChartColors: () => ({
    success: '#22c55e',
    warning: '#eab308',
    error: '#ef4444',
    info: '#3b82f6',
    brand400: '#818cf8',
    brand500: '#5b61ef',
    brand600: '#4f46e5',
    surface3: '#26262d',
    borderStrong: '#3f3f47',
    textSecondary: '#a1a1aa',
    textTertiary: '#71717a',
  }),
  healthColor: (score: number, c: Record<string, string>) => {
    if (score >= 80) return c.success
    if (score >= 50) return c.warning
    return c.error
  },
}))

// Mock recharts ResponsiveContainer to avoid jsdom sizing issues
vi.mock('recharts', async importOriginal => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => (
      <div style={{width: 400, height: 200}}>{children}</div>
    ),
  }
})

import {KPIStripCard} from './KPIStripCard'
import {RuleFrequencyCard} from './RuleFrequencyCard'
import {SecurityPostureCard} from './SecurityPostureCard'
import {ActivityFeedCard} from './ActivityFeedCard'
import {SeverityTrendCard} from './SeverityTrendCard'
import {ConfidenceDonutCard} from './ConfidenceDonutCard'
import {HealthDistributionCard} from './HealthDistributionCard'
import {FixabilityCard} from './FixabilityCard'
import {WorkflowFunnelCard} from './WorkflowFunnelCard'
import type {DashboardOverview, DashboardFindingsAgg, ActivityEntry} from '@/types'

const mockOverview = (over: Partial<DashboardOverview> = {}): DashboardOverview => ({
  avgHealthScore: 78,
  healthAvailable: true,
  totalFlows: 12,
  totalSubflows: 34,
  ...over,
})

const mockFindings = (over: Partial<DashboardFindingsAgg> = {}): DashboardFindingsAgg => ({
  available: true,
  bySeverity: {error: 3, warning: 12, info: 30},
  byCategory: [],
  ...over,
})

describe('KPIStripCard', () => {
  it('renders health score when available', () => {
    render(<KPIStripCard overview={mockOverview()} findings={mockFindings()} costByProvider={[]} />)
    expect(screen.getByText('78')).toBeTruthy()
    expect(screen.getByText('of 100')).toBeTruthy()
  })

  it('shows dash when health not available', () => {
    render(
      <KPIStripCard
        overview={mockOverview({healthAvailable: false, avgHealthScore: 0})}
        findings={mockFindings()}
        costByProvider={[]}
      />,
    )
    expect(screen.getByText('not analyzed')).toBeTruthy()
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(1)
  })

  it('shows total findings with severity breakdown', () => {
    render(<KPIStripCard overview={mockOverview()} findings={mockFindings()} costByProvider={[]} />)
    expect(screen.getByText('45')).toBeTruthy() // 3+12+30
    expect(screen.getByText('3E · 12W · 30I')).toBeTruthy()
  })

  it('shows AI spend when providers have cost', () => {
    render(
      <KPIStripCard
        overview={mockOverview()}
        findings={mockFindings()}
        costByProvider={[
          {provider: 'claude', cost: 1.5},
          {provider: 'openai', cost: 0.84},
        ]}
      />,
    )
    expect(screen.getByText('$2.34')).toBeTruthy()
    expect(screen.getByText('2 providers')).toBeTruthy()
  })

  it('hides AI spend KPI when costByProvider is empty', () => {
    render(<KPIStripCard overview={mockOverview()} findings={mockFindings()} costByProvider={[]} />)
    expect(screen.queryByText(/AI Spend/)).toBeNull()
  })

  it('shows flow count and subflow count', () => {
    render(<KPIStripCard overview={mockOverview()} findings={mockFindings()} costByProvider={[]} />)
    expect(screen.getByText('12')).toBeTruthy()
    expect(screen.getByText('34 subflows')).toBeTruthy()
  })
})

describe('SecurityPostureCard', () => {
  it('shows all-clear when no issues', () => {
    render(<SecurityPostureCard data={{failedLogins24h: 0, credentialFindings: 0, lockedAccounts: 0}} />)
    expect(screen.getByText(/No security issues detected/)).toBeTruthy()
  })

  it('shows failed logins count when present', () => {
    render(<SecurityPostureCard data={{failedLogins24h: 7, credentialFindings: 0, lockedAccounts: 0}} />)
    expect(screen.getByText('7')).toBeTruthy()
    expect(screen.getByText('Failed logins (24h)')).toBeTruthy()
  })

  it('shows credential findings when present', () => {
    render(<SecurityPostureCard data={{failedLogins24h: 0, credentialFindings: 3, lockedAccounts: 0}} />)
    expect(screen.getByText('3')).toBeTruthy()
    expect(screen.getByText('Credential findings')).toBeTruthy()
  })

  it('shows locked accounts and loses the all-clear state', () => {
    render(<SecurityPostureCard data={{failedLogins24h: 0, credentialFindings: 0, lockedAccounts: 2}} />)
    expect(screen.getByText('2')).toBeTruthy()
    expect(screen.getByText('Locked accounts')).toBeTruthy()
    expect(screen.queryByText(/No security issues detected/)).toBeNull()
  })
})

describe('ActivityFeedCard', () => {
  const mockActivity: ActivityEntry[] = [
    {action: 'auth.login', createdAt: new Date().toISOString()},
    {action: 'flow.analyze', flowName: 'MyFlow', createdAt: new Date(Date.now() - 3600_000).toISOString()},
    {action: 'flow.save', flowName: 'TestFlow', createdAt: new Date(Date.now() - 86400_000).toISOString()},
  ]

  it('shows placeholder when empty', () => {
    render(<ActivityFeedCard data={[]} />)
    expect(screen.getByText(/No activity recorded/)).toBeTruthy()
  })

  it('renders activity items with correct labels', () => {
    render(<ActivityFeedCard data={mockActivity} />)
    expect(screen.getByText('Signed in')).toBeTruthy()
    expect(screen.getByText('Analyzed flow')).toBeTruthy()
    expect(screen.getByText('Saved flow')).toBeTruthy()
  })

  it('shows flow name when present', () => {
    render(<ActivityFeedCard data={mockActivity} />)
    expect(screen.getByText(/MyFlow/)).toBeTruthy()
  })

  it('shows relative timestamps', () => {
    render(<ActivityFeedCard data={mockActivity} />)
    expect(screen.getByText('just now')).toBeTruthy()
    expect(screen.getByText('1h ago')).toBeTruthy()
    expect(screen.getByText('1d ago')).toBeTruthy()
  })
})

describe('SeverityTrendCard', () => {
  it('shows placeholder when empty', () => {
    render(<SeverityTrendCard data={[]} />)
    expect(screen.getByText(/multiple sessions/)).toBeTruthy()
  })

  it('shows placeholder when all days are zero', () => {
    render(<SeverityTrendCard data={[{date: '2026-01-01', errors: 0, warnings: 0, info: 0}]} />)
    expect(screen.getByText(/No findings in the last 30 days/)).toBeTruthy()
  })

  it('renders the chart (not a placeholder) for a single data point', () => {
    render(<SeverityTrendCard data={[{date: '2026-01-01', errors: 2, warnings: 1, info: 0}]} />)
    expect(screen.getByText('Severity Trend · 30d')).toBeTruthy()
    expect(screen.queryByText(/multiple sessions/)).toBeNull()
    expect(screen.queryByText(/No findings in the last 30 days/)).toBeNull()
    expect(screen.getByRole('img', {name: /stacked area chart/i})).toBeTruthy()
  })
})

describe('RuleFrequencyCard', () => {
  it('shows placeholder when empty', () => {
    render(<RuleFrequencyCard data={[]} />)
    expect(screen.getByText(/No rule frequency data/)).toBeTruthy()
  })

  it('renders with server-provided topSeverity', () => {
    render(
      <RuleFrequencyCard
        data={[
          {rule: 'hardcoded-credential', count: 5, topSeverity: 'error'},
          {rule: 'missing-timeout', count: 2, topSeverity: 'warning'},
        ]}
      />,
    )
    expect(screen.getByRole('img', {name: /most frequent finding rules/i})).toBeTruthy()
  })

  it('renders with the neutral info tint when topSeverity is absent', () => {
    render(
      <RuleFrequencyCard
        data={[
          {rule: 'missing-timeout', count: 4},
          {rule: 'some-unknown-rule', count: 1},
        ]}
      />,
    )
    expect(screen.getByRole('img', {name: /most frequent finding rules/i})).toBeTruthy()
  })
})

describe('ConfidenceDonutCard', () => {
  it('shows placeholder when there are no findings', () => {
    render(<ConfidenceDonutCard confidence={{}} />)
    expect(screen.getByText(/how much to trust/)).toBeTruthy()
  })

  it('renders total findings count in the center', () => {
    render(<ConfidenceDonutCard confidence={{high: 4, medium: 6, low: 2}} />)
    expect(screen.getByText('12')).toBeTruthy()
    expect(screen.getByText('findings')).toBeTruthy()
  })
})

describe('HealthDistributionCard', () => {
  it('shows placeholder when no flows analyzed', () => {
    render(<HealthDistributionCard data={[]} />)
    expect(screen.getByText(/distributed/)).toBeTruthy()
  })

  it('renders bucket labels when data present', () => {
    render(
      <HealthDistributionCard
        data={[
          {label: '0-20', lo: 0, hi: 20, count: 1},
          {label: '80-100', lo: 80, hi: 100, count: 3},
        ]}
      />,
    )
    // Data present ⇒ title renders and the placeholder is absent.
    expect(screen.getByText('Health Distribution')).toBeTruthy()
    expect(screen.queryByText(/distributed/)).toBeNull()
  })
})

describe('FixabilityCard', () => {
  it('shows placeholder plus the always-known catalog ratio when there are no findings', () => {
    render(<FixabilityCard data={{available: 0, total: 0, autoFixableRules: 11, totalRules: 29}} />)
    expect(screen.getByText(/one-click fixable/)).toBeTruthy()
    expect(screen.getByText('11/29')).toBeTruthy()
    expect(screen.getByText(/rules ship a fixer · 38%/)).toBeTruthy()
  })

  it('renders the fixable percentage and catalog counts', () => {
    render(<FixabilityCard data={{available: 5, total: 20, autoFixableRules: 11, totalRules: 29}} />)
    // 5/20 = 25%
    expect(screen.getByText('25%')).toBeTruthy()
    expect(screen.getByText('5/20')).toBeTruthy()
    expect(screen.getByText('11/29')).toBeTruthy()
  })
})

describe('WorkflowFunnelCard', () => {
  it('shows placeholder when workflow is unavailable (local mode / no triage)', () => {
    render(<WorkflowFunnelCard data={{available: false, funnel: {}, mttrHours: 0, resolvedCount: 0, staleCount: 0}} />)
    expect(screen.getByText(/Triage findings/)).toBeTruthy()
  })

  it('renders status distribution, MTTR, and stale count when available', () => {
    render(
      <WorkflowFunnelCard
        data={{
          available: true,
          funnel: {open: 10, acknowledged: 3, in_progress: 2, resolved: 8, suppressed: 1},
          mttrHours: 16.5,
          resolvedCount: 8,
          staleCount: 4,
        }}
      />,
    )
    // Status labels render.
    expect(screen.getByText('Open')).toBeTruthy()
    expect(screen.getByText('Resolved')).toBeTruthy()
    // MTTR (16.5h) + resolved count.
    expect(screen.getByText('16.5h')).toBeTruthy()
    expect(screen.getByText(/MTTR · 8 resolved/)).toBeTruthy()
    // Stale count surfaces.
    expect(screen.getByText('4')).toBeTruthy()
    expect(screen.getByText(/stale/)).toBeTruthy()
  })

  it('shows an em-dash MTTR when nothing has been resolved', () => {
    render(
      <WorkflowFunnelCard
        data={{
          available: true,
          funnel: {open: 5},
          mttrHours: 0,
          resolvedCount: 0,
          staleCount: 0,
        }}
      />,
    )
    expect(screen.getByText('—')).toBeTruthy()
  })
})
