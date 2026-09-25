import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'

// The Analytics page's filters. In-memory only: the page starts on the last
// 30 days, every scope, no finding filters — the same view for everyone,
// which is the one the numbers on Home summarise.

export type RangePreset = '7d' | '30d' | '90d' | 'custom'
export type AnalyticsScope = 'all' | 'personal' | 'org'

export interface AnalyticsFilters {
  preset: RangePreset
  /** Inclusive local days (YYYY-MM-DD); only read when preset is 'custom'. */
  from: string
  to: string
  scope: AnalyticsScope
  /** The organisation when scope is 'org'. */
  orgId: string | null
  flows: string[]
  severity: string[]
  category: string[]
  rule: string[]
  compare: boolean
}

export const DEFAULT_FILTERS: AnalyticsFilters = {
  preset: '30d',
  from: '',
  to: '',
  scope: 'all',
  orgId: null,
  flows: [],
  severity: [],
  category: [],
  rule: [],
  compare: true,
}

export const MAX_CUSTOM_SPAN_DAYS = 366

interface AnalyticsState {
  filters: AnalyticsFilters
  /** A section Home asked the page to scroll to once it has rendered. */
  pendingSection: string | null
  /** Bumped to refetch the current filters (retry, analysis finished). */
  nonce: number
  setPreset: (preset: Exclude<RangePreset, 'custom'>) => void
  setCustomRange: (from: string, to: string) => void
  setScope: (scope: AnalyticsScope, orgId?: string | null) => void
  setFlows: (ids: string[]) => void
  toggleFlow: (id: string) => void
  toggleSeverity: (severity: string) => void
  toggleCategory: (category: string) => void
  toggleRule: (rule: string) => void
  clearFindingFilters: () => void
  setCompare: (compare: boolean) => void
  reset: () => void
  requestSection: (id: string) => void
  consumeSection: () => string | null
  refresh: () => void
}

function toggled(list: string[], value: string): string[] {
  return list.includes(value) ? list.filter(v => v !== value) : [...list, value]
}

export const useAnalyticsStore = create<AnalyticsState>()((set, get) => ({
  filters: DEFAULT_FILTERS,
  pendingSection: null,
  nonce: 0,
  setPreset: preset => set(s => ({filters: {...s.filters, preset, from: '', to: ''}})),
  setCustomRange: (from, to) => set(s => ({filters: {...s.filters, preset: 'custom', from, to}})),
  setScope: (scope, orgId = null) =>
    set(s => ({filters: {...s.filters, scope, orgId: scope === 'org' ? orgId : null, flows: []}})),
  setFlows: ids => set(s => ({filters: {...s.filters, flows: [...new Set(ids)]}})),
  toggleFlow: id => set(s => ({filters: {...s.filters, flows: toggled(s.filters.flows, id)}})),
  toggleSeverity: severity => set(s => ({filters: {...s.filters, severity: toggled(s.filters.severity, severity)}})),
  toggleCategory: category => set(s => ({filters: {...s.filters, category: toggled(s.filters.category, category)}})),
  toggleRule: rule => set(s => ({filters: {...s.filters, rule: toggled(s.filters.rule, rule)}})),
  clearFindingFilters: () => set(s => ({filters: {...s.filters, severity: [], category: [], rule: []}})),
  setCompare: compare => set(s => ({filters: {...s.filters, compare}})),
  reset: () => set({filters: DEFAULT_FILTERS, pendingSection: null}),
  requestSection: id => set({pendingSection: id}),
  consumeSection: () => {
    const id = get().pendingSection
    if (id) set({pendingSection: null})
    return id
  },
  refresh: () => set(s => ({nonce: s.nonce + 1})),
}))

registerStoreReset(() => useAnalyticsStore.getState().reset())

export function localTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}

// Today's local calendar day as YYYY-MM-DD (the <input type="date"> format).
export function todayISO(now = new Date()): string {
  const y = now.getFullYear()
  const m = String(now.getMonth() + 1).padStart(2, '0')
  const d = String(now.getDate()).padStart(2, '0')
  return `${y}-${m}-${d}`
}

const DAY_RE = /^\d{4}-\d{2}-\d{2}$/

function dayNumber(day: string): number {
  const [y, m, d] = day.split('-').map(Number)
  return Date.UTC(y, m - 1, d) / 86_400_000
}

export type CustomRangeIssue = 'missing' | 'order' | 'future' | 'span'

// The same rules the server applies to a custom range, so the form can say
// what is wrong before the request is made.
export function customRangeIssue(from: string, to: string, today = todayISO()): CustomRangeIssue | null {
  if (!DAY_RE.test(from) || !DAY_RE.test(to)) return 'missing'
  const a = dayNumber(from)
  const b = dayNumber(to)
  if (Number.isNaN(a) || Number.isNaN(b)) return 'missing'
  if (b < a) return 'order'
  if (b > dayNumber(today)) return 'future'
  if (b - a + 1 > MAX_CUSTOM_SPAN_DAYS) return 'span'
  return null
}

export function hasActiveFilters(f: AnalyticsFilters): boolean {
  return (
    f.preset !== DEFAULT_FILTERS.preset ||
    f.scope !== DEFAULT_FILTERS.scope ||
    f.flows.length > 0 ||
    f.severity.length > 0 ||
    f.category.length > 0 ||
    f.rule.length > 0 ||
    f.compare !== DEFAULT_FILTERS.compare
  )
}

// toQuery renders the filters as the canonical query string of
// GET /api/dashboard/analytics: fixed parameter order, lists deduplicated and
// sorted, so equal filters always produce the same URL (the request cache and
// the store's change detection both key on it).
export function toQuery(f: AnalyticsFilters, tz = localTimeZone()): string {
  const p = new URLSearchParams()
  p.set('range', f.preset)
  if (f.preset === 'custom') {
    p.set('from', f.from)
    p.set('to', f.to)
  }
  p.set('tz', tz)
  p.set('scope', f.scope)
  if (f.scope === 'org' && f.orgId) p.set('orgId', f.orgId)
  const list = (key: string, values: string[]) => {
    const clean = [...new Set(values.map(v => v.trim()).filter(Boolean))].sort()
    if (clean.length > 0) p.set(key, clean.join(','))
  }
  list('flows', f.flows)
  list('severity', f.severity)
  list('category', f.category)
  list('rule', f.rule)
  p.set('compare', f.compare ? '1' : '0')
  return p.toString()
}
