import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {ChartCard, StackedAreaChart, HBarChart, useChartTheme, type HBarRow} from '@/components/charts'
import {formatCompactIntl, formatCurrency, formatDateLong} from '@/lib/format'
import {DetailsAction} from './DetailsAction'
import type {DailyTokenUsage, ProviderCost} from '@/types'

interface CostRow extends HBarRow {
  tokensIn: number
  tokensOut: number
}

export function AiUsageCard({
  tokens,
  cost,
  isCloud,
  onDetails,
  className,
}: {
  tokens: DailyTokenUsage[]
  cost: ProviderCost[]
  isCloud: boolean
  onDetails?: () => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const hasTokens = tokens.some(d => d.tokensIn > 0 || d.tokensOut > 0)
  const costRows = useMemo<CostRow[]>(
    () =>
      cost
        .filter(c => c.provider !== 'azure-foundry')
        .sort((a, b) => b.cost - a.cost)
        .map(c => ({key: c.provider, label: c.provider, value: c.cost, tokensIn: c.tokensIn, tokensOut: c.tokensOut})),
    [cost],
  )
  const hasData = hasTokens || cost.length > 0
  const title = t('home.aiUsage')
  return (
    <ChartCard
      title={title}
      actions={<DetailsAction title={title} onClick={onDetails} />}
      state={!isCloud ? 'empty' : hasData ? 'ready' : 'empty'}
      emptyMessage={isCloud ? t('home.aiEmpty') : t('home.aiEmptyLocal')}
      className={className}
      table={{
        caption: title,
        filename: 'ai-tokens.csv',
        rowKey: r => r.date,
        columns: [
          {key: 'date', header: t('analytics.columns.date'), render: r => formatDateLong(r.date)},
          {key: 'tokensIn', header: t('home.tokensIn'), align: 'right'},
          {key: 'tokensOut', header: t('home.tokensOut'), align: 'right'},
        ],
        rows: tokens,
      }}
    >
      <div className="grid gap-4 ai-usage-grid">
        <div className="min-w-0">
          <p className="mb-1 text-2xs text-text-tertiary">{t('home.aiTokens')}</p>
          {hasTokens ? (
            <StackedAreaChart
              data={tokens}
              series={[
                {key: 'tokensIn', name: t('home.tokensIn'), color: theme.series[0]},
                {key: 'tokensOut', name: t('home.tokensOut'), color: theme.series[1]},
              ]}
              valueFormatter={formatCompactIntl}
              height={170}
            />
          ) : (
            <p className="py-8 text-center text-sm text-text-tertiary">{t('common.noData')}</p>
          )}
        </div>
        <div className="min-w-0">
          <p className="mb-1 text-2xs text-text-tertiary">{t('home.aiCost')}</p>
          {costRows.length > 0 ? (
            <HBarChart
              rows={costRows}
              valueName={t('analytics.columns.cost')}
              valueFormatter={v => formatCurrency(v)}
              allowDecimals
            />
          ) : (
            <p className="py-8 text-center text-sm text-text-tertiary">
              {cost.length > 0 ? t('analytics.cards.costOnlyUnpriced') : t('common.noData')}
            </p>
          )}
        </div>
      </div>
    </ChartCard>
  )
}
