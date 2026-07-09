import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'

// Mock useChartColors so ComplexityScatter doesn't touch CSS vars in jsdom.
vi.mock('../dashboard/home/useChartColors', () => ({
  useChartColors: () => ({
    success: '#22c55e', warning: '#eab308', error: '#ef4444', info: '#3b82f6',
    brand400: '#818cf8', brand500: '#5b61ef', brand600: '#4f46e5',
    surface3: '#26262d', borderStrong: '#3f3f47',
    textSecondary: '#a1a1aa', textTertiary: '#71717a',
  }),
}))

// Mock recharts ResponsiveContainer to avoid jsdom sizing issues.
vi.mock('recharts', async (importOriginal) => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => <div style={{width: 400, height: 200}}>{children}</div>,
  }
})

import {ComplexityScatter, ImpactEffortMatrix} from './ComplexityCharts'
import type {Finding, SubflowMetrics} from '@/types'

const finding = (over: Partial<Finding> = {}): Finding => ({
  id: 'F-1',
  ruleId: 'r',
  severity: 'warning',
  title: 't',
  description: 'd',
  blockId: 'b',
  subflowId: 's',
  ...over,
})

const subflow = (over: Partial<SubflowMetrics> = {}): SubflowMetrics => ({
  subflowId: 'sf-1',
  subflowName: 'Main',
  blockCount: 10,
  cyclomaticComplexity: 4,
  cognitiveComplexity: 8,
  maxNestingDepth: 2,
  variableCount: 5,
  fanIn: 0,
  fanOut: 1,
  ...over,
})

describe('ComplexityScatter', () => {
  it('renders nothing when there are no subflows', () => {
    const {container} = render(<ComplexityScatter subflows={[]} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders the chart with title and accessible name when populated', () => {
    render(<ComplexityScatter subflows={[
      subflow(),
      subflow({subflowId: 'sf-2', subflowName: 'Hot', cyclomaticComplexity: 12, cognitiveComplexity: 35, maxNestingDepth: 5}),
    ]} />)
    expect(screen.getByText('Complexity Map')).toBeTruthy()
    expect(screen.getByRole('img', {name: /scatter plot of subflows/i})).toBeTruthy()
  })
})

describe('ImpactEffortMatrix', () => {
  it('renders nothing when there are no findings', () => {
    const {container} = render(<ImpactEffortMatrix findings={[]} />)
    expect(container.firstChild).toBeNull()
  })

  it('sorts findings into the four priority quadrants', () => {
    const findings: Finding[] = [
      finding({id: '1', severity: 'error', autoFix: 'set-timeout'}),   // Quick Win
      finding({id: '2', severity: 'error', autoFix: 'set-timeout'}),   // Quick Win
      finding({id: '3', severity: 'error'}),                           // Strategic (manual error)
      finding({id: '4', severity: 'warning', autoFix: 'insert-close'}),// Easy Cleanup
      finding({id: '5', severity: 'info'}),                            // Backlog (manual lower sev)
      finding({id: '6', severity: 'warning'}),                         // Backlog
    ]
    const {container} = render(<ImpactEffortMatrix findings={findings} />)

    // All four quadrant labels render (they're unique strings).
    expect(screen.getByText('Quick Wins')).toBeTruthy()
    expect(screen.getByText('Strategic')).toBeTruthy()
    expect(screen.getByText('Easy Cleanup')).toBeTruthy()
    expect(screen.getByText('Backlog')).toBeTruthy()

    // Counts: Quick Wins=2, Strategic=1, Easy Cleanup=1, Backlog=2.
    // Aggregate the quadrant count elements and assert the multiset, avoiding
    // getByText collisions where two quadrants share a count.
    const counts = Array.from(container.querySelectorAll('.text-xl')).map(el => Number(el.textContent))
    expect(counts).toEqual(expect.arrayContaining([2, 2, 1, 1]))
    expect(counts.reduce((a, b) => a + b, 0)).toBe(6)
  })

  it('counts info/warning as lower severity (not errors)', () => {
    const findings: Finding[] = [
      finding({id: '1', severity: 'warning', autoFix: 'wrap-error-handler'}),
      finding({id: '2', severity: 'info', autoFix: 'wrap-error-handler'}),
    ]
    const {container} = render(<ImpactEffortMatrix findings={findings} />)
    // Both land in Easy Cleanup, none in Quick Wins.
    expect(screen.getByText('Easy Cleanup')).toBeTruthy()
    const counts = container.querySelectorAll('.text-xl')
    // Two findings → Easy Cleanup quadrant shows 2.
    expect(Array.from(counts).some(el => el.textContent === '2')).toBe(true)
    expect(Array.from(counts).some(el => el.textContent === '0')).toBe(true)
  })
})
