import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'

// Mock recharts ResponsiveContainer to avoid jsdom sizing issues.
vi.mock('recharts', async importOriginal => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => (
      <div style={{width: 400, height: 200}}>{children}</div>
    ),
  }
})

import {HomeKpiRow} from './HomeKpiRow'
import {FindingsByRuleCard} from './FindingsByRuleCard'
import {SecurityPostureCard} from './SecurityPostureCard'
import {ActivityFeedCard} from './ActivityFeedCard'
import {HealthDistributionCard} from './HealthDistributionCard'
import {TriageCard, formatDuration} from './TriageCard'
import {actionLabel} from './activity'
import type {DashboardOverview, DashboardFindingsAgg, ActivityEntry, Fixability} from '@/types'

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

const fixability: Fixability = {available: 5, total: 20, autoFixableRules: 11, totalRules: 29}

function kpis(over: Partial<React.ComponentProps<typeof HomeKpiRow>> = {}) {
  return render(
    <HomeKpiRow
      overview={mockOverview()}
      findings={mockFindings()}
      healthTrend={[]}
      severityTrend={[]}
      fixability={fixability}
      costByProvider={[]}
      {...over}
    />,
  )
}

describe('HomeKpiRow', () => {
  it('leads with the health score and its tier', () => {
    kpis()
    expect(screen.getByText('78')).toBeTruthy()
    expect(screen.getByText('Fair · of 100')).toBeTruthy()
  })

  it('says "not analyzed" instead of a zero score', () => {
    kpis({overview: mockOverview({healthAvailable: false, avgHealthScore: 0})})
    expect(screen.getByText('not analyzed')).toBeTruthy()
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(1)
  })

  it('shows total findings with the severity breakdown', () => {
    kpis()
    expect(screen.getByText('45')).toBeTruthy() // 3+12+30
    expect(screen.getByText('3 E · 12 W · 30 I')).toBeTruthy()
  })

  it('shows the fixable share as a meter', () => {
    kpis()
    expect(screen.getByText('5 of 20 findings')).toBeTruthy()
    const meter = screen.getByRole('meter')
    expect(meter.getAttribute('aria-valuenow')).toBe('5')
    expect(meter.getAttribute('aria-valuemax')).toBe('20')
  })

  it('shows AI spend only when a provider has cost', () => {
    kpis({
      costByProvider: [
        {provider: 'claude', cost: 1.5, tokensIn: 0, tokensOut: 0},
        {provider: 'openai', cost: 0.84, tokensIn: 0, tokensOut: 0},
      ],
    })
    expect(screen.getByText('$2.34')).toBeTruthy()
    expect(screen.getByText('2 providers')).toBeTruthy()
  })

  it('hides AI spend when costByProvider is empty', () => {
    kpis()
    expect(screen.queryByText(/AI spend/)).toBeNull()
  })

  it('shows flow count and subflow count', () => {
    kpis()
    expect(screen.getByText('12')).toBeTruthy()
    expect(screen.getByText('34 subflows')).toBeTruthy()
  })

  it('renders a sparkline from the trend series, skipping days with no flows', () => {
    const {container} = kpis({
      healthTrend: [
        {date: '2026-09-01', avgHealth: 70, flowCount: 1},
        {date: '2026-09-02', avgHealth: 0, flowCount: 0},
        {date: '2026-09-03', avgHealth: 80, flowCount: 1},
      ],
    })
    expect(container.querySelectorAll('[data-sparkline]').length).toBe(1)
  })
})

describe('SecurityPostureCard', () => {
  const base = {available: true, failedLogins24h: 0, credentialFindings: 0, lockedAccounts: 0, admin: false}

  it('shows the all-clear line and the tiles when nothing is wrong', () => {
    render(<SecurityPostureCard data={base} />)
    expect(screen.getByText(/No security issues detected/)).toBeTruthy()
    expect(screen.getByText('Failed logins · 24h')).toBeTruthy()
  })

  it('shows failed logins count when present', () => {
    render(<SecurityPostureCard data={{...base, failedLogins24h: 7}} />)
    expect(screen.getByText('7')).toBeTruthy()
    expect(screen.queryByText(/No security issues detected/)).toBeNull()
  })

  it('shows credential findings when present', () => {
    render(<SecurityPostureCard data={{...base, credentialFindings: 3}} />)
    expect(screen.getByText('3')).toBeTruthy()
    expect(screen.getByText('Credential findings')).toBeTruthy()
  })

  // A failed or stubbed source arrives as all zeros; rendering that as an
  // all-clear told admins nothing was wrong when nothing had been checked.
  it('shows a placeholder, not the all-clear, when the posture is unavailable', () => {
    render(<SecurityPostureCard data={{...base, available: false}} />)
    expect(screen.queryByText(/No security issues detected/)).toBeNull()
    expect(screen.getByText(/Security posture is unavailable/)).toBeTruthy()
  })

  // The locked-account count is instance-wide: only admins get the tile.
  it('shows locked accounts to admins only', () => {
    const {rerender} = render(<SecurityPostureCard data={{...base, lockedAccounts: 2}} />)
    expect(screen.queryByText('Locked accounts')).toBeNull()
    rerender(<SecurityPostureCard data={{...base, lockedAccounts: 2, admin: true}} />)
    expect(screen.getByText('Locked accounts')).toBeTruthy()
    expect(screen.getByText('2')).toBeTruthy()
    expect(screen.queryByText(/No security issues detected/)).toBeNull()
  })
})

