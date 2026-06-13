import React, {useState} from 'react'
import clsx from 'clsx'
import type {Finding, FlowDocument} from '@/types/domain'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {findBlockInDoc} from '@/lib/tree'
import {ArrowRight, Sparkles, EyeOff, Wrench, ChevronDown} from 'lucide-react'

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

  const handleJump = () => {
    selectSubflow(finding.subflowId)
    selectBlock(finding.blockId)
  }

  const handleSuppress = () => {
    suppressFinding(finding, 'Dismissed by user')
  }

  const loc = findBlockInDoc(doc, finding.blockId)
  const blockLabel = loc?.block.name ?? finding.blockId.slice(0, 8)
  const subflowLabel = loc?.subflowName

  const catColors: Record<string, string> = {
    Security: 'text-red-400 bg-red-500/10',
    Reliability: 'text-amber-400 bg-amber-500/10',
    Performance: 'text-orange-400 bg-orange-500/10',
    Style: 'text-purple-400 bg-purple-500/10',
    Logic: 'text-cyan-400 bg-cyan-500/10',
  }

  return (
    <div className="hover:bg-surface-2/50 transition-colors group">
      <div className="px-4 py-2 pl-9 flex items-center gap-3">
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="text-xs text-text-primary font-mono truncate">{blockLabel}</span>
            {finding.category && (
              <span className={`text-2xs font-bold uppercase tracking-wider px-1.5 py-0.5 rounded ${catColors[finding.category] ?? 'text-text-tertiary bg-surface-3'}`}>
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
    </div>
  )
}

export default React.memo(FindingCard)
