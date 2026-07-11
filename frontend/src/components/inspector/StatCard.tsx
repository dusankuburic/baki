import clsx from 'clsx'

export default function StatCard({label, value, warn}: {label: string; value: string | number; warn?: boolean}) {
  return (
    <div className="p-2.5 rounded-lg border border-border-subtle bg-surface-0">
      <div className="text-xs text-text-tertiary uppercase tracking-wider mb-0.5">{label}</div>
      <div className={clsx('text-sm font-bold font-mono tabular-nums', warn ? 'text-amber-400' : 'text-text-primary')}>
        {value}
      </div>
    </div>
  )
}
