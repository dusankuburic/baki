import {useState, useMemo} from 'react'
import {ChevronRight} from 'lucide-react'
import clsx from 'clsx'
import type {Finding, Severity} from '@/types/domain'
import type {FlowDocument} from '@/types/domain'
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
            <button
              onClick={() => toggle(group.ruleId)}
              className="w-full px-3 py-2.5 flex items-start gap-2 text-left hover:bg-surface-2 transition-colors"
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
