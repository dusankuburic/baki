import React from 'react'
import {TrendingUp} from 'lucide-react'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import type {AnalysisSnapshot} from '@/types'

// HealthTrend renders the persisted analysis snapshots as a health-score
// sparkline with per-run severity counts on hover. Hidden until there are at
// least two distinct runs to connect.
export default function HealthTrend() {
  const doc = useFlowStore(s => s.document)
  const generatedAt = useAnalysisStore(s => (doc ? s.reports.get(doc.id)?.generatedAt : undefined))
  const [snapshots, setSnapshots] = React.useState<AnalysisSnapshot[]>([])
  const [hover, setHover] = React.useState<number | null>(null)

  React.useEffect(() => {
    if (!doc) return
    let cancelled = false
    analysisApi
      .getHistory()
      .then(s => {
        if (!cancelled) setSnapshots((s ?? []).slice(-20))
      })
      .catch(err => {
        if (!cancelled) logger.warn('Failed to load history', err)
      })
    return () => {
      cancelled = true
    }
  }, [doc, generatedAt])

  if (snapshots.length < 2) return null

  const W = 280
  const H = 56
  const PAD = 6
  const step = (W - PAD * 2) / (snapshots.length - 1)
  const y = (score: number) => PAD + (H - PAD * 2) * (1 - Math.max(0, Math.min(100, score)) / 100)
  const points = snapshots.map((s, i) => `${PAD + i * step},${y(s.healthScore)}`).join(' ')
  const active = hover != null ? snapshots[hover] : snapshots[snapshots.length - 1]

  return (
    <div className="p-3 rounded-lg border border-border-subtle bg-surface-0">
      <div className="flex items-center justify-between mb-1">
        <span className="text-xs font-bold uppercase tracking-widest text-text-tertiary flex items-center gap-1.5">
          <TrendingUp size={12} />
          Health Trend
        </span>
        <span className="text-xs text-text-tertiary tabular-nums">
          {new Date(active.timestamp).toLocaleString()} · score {active.healthScore} ·{' '}
          <span className="text-red-400">{active.errors}E</span>{' '}
          <span className="text-amber-400">{active.warnings}W</span>{' '}
          <span className="text-blue-400">{active.info}I</span>
        </span>
      </div>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="w-full"
        role="img"
        aria-label={`Health score trend over ${snapshots.length} analysis runs`}
        onMouseLeave={() => setHover(null)}
      >
        <polyline points={points} fill="none" stroke="var(--brand-500)" strokeWidth="1.5" strokeLinejoin="round" />
        {snapshots.map((s, i) => (
          <g key={s.timestamp + i}>
            {/* invisible wide hit area per point */}
            <rect
              x={PAD + i * step - step / 2}
              y={0}
              width={step}
              height={H}
              fill="transparent"
              onMouseEnter={() => setHover(i)}
            />
            <circle
              cx={PAD + i * step}
              cy={y(s.healthScore)}
              r={hover === i ? 3.5 : 2}
              fill={s.errors > 0 ? 'var(--error)' : s.warnings > 0 ? 'var(--warning)' : 'var(--success)'}
              className="transition-all duration-fast"
            />
          </g>
        ))}
      </svg>
    </div>
  )
}
