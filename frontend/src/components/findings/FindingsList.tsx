import {useState, useMemo} from 'react'
import {ChevronRight, EyeOff} from 'lucide-react'
import clsx from 'clsx'
import type {Finding, Severity} from '@/types'
import type {FlowDocument} from '@/types'
import {useAnalysisStore} from '@/stores/analysisStore'
import FindingCard from './FindingCard'

interface Props {
  findings: Finding[]
  doc: FlowDocument
  onFixWithAI?: (finding: Finding) => void
}

interface RuleGroup {
  ruleId: string
  title: string
  severity: Severity
  description: string
  suggestion: string
  findings: Finding[]
}

function groupByRule(findings: Finding[]): RuleGroup[] {
  const map = new Map<string, RuleGroup>()
  for (const f of findings) {
    const existing = map.get(f.ruleId)
    if (existing) {
      existing.findings.push(f)
    } else {
      map.set(f.ruleId, {
        ruleId: f.ruleId,
        title: f.title,
        severity: f.severity,
        description: f.description,
        suggestion: f.suggestion ?? '',
        findings: [f],
      })
    }
  }
  const order: Record<Severity, number> = {error: 0, warning: 1, info: 2}
  return Array.from(map.values()).sort((a, b) => order[a.severity] - order[b.severity])
}

export default function FindingsList({findings, doc, onFixWithAI}: Props) {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const suppressMany = useAnalysisStore(s => s.suppressMany)

  const groups = useMemo(() => groupByRule(findings), [findings])

  const toggle = (ruleId: string) => {
    setCollapsed(prev => {
      const next = new Set(prev)
      if (next.has(ruleId)) next.delete(ruleId)
      else next.add(ruleId)
      return next
    })
  }

  const sevColor: Record<Severity, string> = {
    error: 'border-l-red-500',
    warning: 'border-l-amber-500',
    info: 'border-l-blue-500',
  }

  return (
    <div className="flex-1 overflow-y-auto">
      {groups.map(group => {
        const isCollapsed = collapsed.has(group.ruleId)
        return (
          <div key={group.ruleId} className={clsx('border-b border-border-subtle', sevColor[group.severity], 'border-l-2')}>
            <div className="w-full px-3 py-2.5 flex items-start gap-2 hover:bg-surface-2 transition-colors group/header">
              <button
                onClick={() => toggle(group.ruleId)}
                aria-expanded={!isCollapsed}
                className="flex-1 flex items-start gap-2 text-left min-w-0"
              >
                <ChevronRight
                  size={14}
                  className={clsx(
                    'mt-0.5 shrink-0 text-text-tertiary transition-transform duration-fast',
                    !isCollapsed && 'rotate-90'
                  )}
                />
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="text-sm font-medium text-text-primary truncate">{group.title}</span>
                    <span className="text-2xs text-text-tertiary shrink-0">{group.findings.length}×</span>
                  </div>
                  {!isCollapsed && (
                    <p className="text-2xs text-text-secondary mt-1 leading-relaxed">{group.description}</p>
                  )}
                </div>
              </button>
              <button
                onClick={() => suppressMany(group.findings, `Suppressed all "${group.title}" findings`)}
                aria-label={`Suppress all ${group.findings.length} findings of this rule`}
                title="Suppress all findings in this group"
                className="opacity-0 group-hover/header:opacity-100 focus-visible:opacity-100 shrink-0 mt-0.5 flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-all duration-fast"
              >
                <EyeOff size={12} />
                <span className="hidden sm:inline">All</span>
              </button>
            </div>

            {!isCollapsed && group.findings.map(f => (
              <FindingCard
                key={f.id}
                finding={f}
                doc={doc}
                onFixWithAI={onFixWithAI}
              />
            ))}
          </div>
        )
      })}
    </div>
  )
}
