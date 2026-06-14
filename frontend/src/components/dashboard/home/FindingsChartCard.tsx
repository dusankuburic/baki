import {RadarChart, Radar, PolarGrid, PolarAngleAxis, ResponsiveContainer, Tooltip} from 'recharts'
import {ArrowUpRight} from 'lucide-react'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors} from './useChartColors'
import type {DashboardFindingsAgg} from '@/types/domain'

// FindingsChartCard shows findings-by-category as a radar. The title links into
// the existing analytics dashboard for the full breakdown rather than
// re-implementing it here.
export function FindingsChartCard({
  findings,
  onOpenAnalytics,
  className,
}: {
  findings: DashboardFindingsAgg
  onOpenAnalytics: () => void
  className?: string
}) {
  const colors = useChartColors()
  const data = findings.byCategory

  const action = (
    <button
      onClick={onOpenAnalytics}
      className="inline-flex items-center gap-1 text-2xs text-text-tertiary hover:text-brand-400 transition-colors"
      title="Open full analytics"
    >
      Details <ArrowUpRight size={12} />
    </button>
  )

  return (
    <CardShell title="Findings by Category" action={action} className={className}>
      {!findings.available || data.length === 0 ? (
        <CardPlaceholder message="No findings yet. Analyze a flow to populate this." />
      ) : (
        <div className="h-48">
          <ResponsiveContainer width="100%" height="100%">
            <RadarChart data={data} outerRadius="70%">
              <PolarGrid stroke={colors.borderStrong} />
              <PolarAngleAxis dataKey="category" tick={{fill: colors.textTertiary, fontSize: 11}} />
              <Tooltip
                contentStyle={{
                  backgroundColor: 'var(--glass-bg)',
                  borderColor: 'var(--border-subtle)',
                  borderRadius: 8,
                  backdropFilter: 'var(--glass-blur)',
                  fontSize: 12,
                }}
                labelStyle={{color: 'var(--text-primary)'}}
              />
              <Radar dataKey="count" stroke={colors.brand500} fill={colors.brand500} fillOpacity={0.3} isAnimationActive={false} />
            </RadarChart>
          </ResponsiveContainer>
        </div>
      )}
    </CardShell>
  )
}
