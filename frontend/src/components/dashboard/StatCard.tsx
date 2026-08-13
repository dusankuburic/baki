import clsx from 'clsx'

// StatCard is a single labeled metric tile (Flows Analyzed, Total Findings,
// Avg Health). Extracted from AnalyticsDashboard for reuse + isolation.
export function StatCard({label, value, accent}: {label: string; value: string | number; accent?: string}) {
  return (
    <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
      <div className="text-sm text-text-tertiary uppercase tracking-widest mb-1">{label}</div>
      <div className={clsx('text-2xl font-black font-mono tabular-nums', accent ?? 'text-text-primary')}>{value}</div>
    </div>
  )
}
