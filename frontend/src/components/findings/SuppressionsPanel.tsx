import {useEffect, useState} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {ChevronRight} from 'lucide-react'
import {analysisApi} from '@/api'
import type {SuppressionEntry} from '@/types'

interface Props {
  flowId: string
  // Changes whenever a new report lands (the report's generatedAt), so an open
  // panel re-audits after re-analysis or a "Suppress in file".
  reportVersion?: string
}

// SuppressionsPanel lists the flow's inline `# pad-ignore` directives with a
// stale marker. The app could write directives ("Suppress in file") but never
// list them, so a directive left behind after its finding was fixed silently
// hid any future finding of that rule on that block. Collapsed by default and
// fetched only while open: the audit re-runs every rule over the flow.
export default function SuppressionsPanel({flowId, reportVersion}: Props) {
  const {t} = useTranslation('findings')
  const [open, setOpen] = useState(false)
  // Results are tagged with the request they answer, so loading is derived
  // (no synchronous setState in the effect) and a late response for a
  // previous flow/report is never shown.
  const requestKey = `${flowId}@${reportVersion ?? ''}`
  const [result, setResult] = useState<{key: string; entries?: SuppressionEntry[]; failed?: boolean} | null>(null)

  useEffect(() => {
    if (!open) return
    let cancelled = false
    const load = async () => {
      try {
        const entries = await analysisApi.listSuppressions(flowId)
        if (!cancelled) setResult({key: requestKey, entries})
      } catch {
        if (!cancelled) setResult({key: requestKey, failed: true})
      }
    }
    void load()
    return () => {
      cancelled = true
    }
  }, [open, flowId, requestKey])

  const current = result?.key === requestKey ? result : null
  const entries = current?.entries
  const staleCount = entries?.filter(e => e.stale).length ?? 0

  return (
    <div className="border-b border-border-subtle text-2xs">
      <button
        onClick={() => setOpen(o => !o)}
        aria-expanded={open}
        className="w-full px-3 py-1 flex items-center gap-1 text-text-tertiary hover:text-text-secondary hover:bg-surface-2 transition-colors"
      >
        <ChevronRight size={10} className={clsx('transition-transform duration-fast', open && 'rotate-90')} />
        <span>{t('inlineSuppressions.heading')}</span>
        {entries && <span className="tabular-nums">({entries.length})</span>}
        {staleCount > 0 && (
          <span className="ml-1 text-semantic-warning">{t('inlineSuppressions.staleCount', {count: staleCount})}</span>
        )}
      </button>
      {open && (
        <div className="px-3 pb-2">
          {!current ? (
            <div className="text-text-tertiary">{t('inlineSuppressions.loading')}</div>
          ) : current.failed ? (
            <div className="text-semantic-error">{t('inlineSuppressions.loadFailed')}</div>
          ) : !entries || entries.length === 0 ? (
            <div className="text-text-tertiary">{t('inlineSuppressions.empty')}</div>
          ) : (
            <ul className="space-y-0.5">
              {entries.map(e => (
                <li key={`${e.line}:${e.rule}`} className="flex items-center gap-2 min-w-0">
                  <span className="font-mono text-text-tertiary tabular-nums shrink-0">L{e.line}</span>
                  <span className="font-mono text-text-secondary shrink-0">
                    {e.rule === '*' ? t('inlineSuppressions.allRules') : e.rule}
                  </span>
                  <span className="text-text-tertiary truncate" title={e.subflow}>
                    {e.blockLabel}
                  </span>
                  {e.stale && (
                    <span
                      className="ml-auto shrink-0 uppercase tracking-wider px-1 py-0.5 rounded border text-semantic-warning border-semantic-warning/30 bg-semantic-warning/5"
                      title={e.reason}
                    >
                      {t('inlineSuppressions.stale')}
                    </span>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  )
}
