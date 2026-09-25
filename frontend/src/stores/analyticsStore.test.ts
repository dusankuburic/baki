import {describe, it, expect, beforeEach} from 'vitest'
import {
  useAnalyticsStore,
  DEFAULT_FILTERS,
  toQuery,
  customRangeIssue,
  hasActiveFilters,
  todayISO,
} from './analyticsStore'

beforeEach(() => {
  useAnalyticsStore.getState().reset()
  useAnalyticsStore.setState({nonce: 0})
})

describe('analyticsStore', () => {
  it('starts on the last 30 days, every scope, comparing', () => {
    const q = new URLSearchParams(toQuery(DEFAULT_FILTERS, 'Europe/Belgrade'))
    expect(q.get('range')).toBe('30d')
    expect(q.get('scope')).toBe('all')
    expect(q.get('compare')).toBe('1')
    expect(q.get('tz')).toBe('Europe/Belgrade')
    expect(q.has('from')).toBe(false)
    expect(q.has('orgId')).toBe(false)
    expect(q.has('flows')).toBe(false)
    expect(hasActiveFilters(DEFAULT_FILTERS)).toBe(false)
  })

  it('renders lists deduplicated and sorted so equal filters share a URL', () => {
    const s = useAnalyticsStore.getState()
    s.toggleRule('b')
    s.toggleRule('a')
    s.toggleRule('b')
    s.toggleRule('b')
    s.toggleSeverity('warning')
    s.toggleSeverity('error')
    s.toggleCategory('Best Practices')
    s.setFlows(['f2', 'f1', 'f2'])
    const q = new URLSearchParams(toQuery(useAnalyticsStore.getState().filters, 'UTC'))
    expect(q.get('rule')).toBe('a,b')
    expect(q.get('severity')).toBe('error,warning')
    expect(q.get('category')).toBe('Best Practices')
    expect(q.get('flows')).toBe('f1,f2')
    expect(toQuery(useAnalyticsStore.getState().filters, 'UTC')).toBe(
      toQuery({...useAnalyticsStore.getState().filters, flows: ['f1', 'f2', 'f1']}, 'UTC'),
    )
    expect(hasActiveFilters(useAnalyticsStore.getState().filters)).toBe(true)
  })

  it('keeps orgId only for the org scope and drops the flow filter on a scope change', () => {
    const s = useAnalyticsStore.getState()
    s.setFlows(['f1'])
    s.setScope('org', 'acme')
    let f = useAnalyticsStore.getState().filters
    expect(f.orgId).toBe('acme')
    expect(f.flows).toEqual([])
    expect(new URLSearchParams(toQuery(f, 'UTC')).get('orgId')).toBe('acme')
    s.setScope('personal', 'acme')
    f = useAnalyticsStore.getState().filters
    expect(f.orgId).toBeNull()
    expect(new URLSearchParams(toQuery(f, 'UTC')).has('orgId')).toBe(false)
  })

  it('serialises a custom range and clears it on a preset', () => {
    const s = useAnalyticsStore.getState()
    s.setCustomRange('2026-09-01', '2026-09-10')
    let q = new URLSearchParams(toQuery(useAnalyticsStore.getState().filters, 'UTC'))
    expect(q.get('range')).toBe('custom')
    expect(q.get('from')).toBe('2026-09-01')
    expect(q.get('to')).toBe('2026-09-10')
    s.setPreset('7d')
    q = new URLSearchParams(toQuery(useAnalyticsStore.getState().filters, 'UTC'))
    expect(q.get('range')).toBe('7d')
    expect(q.has('from')).toBe(false)
  })

  it('validates a custom range like the server does', () => {
    const today = '2026-09-24'
    expect(customRangeIssue('2026-09-01', '2026-09-10', today)).toBeNull()
    expect(customRangeIssue('2026-09-24', '2026-09-24', today)).toBeNull()
    expect(customRangeIssue('', '2026-09-10', today)).toBe('missing')
    expect(customRangeIssue('2026-09-10', '2026-09-01', today)).toBe('order')
    expect(customRangeIssue('2026-09-01', '2026-09-25', today)).toBe('future')
    expect(customRangeIssue('2025-01-01', '2026-09-01', today)).toBe('span')
    expect(customRangeIssue('2025-09-24', '2026-09-24', today)).toBeNull()
    expect(todayISO(new Date(2026, 8, 5))).toBe('2026-09-05')
  })

  it('hands a requested section over exactly once', () => {
    const s = useAnalyticsStore.getState()
    expect(s.consumeSection()).toBeNull()
    s.requestSection('analytics-findings')
    expect(useAnalyticsStore.getState().pendingSection).toBe('analytics-findings')
    expect(useAnalyticsStore.getState().consumeSection()).toBe('analytics-findings')
    expect(useAnalyticsStore.getState().consumeSection()).toBeNull()
  })

  it('reset restores the defaults; refresh only bumps the nonce', () => {
    const s = useAnalyticsStore.getState()
    s.toggleSeverity('error')
    s.setCompare(false)
    s.requestSection('x')
    s.refresh()
    expect(useAnalyticsStore.getState().nonce).toBe(1)
    useAnalyticsStore.getState().reset()
    const st = useAnalyticsStore.getState()
    expect(st.filters).toEqual(DEFAULT_FILTERS)
    expect(st.pendingSection).toBeNull()
    expect(st.nonce).toBe(1)
  })
})