describe('ActivityFeedCard', () => {
  const mockActivity: ActivityEntry[] = [
    {action: 'auth.login', createdAt: new Date().toISOString()},
    {action: 'flow.analyze', flowName: 'MyFlow', createdAt: new Date(Date.now() - 3600_000).toISOString()},
    {action: 'flow.save', flowName: 'TestFlow', createdAt: new Date(Date.now() - 86400_000).toISOString()},
    {action: 'flow.baseline_set', createdAt: new Date(Date.now() - 86400_000).toISOString()},
  ]

  it('shows placeholder when empty', () => {
    render(<ActivityFeedCard data={[]} />)
    expect(screen.getByText(/No activity recorded/)).toBeTruthy()
  })

  it('renders activity items with human labels', () => {
    render(<ActivityFeedCard data={mockActivity} />)
    expect(screen.getByText('Signed in')).toBeTruthy()
    expect(screen.getByText('Analyzed flow')).toBeTruthy()
    expect(screen.getByText('Saved flow')).toBeTruthy()
    expect(screen.getByText('Set a baseline')).toBeTruthy()
  })

  it('shows flow name when present', () => {
    render(<ActivityFeedCard data={mockActivity} />)
    expect(screen.getByText(/MyFlow/)).toBeTruthy()
  })

  it('humanises an action the table does not know', () => {
    expect(actionLabel({}, 'org.knowledge_reindex')).toBe('Org knowledge reindex')
    expect(actionLabel({auth_login: 'Signed in'}, 'auth.login')).toBe('Signed in')
  })
})

describe('TriageCard', () => {
  it('shows placeholder when workflow is unavailable (local mode / no triage)', () => {
    render(<TriageCard data={{available: false, funnel: {}, mttrHours: 0, resolvedCount: 0, staleCount: 0}} />)
    expect(screen.getByText(/Triage findings/)).toBeTruthy()
  })

  it('renders status distribution, MTTR, and stale count when available', () => {
    render(
      <TriageCard
        data={{
          available: true,
          funnel: {open: 10, acknowledged: 3, in_progress: 2, resolved: 8, suppressed: 1},
          mttrHours: 16.5,
          resolvedCount: 8,
          staleCount: 4,
        }}
      />,
    )
    expect(screen.getByRole('group', {name: 'Triage'})).toBeTruthy()
    expect(screen.getByText('16.5h')).toBeTruthy()
    expect(screen.getByText('over 8 resolved findings')).toBeTruthy()
    expect(screen.getByText('4')).toBeTruthy()
    expect(screen.getByText('untouched for 14+ days')).toBeTruthy()
  })

  it('formats durations and shows an em-dash MTTR when nothing has been resolved', () => {
    expect(formatDuration(0)).toBe('—')
    expect(formatDuration(0.5)).toBe('30m')
    expect(formatDuration(30)).toBe('1d 6h')
    render(<TriageCard data={{available: true, funnel: {open: 5}, mttrHours: 0, resolvedCount: 0, staleCount: 0}} />)
    expect(screen.getByText('—')).toBeTruthy()
    expect(screen.getByText('nothing resolved yet')).toBeTruthy()
  })
})

describe('FindingsByRuleCard', () => {
  it('shows placeholder when empty', () => {
    render(<FindingsByRuleCard data={[]} />)
    expect(screen.getByText(/No rule data/)).toBeTruthy()
  })

  it('renders the chart with an accessible name and a table twin with severity chips', () => {
    render(
      <FindingsByRuleCard
        data={[
          {rule: 'hardcoded-credential', count: 5, topSeverity: 'error'},
          {rule: 'missing-timeout', count: 2, topSeverity: 'warning'},
        ]}
      />,
    )
    expect(screen.getByRole('group', {name: 'Top rules'})).toBeTruthy()
    fireEvent.click(screen.getByRole('radio', {name: 'Table'}))
    expect(screen.getByRole('table')).toBeTruthy()
    expect(screen.getByText('Error')).toBeTruthy()
    expect(screen.getByText('Warning')).toBeTruthy()
    expect(screen.getByText('hardcoded-credential')).toBeTruthy()
  })
})

describe('HealthDistributionCard', () => {
  it('shows placeholder when no flows analyzed', () => {
    render(<HealthDistributionCard data={[]} />)
    expect(screen.getByText(/distributed/)).toBeTruthy()
  })

  it('renders the chart when data is present', () => {
    render(
      <HealthDistributionCard
        data={[
          {label: '0-20', lo: 0, hi: 20, count: 1},
          {label: '80-100', lo: 80, hi: 100, count: 3},
        ]}
      />,
    )
    expect(screen.getByRole('group', {name: 'Health distribution'})).toBeTruthy()
    expect(screen.queryByText(/distributed/)).toBeNull()
  })
})
