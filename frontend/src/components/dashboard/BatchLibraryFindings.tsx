import {useId, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {Layers} from 'lucide-react'
import clsx from 'clsx'
import SeverityBadge from '@/components/findings/SeverityBadge'
import PreviewBadge from '@/components/analyzer/PreviewBadge'
import {severityTone} from '@/lib/severityTone'
import type {LibraryFinding, LibraryLocation, LibraryReport} from '@/types'

interface RuleGroup {
  ruleId: string
  head: LibraryFinding
  findings: LibraryFinding[]
}

function groupByRule(findings: LibraryFinding[]): RuleGroup[] {
  const groups = new Map<string, RuleGroup>()
  for (const f of findings) {
    const group = groups.get(f.ruleId)
    if (group) group.findings.push(f)
    else groups.set(f.ruleId, {ruleId: f.ruleId, head: f, findings: [f]})
  }
  return [...groups.values()]
}

/** The folder batch's library findings (desktop "Analyze Folder…"): one group per rule, as the Findings tab groups
 * a flow's, each finding naming its flow and subflow and the other places involved. */
export function BatchLibraryFindings({report}: {report: LibraryReport}) {
  const {t, i18n} = useTranslation('dashboard')
  const titleId = useId()
  const groups = useMemo(() => groupByRule(report.findings ?? []), [report.findings])
  const list = useMemo(() => new Intl.ListFormat(i18n.language, {type: 'conjunction'}), [i18n.language])

  const place = (loc: LibraryLocation) => {
    const flow = loc.flowName || loc.flowId
    return loc.subflowName ? t('batchLibrary.location', {flow, subflow: loc.subflowName}) : flow
  }

  return (
    <section aria-labelledby={titleId} className="mt-3 pt-3 border-t border-border-subtle space-y-2">
      <div className="flex items-center justify-between gap-2">
        <h4 id={titleId} className="text-sm font-bold text-text-secondary flex items-center gap-1.5">
          <Layers size={13} aria-hidden="true" />
          {t('batchLibrary.title')}
        </h4>
        <span className="text-xs text-text-tertiary tabular-nums flex items-center gap-1.5">
          <span>{t('batchLibrary.findings', {count: report.findings?.length ?? 0})}</span>
          <span aria-hidden="true">·</span>
          <span>{t('batchLibrary.compared', {count: report.flows})}</span>
        </span>
      </div>
      <p className="text-xs text-text-tertiary">{t('batchLibrary.hint')}</p>
      {report.truncated && <p className="text-xs text-semantic-warning">{t('batchLibrary.truncated')}</p>}
      {(report.rulesSkipped ?? 0) > 0 && (
        <p className="text-xs text-semantic-warning">{t('batchLibrary.skipped', {count: report.rulesSkipped})}</p>
      )}
      {groups.length === 0 ? (
        <p className="text-sm text-text-tertiary">{t('batchLibrary.none')}</p>
      ) : (
        <ul className="space-y-2">
          {groups.map(group => (
            <li
              key={group.ruleId}
              className={clsx('rounded border border-border-subtle border-l-2', severityTone(group.head.severity).bar)}
            >
              <div className="px-2.5 py-2 space-y-1">
                <div className="flex items-center gap-2 min-w-0">
                  <SeverityBadge severity={group.head.severity} />
                  <span className="text-sm font-medium text-text-primary truncate">{group.head.title}</span>
                  <span className="text-2xs text-text-tertiary shrink-0 tabular-nums">{group.findings.length}×</span>
                  {group.head.stage === 'preview' && <PreviewBadge of="finding" />}
                </div>
                {group.head.suggestion && (
                  <p className="text-2xs text-text-secondary leading-relaxed">{group.head.suggestion}</p>
                )}
              </div>
              <ul className="border-t border-border-subtle divide-y divide-border-subtle">
                {group.findings.map((f, i) => (
                  <li key={`${f.flowId}:${f.subflowName ?? ''}:${i}`} className="px-2.5 py-1.5">
                    <p className="text-sm text-text-primary truncate">{place(f)}</p>
                    <p className="text-2xs text-text-tertiary leading-relaxed">{f.description}</p>
                    {f.related && f.related.length > 0 && (
                      <p className="text-2xs text-text-tertiary">
                        {t('batchLibrary.alsoIn', {places: list.format(f.related.map(place))})}
                      </p>
                    )}
                  </li>
                ))}
              </ul>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
