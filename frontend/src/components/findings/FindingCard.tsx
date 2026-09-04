import {useTranslation} from 'react-i18next'
import React, {useState} from 'react'
import clsx from 'clsx'
import type {Finding} from '@/types'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {BlockLookup} from '@/lib/tree'
import {categoryBadgeClass} from '@/lib/findingsColors'
import {useToast} from '@/components/shared'
import {ArrowRight, Sparkles, EyeOff, Wrench, ChevronDown, GitBranch, FilePen, MessageSquare} from 'lucide-react'
import PatchPreviewModal from './PatchPreviewModal'
import FindingTriageMenu from './FindingTriageMenu'
import FindingRelatedPanel from './FindingRelatedPanel'
import FindingCommentsPanel from './FindingCommentsPanel'
import {useRelatedFindings} from './hooks/useRelatedFindings'
import {useFindingComments} from './hooks/useFindingComments'
import {useFindingFix} from './hooks/useFindingFix'
import {isTauri} from '@/platform/guards'

interface Props {
  finding: Finding
  blockLookup: BlockLookup
  onFixWithAI?: (finding: Finding) => void
}

function FindingCard({finding, blockLookup, onFixWithAI}: Props) {
  const {t} = useTranslation('findings')
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const doc = useFlowStore(s => s.document)
  // View-only shares hide source-writing actions (apply fix / suppress in
  // file); triage + AI suggestions stay available.
  const readOnly = useFlowStore(s => s.readOnly)
  const suppressFinding = useAnalysisStore(s => s.suppressFinding)
  const unsuppressFinding = useAnalysisStore(s => s.unsuppressFinding)
  const toast = useToast()
  const [showHint, setShowHint] = useState(false)
  const isDesktop = isTauri()

  const {showRelated, related, relatedLoading, relatedError, handleRelated, fetchRelated} = useRelatedFindings(finding)
  const {showComments, comments, commentBody, setCommentBody, commentLoading, handleComments, handleSubmitComment} =
    useFindingComments(finding, doc)
  const {applyingFix, preview, handleApplyFix, doApplyFix, closePreview} = useFindingFix(finding, doc)

  const handleJump = () => {
    selectSubflow(finding.subflowId)
    selectBlock(finding.blockId)
  }

  const handleSuppress = () => {
    suppressFinding(finding, t('card.dismissedByUser'))
    toast.warning(t('toasts.findingSuppressed'), {
      action: {label: 'Undo', onClick: () => unsuppressFinding(finding)},
    })
  }

  const loc = blockLookup.get(finding.blockId)
  const blockLabel = loc?.name ?? finding.blockId.slice(0, 8)
  const subflowLabel = loc?.subflowName

  return (
    <div className="hover:bg-surface-2/50 transition-colors group">
      <div className="px-4 py-2 pl-9 flex items-center gap-2 flex-wrap">
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="text-xs text-text-primary font-mono truncate">{blockLabel}</span>
            {finding.category && (
              <span
                className={`text-2xs font-bold uppercase tracking-wider px-1.5 py-0.5 rounded ${categoryBadgeClass(finding.category)}`}
              >
                {finding.category}
              </span>
            )}
            {finding.confidence && finding.confidence !== 'medium' && (
              <span
                className={clsx(
                  'text-2xs uppercase tracking-wider px-1 py-0.5 rounded border',
                  finding.confidence === 'low'
                    ? 'text-semantic-warning border-semantic-warning/30 bg-semantic-warning/5'
                    : 'text-semantic-success border-semantic-success/30 bg-semantic-success/5',
                )}
                title={
                  finding.confidence === 'low'
                    ? 'Low-confidence heuristic — possible false positive'
                    : t('card.highConfidence')
                }
              >
                {finding.confidence}
              </span>
            )}
          </div>
          {subflowLabel && <span className="text-2xs text-text-tertiary ml-2">in {subflowLabel}</span>}
        </div>

        <FindingTriageMenu finding={finding} />

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
              : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3',
          )}
          title={t('card.showRelated')}
        >
          <GitBranch size={10} />
          Related
        </button>

        <button
          onClick={handleComments}
          className={clsx(
            'flex items-center gap-1 text-2xs px-1.5 py-1 rounded transition-colors shrink-0',
            showComments
              ? 'text-brand-400 bg-brand-500/10'
              : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3',
          )}
          title={t('card.comments')}
          aria-label={t('card.comments')}
          aria-pressed={showComments}
        >
          <MessageSquare size={10} />
          {comments && comments.length > 0 && comments.length}
        </button>

        {finding.autoFixHint && (
          <button
            onClick={() => setShowHint(h => !h)}
            aria-expanded={showHint}
            className="flex items-center gap-1 text-2xs text-semantic-success hover:text-semantic-success px-1.5 py-1 rounded hover:bg-semantic-success/10 transition-colors shrink-0"
            title={t('card.showFix')}
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

        {finding.autoFix && finding.autoFix !== 'suppress' && !readOnly && (
          <button
            onClick={() => {
              if (finding.autoFix) void handleApplyFix(finding.autoFix)
            }}
            disabled={applyingFix}
            className="flex items-center gap-1 text-2xs text-semantic-success hover:text-semantic-success px-1.5 py-1 rounded hover:bg-semantic-success/10 transition-colors shrink-0 disabled:opacity-50"
            title={isDesktop ? t('card.applyFixLocalTitle') : t('card.applyFixCloudTitle')}
          >
            <Wrench size={10} />
            {applyingFix ? t('card.applying') : t('card.applyFix')}
          </button>
        )}

        {isDesktop && !readOnly && (
          <button
            onClick={() => handleApplyFix('suppress')}
            disabled={applyingFix}
            className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-1.5 py-1 rounded hover:bg-brand-500/10 transition-colors shrink-0 disabled:opacity-50"
            title={t('card.suppressInFileTitle')}
          >
            <FilePen size={10} />
            {applyingFix ? t('card.applying') : t('card.suppressInFile')}
          </button>
        )}

        <button
          onClick={handleSuppress}
          aria-label={t('card.suppressAria')}
          className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
          title={t('card.suppressTitle')}
        >
          <EyeOff size={10} />
        </button>
      </div>

      {showHint && finding.autoFixHint && (
        <div className="mx-4 mb-2 ml-9 px-3 py-2 bg-semantic-success/10 border border-semantic-success/20 rounded text-2xs text-semantic-success font-mono leading-relaxed">
          {finding.autoFixHint}
        </div>
      )}

      {showRelated && (
        <FindingRelatedPanel
          related={related}
          relatedLoading={relatedLoading}
          relatedError={relatedError}
          onRetry={fetchRelated}
        />
      )}

      {showComments && (
        <FindingCommentsPanel
          comments={comments}
          commentLoading={commentLoading}
          commentBody={commentBody}
          onCommentBodyChange={setCommentBody}
          onSubmit={handleSubmitComment}
        />
      )}

      <PatchPreviewModal
        open={preview.open}
        original={preview.original}
        patched={preview.patched}
        fixType={preview.fixType}
        onApply={() => doApplyFix(preview.fixType)}
        onCancel={closePreview}
      />
    </div>
  )
}

export default React.memo(FindingCard)
