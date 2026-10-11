import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {Wrench} from 'lucide-react'
import {ChartCard, ChartLegend, HBarChart, Meter, useChartTheme, type HBarRow} from '@/components/charts'
import {formatCompactIntl, formatPercent} from '@/lib/format'
import {useAnalyticsStore} from '@/stores/analyticsStore'
import type {DashboardAnalyticsData, RuleCount} from '@/types'
import {SeverityChip} from '../../home/FindingsByRuleCard'
import {ChartSection} from '../ChartSection'

const TOP_RULES = 15
const CONFIDENCE = ['high', 'medium', 'low'] as const

interface RuleRow extends HBarRow {
  name: string
  category: string
  severity: string
  autoFixable: boolean
  flows: number
}

interface CategoryRow extends HBarRow {
  errors: number
  warnings: number
  info: number
}

export function FindingsSection({data, refetching}: {data: DashboardAnalyticsData; refetching: boolean}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const toggleRule = useAnalyticsStore(s => s.toggleRule)
  const activeRules = data.applied.rule
  const failed = data.partial.includes('findings')
  const state = (ok: boolean) => (failed ? 'error' : ok ? 'ready' : 'empty')

  const ruleRows = useMemo<RuleRow[]>(
    () =>
      data.findings.byRule.map(r => ({
        key: r.rule,
        label: r.name || r.rule,
        value: r.count,
        name: r.name,
        category: r.category,
        severity: r.severity,
        autoFixable: r.autoFixable,
        flows: r.flows,
      })),
    [data.findings.byRule],
  )
  const topRules = useMemo<RuleRow[]>(() => {
    const top = ruleRows.slice(0, TOP_RULES)
    const rest = ruleRows.slice(TOP_RULES)
    if (rest.length === 0) return top
    const other: RuleRow = {
      key: '__other',
      label: t('common.other'),
      value: rest.reduce((s, r) => s + r.value, 0),
      name: '',
      category: '',
      severity: '',
      autoFixable: false,
      flows: 0,
    }
    return [...top, other]
  }, [ruleRows, t])

  const categoryRows = useMemo<CategoryRow[]>(
    () =>
      data.findings.byCategory.map(c => ({
        key: c.category,
        label: c.category,
        value: c.count,
        errors: c.errors,
        warnings: c.warnings,
        info: c.info,
      })),
    [data.findings.byCategory],
  )

  const confidenceTotal = CONFIDENCE.reduce((s, k) => s + (data.findings.byConfidence[k] ?? 0), 0)
  const confidenceRows = CONFIDENCE.map((k, i) => ({
    key: k,
    label: t(`confidence.${k}`),
    count: data.findings.byConfidence[k] ?? 0,
    share: confidenceTotal > 0 ? (data.findings.byConfidence[k] ?? 0) / confidenceTotal : 0,
    color: theme.seq[3 - i] ?? theme.muted,
  }))
  const fix = data.findings.fixability

  return (
    <ChartSection
      id="analytics-findings"
      title={t('analytics.sections.findings')}
      description={t('analytics.sectionSub.findings')}
    >
      <ChartCard
        title={t('analytics.cards.byRule')}
        subtitle={t('analytics.cards.byRuleSub')}
        state={state(ruleRows.length > 0)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.byRuleEmpty')}
        className="analytics-span-7"
        table={{
          caption: t('analytics.cards.byRule'),
          filename: 'findings-by-rule.csv',
          rowKey: r => r.key,
          columns: [
            {key: 'key', header: t('analytics.columns.rule')},
            {key: 'name', header: t('analytics.columns.name')},
            {key: 'category', header: t('analytics.columns.category')},
            {
              key: 'severity',
              header: t('analytics.columns.severity'),
              render: r => <SeverityChip severity={r.severity} />,
              csv: r => r.severity,
            },
            {
              key: 'autoFixable',
              header: t('analytics.columns.autoFix'),
              render: r => (r.autoFixable ? <Wrench size={12} aria-label={t('analytics.columns.yes')} /> : ''),
              csv: r => (r.autoFixable ? t('analytics.columns.yes') : t('analytics.columns.no')),
            },
            {key: 'value', header: t('analytics.columns.findings'), align: 'right'},
            {key: 'flows', header: t('analytics.columns.flows'), align: 'right'},
          ],
          rows: ruleRows,
        }}
      >
        <HBarChart
          rows={topRules}
          valueName={t('analytics.columns.findings')}
          maxLabel={28}
          activeKey={activeRules.length === 1 ? activeRules[0] : null}
          onBarClick={key => {
            if (key !== '__other') toggleRule(key)
          }}
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.byCategory')}
        subtitle={t('analytics.cards.byCategorySub')}
        state={state(categoryRows.length > 0)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.byRuleEmpty')}
        className="analytics-span-5"
        table={{
          caption: t('analytics.cards.byCategory'),
          filename: 'findings-by-category.csv',
          rowKey: r => r.key,
          columns: [
            {key: 'label', header: t('analytics.columns.category')},
            {key: 'errors', header: t('analytics.columns.errors'), align: 'right'},
            {key: 'warnings', header: t('analytics.columns.warnings'), align: 'right'},
            {key: 'info', header: t('analytics.columns.info'), align: 'right'},
            {key: 'value', header: t('analytics.columns.findings'), align: 'right'},
          ],
          rows: categoryRows,
        }}
      >
        <HBarChart
          rows={categoryRows}
          valueName={t('analytics.columns.findings')}
          series={[
            {key: 'errors', name: t('severityPlural.error'), color: theme.status.error},
            {key: 'warnings', name: t('severityPlural.warning'), color: theme.status.warning},
            {key: 'info', name: t('severityPlural.info'), color: theme.status.info},
          ]}
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.confidence')}
        subtitle={t('analytics.cards.confidenceSub')}
        state={state(confidenceTotal > 0)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.confidenceEmpty')}
        className="analytics-span-7"
        table={{
          caption: t('analytics.cards.confidence'),
          filename: 'finding-confidence.csv',
          rowKey: r => r.key,
          columns: [
            {key: 'label', header: t('analytics.columns.confidence')},
            {key: 'count', header: t('analytics.columns.findings'), align: 'right'},
            {
              key: 'share',
              header: t('analytics.columns.share'),
              align: 'right',
              render: r => formatPercent(r.share),
              csv: r => formatPercent(r.share),
            },
          ],
          rows: confidenceRows,
        }}
      >
        <div className="flex h-3 w-full gap-0.5 overflow-hidden rounded-full" aria-hidden="true">
          {confidenceRows
            .filter(r => r.count > 0)
            .map(r => (
              <div key={r.key} style={{width: `${r.share * 100}%`, background: r.color}} />
            ))}
        </div>
        <ChartLegend
          items={confidenceRows.map(r => ({
            key: r.key,
            label: `${r.label} · ${formatCompactIntl(r.count)} (${formatPercent(r.share)})`,
            color: r.color,
            shape: 'rect',
          }))}
          className="mt-3"
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.fixability')}
        subtitle={t('analytics.cards.fixabilitySub')}
        state={state(fix.total > 0)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.confidenceEmpty')}
        footnote={t('home.rulesWithFixer', {available: fix.autoFixableRules, total: fix.totalRules})}
        className="analytics-span-5"
      >
        <div className="flex items-baseline gap-2">
          <span className="text-[32px] font-semibold leading-none text-text-primary">
            {formatPercent(fix.total > 0 ? fix.available / fix.total : 0)}
          </span>
          <span className="text-xs text-text-tertiary">
            {t('analytics.tiles.fixableOf', {available: fix.available, total: fix.total})}
          </span>
        </div>
        <Meter
          value={fix.available}
          max={fix.total}
          token="--success"
          label={t('analytics.tiles.meterFixable')}
          className="mt-3"
        />
      </ChartCard>
    </ChartSection>
  )
}

export type {RuleCount}
