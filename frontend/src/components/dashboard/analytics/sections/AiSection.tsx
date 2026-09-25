import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {
  ChartCard,
  ColumnChart,
  DataTable,
  HBarChart,
  StackedAreaChart,
  useChartTheme,
  type HBarRow,
} from '@/components/charts'
import {formatCompactIntl, formatCurrency, formatDateLong} from '@/lib/format'
import type {DashboardAnalyticsData, ModelUsage} from '@/types'
import {ChartSection} from '../ChartSection'

interface ProviderRow extends HBarRow {
  tokensIn: number
  tokensOut: number
}

// Model calls, tokens and spend for the account and/or the scoped orgs. Usage
// is never tied to flows, and the section says so.
export function AiSection({data, refetching}: {data: DashboardAnalyticsData; refetching: boolean}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const ai = data.ai
  const failed = data.partial.includes('ai')
  const hasTokens = ai.daily.some(d => d.tokensIn > 0 || d.tokensOut > 0)
  const hasCost = ai.daily.some(d => d.cost > 0)
  const providers = useMemo<ProviderRow[]>(
    () =>
      [...ai.byProvider]
        .sort((a, b) => b.cost - a.cost)
        .map(p => ({key: p.provider, label: p.provider, value: p.cost, tokensIn: p.tokensIn, tokensOut: p.tokensOut})),
    [ai.byProvider],
  )
  const scope =
    ai.scoped === 'org'
      ? t('analytics.cards.aiScopeOrg')
      : ai.scoped === 'user+orgs'
        ? t('analytics.cards.aiScopeBoth')
        : t('analytics.cards.aiScopeUser')
  const state = (ok: boolean) => (failed ? 'error' : ok ? 'ready' : 'empty')
  const dateCol = {
    key: 'date',
    header: t('analytics.columns.date'),
    render: (r: {date: string}) => formatDateLong(r.date),
  }

  return (
    <ChartSection
      id="analytics-ai"
      title={t('analytics.sections.ai')}
      description={`${t('analytics.sectionSub.ai')} ${scope}.`}
    >
      <ChartCard
        title={t('analytics.cards.tokens')}
        subtitle={t('analytics.cards.tokensSub')}
        state={state(hasTokens)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.aiEmpty')}
        className="analytics-span-7"
        table={{
          caption: t('analytics.cards.tokens'),
          filename: 'ai-tokens.csv',
          columns: [
            dateCol,
            {key: 'tokensIn', header: t('analytics.columns.tokensIn'), align: 'right'},
            {key: 'tokensOut', header: t('analytics.columns.tokensOut'), align: 'right'},
            {key: 'calls', header: t('analytics.columns.calls'), align: 'right'},
          ],
          rows: ai.daily,
        }}
      >
        <StackedAreaChart
          data={ai.daily}
          series={[
            {key: 'tokensIn', name: t('analytics.columns.tokensIn'), color: theme.series[0]},
            {key: 'tokensOut', name: t('analytics.columns.tokensOut'), color: theme.series[1]},
          ]}
          valueFormatter={formatCompactIntl}
          height={220}
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.cost')}
        subtitle={t('analytics.cards.costSub')}
        state={state(hasCost)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.aiEmpty')}
        className="analytics-span-5"
        table={{
          caption: t('analytics.cards.cost'),
          filename: 'ai-cost.csv',
          columns: [
            dateCol,
            {
              key: 'cost',
              header: t('analytics.columns.cost'),
              align: 'right',
              render: r => formatCurrency(r.cost),
              csv: r => String(r.cost),
            },
          ],
          rows: ai.daily,
        }}
      >
        <ColumnChart
          data={ai.daily}
          dataKey="cost"
          name={t('analytics.columns.cost')}
          valueFormatter={v => formatCurrency(v)}
          allowDecimals
          height={220}
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.byProvider')}
        state={state(providers.length > 0)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.aiEmpty')}
        className="analytics-span-5"
        table={{
          caption: t('analytics.cards.byProvider'),
          filename: 'ai-cost-by-provider.csv',
          columns: [
            {key: 'label', header: t('analytics.columns.provider')},
            {key: 'tokensIn', header: t('analytics.columns.tokensIn'), align: 'right'},
            {key: 'tokensOut', header: t('analytics.columns.tokensOut'), align: 'right'},
            {
              key: 'value',
              header: t('analytics.columns.cost'),
              align: 'right',
              render: r => formatCurrency(r.value),
              csv: r => String(r.value),
            },
          ],
          rows: providers,
        }}
      >
        <HBarChart
          rows={providers}
          valueName={t('analytics.columns.cost')}
          valueFormatter={v => formatCurrency(v)}
          allowDecimals
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.byModel')}
        state={state(ai.byModel.length > 0)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.aiEmpty')}
        className="analytics-span-7"
      >
        <DataTable<ModelUsage>
          caption={t('analytics.cards.byModel')}
          filename="ai-usage-by-model.csv"
          rowKey={r => `${r.provider}/${r.model}`}
          columns={[
            {key: 'provider', header: t('analytics.columns.provider')},
            {key: 'model', header: t('analytics.columns.model')},
            {key: 'calls', header: t('analytics.columns.calls'), align: 'right'},
            {
              key: 'tokensIn',
              header: t('analytics.columns.tokensIn'),
              align: 'right',
              render: r => formatCompactIntl(r.tokensIn),
              csv: r => String(r.tokensIn),
            },
            {
              key: 'tokensOut',
              header: t('analytics.columns.tokensOut'),
              align: 'right',
              render: r => formatCompactIntl(r.tokensOut),
              csv: r => String(r.tokensOut),
            },
            {
              key: 'cost',
              header: t('analytics.columns.cost'),
              align: 'right',
              render: r => formatCurrency(r.cost),
              csv: r => String(r.cost),
            },
          ]}
          rows={ai.byModel}
        />
      </ChartCard>
    </ChartSection>
  )
}
