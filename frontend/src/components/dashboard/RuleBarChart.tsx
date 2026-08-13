import {ShieldAlert} from 'lucide-react'

// RuleBarChart renders the top-N rules by finding count as horizontal bars.
// entries is pre-sorted+truncated by the caller; maxCount sizes the widest bar.
// Extracted from AnalyticsDashboard; pure/presentational.
export function RuleBarChart({entries, maxCount}: {entries: [string, number][]; maxCount: number}) {
  if (entries.length === 0) return null
  return (
    <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
      <h3 className="text-sm font-bold uppercase tracking-widest text-text-tertiary mb-2 flex items-center gap-1.5">
        <ShieldAlert size={14} />
        Findings by Rule
      </h3>
      <div className="space-y-1.5">
        {entries.map(([rule, count]) => (
          <div key={rule} className="flex items-center gap-2">
            <span className="text-sm text-text-secondary font-mono w-44 truncate shrink-0">{rule}</span>
            <div className="flex-1 h-1.5 bg-surface-3 rounded-full overflow-hidden">
              <div className="h-full rounded-full bg-brand-500/70" style={{width: `${(count / maxCount) * 100}%`}} />
            </div>
            <span className="text-sm text-text-tertiary tabular-nums w-8 text-right shrink-0">{count}</span>
          </div>
        ))}
      </div>
    </div>
  )
}
