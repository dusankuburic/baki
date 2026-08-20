import clsx from 'clsx'

// StatCard is a single labeled metric tile (Flows Analyzed, Total Findings,
// Avg Health). Extracted from AnalyticsDashboard for reuse + isolation.
// ariaLabel optionally overrides the accessible name when the visual value
// alone is ambiguous (e.g. a color-banded average health score).
export function StatCard({
  label,
  value,
  accent,
  ariaLabel,
}: {
  label: string
  value: string | number
  accent?: string
  ariaLabel?: string
}) {
  return (
    <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
      <div className="text-sm text-text-tertiary uppercase tracking-widest mb-1">{label}</div>
      <div
        className={clsx('text-2xl font-black font-mono tabular-nums', accent ?? 'text-text-primary')}
        role={ariaLabel ? 'img' : undefined}
        aria-label={ariaLabel}
      >
        {value}
      </div>
    </div>
  )
}
