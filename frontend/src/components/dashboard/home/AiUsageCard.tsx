import {useTranslation} from 'react-i18next'
import {ChartCard, StackedAreaChart, useChartTheme} from '@/components/charts'
import {formatCompactIntl, formatDateLong} from '@/lib/format'
import {DetailsAction} from './DetailsAction'
import type {DailyTokenUsage} from '@/types'

export function AiUsageCard({
  tokens,
  isCloud,
  onDetails,
  className,
}: {
  tokens: DailyTokenUsage[]
  isCloud: boolean
  onDetails?: () => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const hasTokens = tokens.some(d => d.tokensIn > 0 || d.tokensOut > 0)
  const title = t('home.aiUsage')
  return (
    <ChartCard
      title={title}
      actions={<DetailsAction title={title} onClick={onDetails} />}
      state={!isCloud ? 'empty' : hasTokens ? 'ready' : 'empty'}
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
    </ChartCard>
  )
}
