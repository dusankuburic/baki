import {useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Building2, ChevronDown, RotateCcw} from 'lucide-react'
import {Button, Dropdown, Input, SegmentedControl, Switch, type DropdownItem} from '@/components/shared'
import {severityTone} from '@/lib/severityTone'
import {formatDateLong} from '@/lib/format'
import {useOrgStore} from '@/stores/orgStore'
import {
  useAnalyticsStore,
  customRangeIssue,
  hasActiveFilters,
  todayISO,
  type RangePreset,
} from '@/stores/analyticsStore'
import type {AnalyticsApplied, AnalyticsFacets, AnalyticsRange} from '@/types'
import {FilterChip} from './FilterChip'
import {FlowMultiSelect} from './FlowMultiSelect'

const SEVERITIES = ['error', 'warning', 'info'] as const
const RULE_CHIPS = 8

// The filter row above the charts: the time range first, then scope, flows,
// the comparison switch and reset; below it the severity / category / rule
// chips with their facet counts. The applied summary is read out on change.
export function AnalyticsFilterBar({
  facets,
  applied,
  range,
  busy,
}: {
  facets: AnalyticsFacets | null
  applied: AnalyticsApplied | null
  range: AnalyticsRange | null
  busy: boolean
}) {
  const {t} = useTranslation('dashboard')
  const filters = useAnalyticsStore(s => s.filters)
  const setPreset = useAnalyticsStore(s => s.setPreset)
  const setCustomRange = useAnalyticsStore(s => s.setCustomRange)
  const setScope = useAnalyticsStore(s => s.setScope)
  const setFlows = useAnalyticsStore(s => s.setFlows)
  const toggleSeverity = useAnalyticsStore(s => s.toggleSeverity)
  const toggleCategory = useAnalyticsStore(s => s.toggleCategory)
  const toggleRule = useAnalyticsStore(s => s.toggleRule)
  const setCompare = useAnalyticsStore(s => s.setCompare)
  const reset = useAnalyticsStore(s => s.reset)
  const organisations = useOrgStore(s => s.organisations)

  const [draftPreset, setDraftPreset] = useState<RangePreset>(filters.preset)
  const [draftFrom, setDraftFrom] = useState(filters.from)
  const [draftTo, setDraftTo] = useState(filters.to)
  const [rangeError, setRangeError] = useState<string | null>(null)
  const [allRules, setAllRules] = useState(false)
  const today = todayISO()

  const onPreset = (p: RangePreset) => {
    setDraftPreset(p)
    setRangeError(null)
    if (p !== 'custom') setPreset(p)
  }
  const applyCustom = () => {
    const issue = customRangeIssue(draftFrom, draftTo, today)
    if (issue) {
      setRangeError(
        t(
          issue === 'missing'
            ? 'analytics.filters.rangeMissing'
            : issue === 'order'
              ? 'analytics.filters.rangeOrder'
              : issue === 'future'
                ? 'analytics.filters.rangeFuture'
                : 'analytics.filters.rangeSpan',
        ),
      )
      return
    }
    setRangeError(null)
    setCustomRange(draftFrom, draftTo)
  }

  const scopeLabel =
    filters.scope === 'personal'
      ? t('analytics.filters.scopePersonal')
      : filters.scope === 'org'
        ? (organisations.find(o => o.id === filters.orgId)?.name ?? t('analytics.filters.scopeAll'))
        : t('analytics.filters.scopeAll')
  const scopeItems = useMemo<DropdownItem[]>(
    () => [
      {type: 'item', label: t('analytics.filters.scopeAll'), onSelect: () => setScope('all')},
      {type: 'item', label: t('analytics.filters.scopePersonal'), onSelect: () => setScope('personal')},
      ...(organisations.length > 0 ? [{type: 'separator'} as const] : []),
      ...organisations.map(o => ({type: 'item' as const, label: o.name, onSelect: () => setScope('org', o.id)})),
    ],
    [organisations, setScope, t],
  )

  const severityCounts = new Map(facets?.severity.map(f => [f.key, f.count]) ?? [])
  const categories = facets?.category ?? []
  const rules = facets?.rule ?? []
  const shownRules = allRules ? rules : rules.slice(0, RULE_CHIPS)
  const activeRulesHidden = filters.rule.filter(r => !shownRules.some(f => f.key === r))

  const summary = useMemo(() => {
    const parts: string[] = []
    if (filters.preset === 'custom') {
      parts.push(
        range
          ? t('analytics.filters.customRange', {from: formatDateLong(range.from), to: formatDateLong(range.to)})
          : t('analytics.filters.custom'),
      )
    } else {
      parts.push(
        t(
          filters.preset === '7d'
            ? 'analytics.filters.last7'
            : filters.preset === '90d'
              ? 'analytics.filters.last90'
              : 'analytics.filters.last30',
        ),
      )
    }
    parts.push(scopeLabel)
    const flows = applied?.flows.length ?? filters.flows.length
    if (flows > 0) parts.push(t('analytics.filters.summaryFlows', {count: flows}))
    if (filters.severity.length > 0)
      parts.push(filters.severity.map(s => t(`severityPlural.${s as 'error' | 'warning' | 'info'}`)).join(', '))
    if (filters.category.length > 0) parts.push(filters.category.join(', '))
    if (filters.rule.length > 0) parts.push(t('analytics.filters.summaryRules', {count: filters.rule.length}))
    if (filters.compare) parts.push(t('analytics.filters.summaryCompare'))
    return t('analytics.filters.summary', {parts: parts.join(' · ')})
  }, [filters, applied, range, scopeLabel, t])

  return (
    <div className="analytics-filters" aria-busy={busy || undefined}>
      <div className="analytics-filter-row">
        {/* Manual activation: a preset refetches the analytics, so arrowing
            across them only moves focus; Space or Enter picks one. */}
        <SegmentedControl<RangePreset>
          value={draftPreset}
          onChange={onPreset}
          size="sm"
          activation="manual"
          label={t('analytics.filters.range')}
          options={[
            {value: '7d', label: t('analytics.filters.preset7')},
            {value: '30d', label: t('analytics.filters.preset30')},
            {value: '90d', label: t('analytics.filters.preset90')},
            {value: 'custom', label: t('analytics.filters.custom')},
          ]}
        />
        {draftPreset === 'custom' && (
          <form
            className="analytics-custom-range flex flex-wrap items-center gap-2"
            onSubmit={e => {
              e.preventDefault()
              applyCustom()
            }}
          >
            <Input
              type="date"
              value={draftFrom}
              max={today}
              onChange={e => setDraftFrom(e.target.value)}
              aria-label={t('analytics.filters.from')}
              className="w-40"
            />
            <span className="text-xs text-text-tertiary" aria-hidden="true">
              –
            </span>
            <Input
              type="date"
              value={draftTo}
              max={today}
              onChange={e => setDraftTo(e.target.value)}
              aria-label={t('analytics.filters.to')}
              className="w-40"
            />
            <Button type="submit" size="sm" variant="primary">
              {t('analytics.filters.apply')}
            </Button>
            {rangeError && (
              <span role="alert" className="text-xs text-semantic-error">
                {rangeError}
              </span>
            )}
          </form>
        )}
        <Dropdown
          items={scopeItems}
          trigger={
            <Button size="sm" icon={Building2} aria-label={`${t('analytics.filters.scope')}: ${scopeLabel}`}>
              <span className="inline-flex items-center gap-1">
                {scopeLabel}
                <ChevronDown size={12} aria-hidden="true" className="text-text-tertiary" />
              </span>
            </Button>
          }
        />
        <FlowMultiSelect
          flows={facets?.flows ?? []}
          selected={filters.flows}
          truncated={facets?.flowsTruncated ?? false}
          onChange={setFlows}
        />
        <Switch
          checked={filters.compare}
          onChange={setCompare}
          label={t('analytics.filters.compare')}
          className="text-xs"
        />
        {hasActiveFilters(filters) && (
          <Button
            size="sm"
            variant="ghost"
            icon={RotateCcw}
            onClick={() => {
              reset()
              setDraftPreset('30d')
              setDraftFrom('')
              setDraftTo('')
              setRangeError(null)
            }}
          >
            {t('analytics.filters.reset')}
          </Button>
        )}
      </div>

      <div className="analytics-chip-row" role="group" aria-label={t('analytics.filters.severity')}>
        {SEVERITIES.map(s => (
          <FilterChip
            key={s}
            label={t(`severityPlural.${s}`)}
            count={severityCounts.get(s) ?? 0}
            pressed={filters.severity.includes(s)}
            tone={severityTone(s)}
            onClick={() => toggleSeverity(s)}
          />
        ))}
        {categories.length > 0 && <span className="analytics-chip-sep" aria-hidden="true" />}
        {categories.map(c => (
          <FilterChip
            key={c.key}
            label={c.label}
            count={c.count}
            pressed={filters.category.includes(c.key)}
            onClick={() => toggleCategory(c.key)}
          />
        ))}
      </div>

      <div className="analytics-chip-row" role="group" aria-label={t('analytics.filters.rule')}>
        {rules.length === 0 && <span className="text-2xs text-text-tertiary">{t('analytics.filters.noRules')}</span>}
        {activeRulesHidden.map(r => (
          <FilterChip key={r} label={r} pressed onClick={() => toggleRule(r)} />
        ))}
        {shownRules.map(r => (
          <FilterChip
            key={r.key}
            label={r.label}
            count={r.count}
            pressed={filters.rule.includes(r.key)}
            onClick={() => toggleRule(r.key)}
          />
        ))}
        {rules.length > RULE_CHIPS && (
          <button
            type="button"
            onClick={() => setAllRules(v => !v)}
            aria-expanded={allRules}
            className={clsx(
              'inline-flex h-7 shrink-0 items-center whitespace-nowrap rounded-full px-2 text-xs text-text-tertiary transition-colors duration-fast hover:text-text-primary',
            )}
          >
            {allRules ? t('analytics.filters.fewerRules') : t('analytics.filters.moreRules')}
          </button>
        )}
      </div>

      <p className="analytics-summary text-2xs text-text-tertiary" aria-live="polite">
        {summary}
      </p>
    </div>
  )
}
