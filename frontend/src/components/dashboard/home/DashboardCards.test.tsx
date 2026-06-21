import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'

// Mock useChartColors so we don't depend on CSS custom properties in jsdom
vi.mock('./useChartColors', () => ({
  useChartColors: () => ({
    success: '#22c55e',
    warning: '#eab308',
    error: '#ef4444',
    brand400: '#818cf8',
    brand500: '#5b61ef',
    brand600: '#4f46e5',
    surface3: '#26262d',
    borderStrong: '#3f3f47',
    textTertiary: '#71717a',
  }),
  healthColor: (score: number, c: Record<string, string>) => {
    if (score >= 80) return c.success
    if (score >= 50) return c.warning
    return c.error
  },
}))

// Mock recharts ResponsiveContainer to avoid jsdom sizing issues
vi.mock('recharts', async (importOriginal) => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => <div style={{width: 400, height: 200}}>{children}</div>,
  }
})

import {KPIStripCard} from './KPIStripCard'
import {SecurityPostureCard} from './SecurityPostureCard'
import {ActivityFeedCard} from './ActivityFeedCard'
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
    render(<KPIStripCard overview={mockOverview({healthAvailable: false, avgHealthScore: 0})} findings={mockFindings()} costByProvider={[]} />)
    expect(screen.getByText('not analyzed')).toBeTruthy()
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(1)
  })

  it('shows total findings with severity breakdown', () => {
    render(<KPIStripCard overview={mockOverview()} findings={mockFindings()} costByProvider={[]} />)
    expect(screen.getByText('45')).toBeTruthy() // 3+12+30
    expect(screen.getByText('3E · 12W · 30I')).toBeTruthy()
  })

  it('shows AI spend when providers have cost', () => {
    render(<KPIStripCard overview={mockOverview()} findings={mockFindings()} costByProvider={[{provider: 'claude', cost: 1.5}, {provider: 'openai', cost: 0.84}]} />)
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
    render(<SecurityPostureCard data={{failedLogins24h: 0, credentialFindings: 0}} />)
    expect(screen.getByText(/No security issues detected/)).toBeTruthy()
  })

  it('shows failed logins count when present', () => {
    render(<SecurityPostureCard data={{failedLogins24h: 7, credentialFindings: 0}} />)
    expect(screen.getByText('7')).toBeTruthy()
    expect(screen.getByText('Failed logins (24h)')).toBeTruthy()
  })

  it('shows credential findings when present', () => {
    render(<SecurityPostureCard data={{failedLogins24h: 0, credentialFindings: 3}} />)
    expect(screen.getByText('3')).toBeTruthy()
    expect(screen.getByText('Credential findings')).toBeTruthy()
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
