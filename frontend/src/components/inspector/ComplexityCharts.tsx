import {useTranslation} from 'react-i18next'
import {ScatterChart, Scatter, XAxis, YAxis, ZAxis, Tooltip, CartesianGrid, Cell} from 'recharts'
import {Zap, Target, Wrench, Clock} from 'lucide-react'
import {
  useChartTheme,
  useReducedMotion,
  ChartFigure,
  ChartTooltip,
  ChartLegend,
  gridProps,
  tickStyle,
  yAxisNumber,
  cursorLine,
  CHART_MARGIN,
} from '@/components/charts'
import type {Finding, SubflowMetrics} from '@/types'

export function ComplexityScatter({subflows}: {subflows: SubflowMetrics[]}) {
  const {t} = useTranslation('inspector')
  const theme = useChartTheme()
  const reduced = useReducedMotion()
  const data = subflows.map(s => ({
    name: s.subflowName,
    cyclo: s.cyclomaticComplexity,
    cog: s.cognitiveComplexity,
    depth: s.maxNestingDepth,
  }))

  if (subflows.length === 0) return null

  const pointColor = (cog: number) =>
    cog > 30 ? theme.status.error : cog > 15 ? theme.status.warning : theme.status.success
  const legend = [
    {key: 'low', label: t('scatter.legendLow'), color: theme.status.success, shape: 'dot' as const},
    {key: 'mid', label: t('scatter.legendMid'), color: theme.status.warning, shape: 'dot' as const},
    {key: 'high', label: t('scatter.legendHigh'), color: theme.status.error, shape: 'dot' as const},
  ]

  return (
    <div>
      <h3 className="text-xs font-bold uppercase tracking-widest text-text-tertiary mb-2">{t('scatter.title')}</h3>
      <p className="text-2xs text-text-tertiary mb-2">{t('scatter.description')}</p>
      <div role="group" aria-roledescription="chart" aria-label={t('scatter.aria')}>
        <ChartFigure height={232}>
          <ScatterChart margin={{...CHART_MARGIN, bottom: 16}}>
            <CartesianGrid {...gridProps(theme)} vertical />
            <XAxis
              type="number"
              dataKey="cyclo"
              name={t('scatter.cyclomaticAxis')}
              axisLine={{stroke: theme.axis}}
              tickLine={false}
              tick={tickStyle(theme)}
              tickMargin={8}
              allowDecimals={false}
              label={{value: t('scatter.cyclomaticAxis'), position: 'insideBottom', offset: -12, fill: theme.tick, fontSize: 10}}
            />
            <YAxis type="number" dataKey="cog" name={t('scatter.cognitiveAxis')} {...yAxisNumber(theme, v => String(v))} />
            <ZAxis type="number" dataKey="depth" range={[40, 320]} name={t('scatter.depthAxis')} />
            <Tooltip
              cursor={cursorLine(theme)}
              content={
                <ChartTooltip theme={theme} labelFormatter={(_label, payload) => String(payload[0]?.payload?.name ?? '')} />
              }
            />
            <Scatter data={data} isAnimationActive={!reduced}>
              {data.map((d, i) => (
                <Cell key={i} fill={pointColor(d.cog)} fillOpacity={0.8} stroke={theme.surface} strokeWidth={2} />
              ))}
            </Scatter>
          </ScatterChart>
        </ChartFigure>
      </div>
      <ChartLegend items={legend} className="mt-2" />
    </div>
  )
}

export function ImpactEffortMatrix({findings}: {findings: Finding[]}) {
  const {t} = useTranslation('inspector')

  if (findings.length === 0) return null

  let quickWins = 0
  let strategic = 0
  let easyCleanup = 0
  let backlog = 0
  for (const f of findings) {
    const isError = f.severity === 'error'
    const fixable = !!f.autoFix
    if (isError && fixable) quickWins++
    else if (isError) strategic++
    else if (fixable) easyCleanup++
    else backlog++
  }

  const quadrants = [
    {
      icon: Zap,
      label: t('matrix.quickWins'),
      desc: t('matrix.quickWinsDesc'),
      count: quickWins,
      tone: 'text-semantic-success',
      ring: 'border-semantic-success/30 bg-semantic-success/5',
    },
    {
      icon: Target,
      label: t('matrix.strategic'),
      desc: t('matrix.strategicDesc'),
      count: strategic,
      tone: 'text-semantic-error',
      ring: 'border-semantic-error/30 bg-semantic-error/5',
    },
    {
      icon: Wrench,
      label: t('matrix.easyCleanup'),
      desc: t('matrix.easyCleanupDesc'),
      count: easyCleanup,
      tone: 'text-brand-400',
      ring: 'border-brand-500/30 bg-brand-500/5',
    },
    {
      icon: Clock,
      label: t('matrix.backlog'),
      desc: t('matrix.backlogDesc'),
      count: backlog,
      tone: 'text-text-tertiary',
      ring: 'border-border-subtle bg-surface-0',
    },
  ]

  return (
    <div>
      <h3 className="text-xs font-bold uppercase tracking-widest text-text-tertiary mb-2">{t('matrix.title')}</h3>
      <p className="text-2xs text-text-tertiary mb-2">{t('matrix.description')}</p>
      <div className="grid grid-cols-2 gap-2">
        {quadrants.map(q => (
          <div key={q.label} className={`p-3 rounded-lg border ${q.ring}`}>
            <div className="flex items-center gap-1.5 mb-1">
              <q.icon size={13} className={q.tone} />
              <span className="text-xs font-semibold text-text-primary">{q.label}</span>
            </div>
            <div className={`text-xl font-semibold ${q.tone}`}>{q.count}</div>
            <div className="text-2xs text-text-tertiary mt-0.5">{q.desc}</div>
          </div>
        ))}
      </div>
    </div>
  )
}
