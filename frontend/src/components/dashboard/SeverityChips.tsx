import clsx from 'clsx'

// Severity/category chip colour maps. Exported so tests/other tiles can reuse.
export const sevChip: Record<string, string> = {
  error: 'text-red-400 bg-red-500/10',
  warning: 'text-amber-400 bg-amber-500/10',
  info: 'text-blue-400 bg-blue-500/10',
}

export const catChip: Record<string, string> = {
  Security: 'text-red-400 bg-red-500/10',
  Reliability: 'text-amber-400 bg-amber-500/10',
  Performance: 'text-orange-400 bg-orange-500/10',
  Style: 'text-purple-400 bg-purple-500/10',
  Logic: 'text-cyan-400 bg-cyan-500/10',
}

// SeverityChips renders the severity + category distribution as labelled pill
// counts. Extracted from AnalyticsDashboard; pure/presentational.
export function SeverityChips({
  bySeverity,
  byCategory,
}: {
  bySeverity: Record<string, number>
  byCategory: Record<string, number>
}) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {Object.entries(bySeverity).map(([sev, n]) => (
        <span
          key={sev}
          className={clsx(
            'text-sm font-bold px-2 py-0.5 rounded-full',
            sevChip[sev] ?? 'text-text-tertiary bg-surface-3',
          )}
        >
          {n} {sev}
        </span>
      ))}
      {Object.entries(byCategory).map(([cat, n]) => (
        <span
          key={cat}
          className={clsx(
            'text-sm font-bold px-2 py-0.5 rounded-full',
            catChip[cat] ?? 'text-text-tertiary bg-surface-3',
          )}
        >
          {n} {cat}
        </span>
      ))}
    </div>
  )
}
