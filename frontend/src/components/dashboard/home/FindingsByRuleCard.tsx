import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {ChartCard, HBarChart, type HBarRow} from '@/components/charts'
import {severityTone} from '@/lib/severityTone'
import {DetailsAction} from './DetailsAction'
import type {RuleFrequency} from '@/types'

interface Row extends HBarRow {
  severity: string
}

const SEVERITIES = new Set(['error', 'warning', 'info'])

// The ten most frequent rules as single-hue bars; the severity a rule fires at
// is a chip in the table twin, not a bar colour (status colours stay reserved
// for severity charts).
export function FindingsByRuleCard({
  data,
  onDetails,
  className,
}: {
  data: RuleFrequency[]
  onDetails?: () => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const rows = useMemo<Row[]>(
    () => data.slice(0, 10).map(d => ({key: d.rule, label: d.rule, value: d.count, severity: d.topSeverity ?? ''})),
    [data],
  )
  const title = t('home.byRule')
  return (
    <ChartCard
      title={title}
      subtitle={t('home.byRuleSub')}
      actions={<DetailsAction title={title} onClick={onDetails} />}
      state={rows.length > 0 ? 'ready' : 'empty'}
      emptyMessage={t('home.byRuleEmpty')}
      className={className}
      table={{
        caption: title,
        filename: 'findings-by-rule.csv',
        rowKey: r => r.key,
        columns: [
          {key: 'label', header: t('analytics.columns.rule')},
          {
            key: 'severity',
            header: t('analytics.columns.severity'),
            render: r => <SeverityChip severity={r.severity} />,
            csv: r => r.severity,
          },
          {key: 'value', header: t('analytics.columns.findings'), align: 'right'},
        ],
        rows,
      }}
    >
      <HBarChart rows={rows} valueName={t('analytics.columns.findings')} maxLabel={26} />
    </ChartCard>
  )
}

export function SeverityChip({severity}: {severity: string}) {
  const {t} = useTranslation('dashboard')
  if (!SEVERITIES.has(severity)) return <span className="text-text-tertiary">—</span>
  const tone = severityTone(severity as 'error' | 'warning' | 'info')
  return (
    <span className={clsx('inline-block rounded-full px-1.5 py-0.5 text-2xs font-semibold', tone.bg, tone.text)}>
      {t(`severity.${severity as 'error' | 'warning' | 'info'}`)}
    </span>
  )
}
