import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import type {TFunction} from 'i18next'
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

// The picker's labels, not the internal routing ids: `agent:<x>` renders
// as "Agent · <x>" and `claude:<x>` as "<x> · Claude" (no other
// provider mints colon-prefixed model ids). "· served" is appended only
// when the model that answered differs from what was asked for (an Azure
// AI Foundry deployment alias, model router or agent) — compared against
// the RAW id, not the mapped label. Rows are grouped by the served model,
// so the same deployment can appear once per model it was routed to. The
// CSV export uses this same label (the display IS the export); the raw id
// stays recoverable from the model picker.
export function modelLabel(t: TFunction<'dashboard'>, r: Pick<ModelUsage, 'model' | 'servedModel'>): string {
  const raw = r.model
  const id = raw.startsWith('agent:')
    ? t('analytics.models.agent', {name: raw.slice('agent:'.length)})
    : raw.startsWith('claude:')
      ? t('analytics.models.claude', {name: raw.slice('claude:'.length)})
      : raw
  return r.servedModel && r.servedModel !== raw ? `${id} · ${r.servedModel}` : id
}

// Azure AI Foundry usage is not priced by baki (the cost lives in the Azure
// portal): its cost cells show an em dash, named "Not priced by baki" for
// assistive tech, instead of a misleading $0.00, and its bar is left out of
// the cost chart (the bar's value label and tooltip would read $0.00). The
// CSV export keeps the raw number.
const isUnpriced = (provider: string) => provider === 'azure-foundry'

function ProviderCost({provider, cost}: {provider: string; cost: number}) {
  const {t} = useTranslation('dashboard')
  if (!isUnpriced(provider)) return formatCurrency(cost)
  const label = t('analytics.cards.costNotPriced')
  return (
    <span title={label}>
      <span aria-hidden="true">—</span>
      <span className="sr-only">{label}</span>
    </span>
  )
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
  const pricedProviders = useMemo(() => providers.filter(p => !isUnpriced(p.key)), [providers])
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
              render: r => <ProviderCost provider={r.key} cost={r.value} />,
              csv: r => String(r.value),
            },
          ],
          rows: providers,
        }}
      >
        {pricedProviders.length > 0 ? (
          <HBarChart
            rows={pricedProviders}
            valueName={t('analytics.columns.cost')}
            valueFormatter={v => formatCurrency(v)}
            allowDecimals
          />
        ) : (
          <p className="py-8 text-center text-sm text-text-tertiary">{t('analytics.cards.costOnlyUnpriced')}</p>
        )}
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
          rowKey={r => `${r.provider}/${r.model}/${r.servedModel ?? ''}`}
          columns={[
            {key: 'provider', header: t('analytics.columns.provider')},
            {
              key: 'model',
              header: t('analytics.columns.model'),
              render: r => modelLabel(t, r),
              csv: r => modelLabel(t, r),
            },
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
              render: r => <ProviderCost provider={r.provider} cost={r.cost} />,
              csv: r => String(r.cost),
            },
          ]}
          rows={ai.byModel}
        />
      </ChartCard>
    </ChartSection>
  )
}
