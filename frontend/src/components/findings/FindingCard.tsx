import React, {useState, useCallback} from 'react'
import clsx from 'clsx'
import type {Finding, FlowDocument} from '@/types'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {findBlockInDoc} from '@/lib/tree'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import {categoryBadgeClass} from '@/lib/findingsColors'
import {ArrowRight, Sparkles, EyeOff, Wrench, ChevronDown, GitBranch} from 'lucide-react'

interface Props {
  finding: Finding
  doc: FlowDocument
  onFixWithAI?: (finding: Finding) => void
}

function FindingCard({finding, doc, onFixWithAI}: Props) {
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const suppressFinding = useAnalysisStore(s => s.suppressFinding)
  const [showHint, setShowHint] = useState(false)
  const [showRelated, setShowRelated] = useState(false)
  const [related, setRelated] = useState<Finding[] | null>(null)
  const [relatedLoading, setRelatedLoading] = useState(false)

  const handleJump = () => {
    selectSubflow(finding.subflowId)
    selectBlock(finding.blockId)
  }

  const handleSuppress = () => {
    suppressFinding(finding, 'Dismissed by user')
  }

  const handleRelated = useCallback(async () => {
    if (showRelated) {
      setShowRelated(false)
      return
    }
    setShowRelated(true)
    if (related !== null) return
    setRelatedLoading(true)
    try {
      const result = await analysisApi.getRelatedFindings(finding.blockId)
      setRelated(result.filter(f => f.id !== finding.id))
    } catch (err) {
      logger.warn('Failed to load related findings', err)
      setRelated([])
    } finally {
      setRelatedLoading(false)
    }
  }, [showRelated, related, finding.id, finding.blockId])

  const loc = findBlockInDoc(doc, finding.blockId)
  const blockLabel = loc?.block.name ?? finding.blockId.slice(0, 8)
  const subflowLabel = loc?.subflowName

  return (
    <div className="hover:bg-surface-2/50 transition-colors group">
      <div className="px-4 py-2 pl-9 flex items-center gap-3">
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="text-xs text-text-primary font-mono truncate">{blockLabel}</span>
            {finding.category && (
              <span className={`text-2xs font-bold uppercase tracking-wider px-1.5 py-0.5 rounded ${categoryBadgeClass(finding.category)}`}>
                {finding.category}
              </span>
            )}
          </div>
          {subflowLabel && (
            <span className="text-2xs text-text-tertiary ml-2">in {subflowLabel}</span>
          )}
        </div>

        <button
          onClick={handleJump}
          className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-primary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
        >
          <ArrowRight size={10} />
          Jump
        </button>

        <button
          onClick={handleRelated}
          disabled={relatedLoading}
          aria-expanded={showRelated}
          className={clsx(
            'flex items-center gap-1 text-2xs px-1.5 py-1 rounded transition-colors shrink-0 disabled:opacity-50',
            showRelated
              ? 'text-brand-400 bg-brand-500/10'
              : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3'
          )}
          title="Show related findings for this block"
        >
          <GitBranch size={10} />
          Related
        </button>

        {finding.autoFixHint && (
          <button
            onClick={() => setShowHint(h => !h)}
            aria-expanded={showHint}
            className="flex items-center gap-1 text-2xs text-emerald-400 hover:text-emerald-300 px-1.5 py-1 rounded hover:bg-emerald-500/10 transition-colors shrink-0"
            title="Show fix suggestion"
          >
            <Wrench size={10} />
            Fix
            <ChevronDown size={10} className={clsx('transition-transform duration-fast', showHint && 'rotate-180')} />
          </button>
        )}

        {onFixWithAI && (
          <button
            onClick={() => onFixWithAI(finding)}
            className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-1.5 py-1 rounded hover:bg-brand-500/10 transition-colors shrink-0"
          >
            <Sparkles size={10} />
            Fix with AI
          </button>
        )}

        <button
          onClick={handleSuppress}
          aria-label="Suppress this finding"
          className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
          title="Suppress this finding"
        >
          <EyeOff size={10} />
        </button>
      </div>

      {showHint && finding.autoFixHint && (
        <div className="mx-4 mb-2 ml-9 px-3 py-2 bg-emerald-500/8 border border-emerald-500/20 rounded text-2xs text-emerald-300 font-mono leading-relaxed">
          {finding.autoFixHint}
        </div>
      )}

      {showRelated && (
        <div className="mx-4 mb-2 ml-9 px-3 py-2 bg-surface-3 border border-border-subtle rounded space-y-1">
          <span className="text-2xs font-bold uppercase tracking-wider text-text-tertiary">
            Related findings
          </span>
          {relatedLoading ? (
            <span className="text-2xs text-text-tertiary">Loading…</span>
          ) : related && related.length > 0 ? (
            related.map(r => (
              <div key={r.id} className="flex items-center gap-2 text-2xs">
                <span className={clsx('font-bold uppercase', r.severity === 'error' ? 'text-red-400' : r.severity === 'warning' ? 'text-amber-400' : 'text-blue-400')}>
                  {r.severity}
                </span>
                <span className="text-text-secondary truncate">{r.title}</span>
                <span className="text-text-tertiary shrink-0">{r.ruleId}</span>
              </div>
            ))
          ) : (
            <span className="text-2xs text-text-tertiary">No other findings for this block.</span>
          )}
        </div>
      )}
    </div>
  )
}

export default React.memo(FindingCard)
