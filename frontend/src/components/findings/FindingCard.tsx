import React, {useState, useCallback, useRef, useEffect} from 'react'
import clsx from 'clsx'
import type {Finding, AnalysisReport, TriageStatus, FindingComment} from '@/types'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore, findingKey} from '@/stores/analysisStore'
import type {BlockLookup} from '@/lib/tree'
import {analysisApi, flowApi} from '@/api'
import {logger} from '@/lib/logger'
import {categoryBadgeClass} from '@/lib/findingsColors'
import {useToast} from '@/components/shared'
import {ArrowRight, Sparkles, EyeOff, Wrench, ChevronDown, GitBranch, FilePen, CircleDot, MessageSquare} from 'lucide-react'
import PatchPreviewModal from './PatchPreviewModal'

interface Props {
  finding: Finding
  blockLookup: BlockLookup
  onFixWithAI?: (finding: Finding) => void
}

function FindingCard({finding, blockLookup, onFixWithAI}: Props) {
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const setDocument = useFlowStore(s => s.setDocument)
  const doc = useFlowStore(s => s.document)
  const suppressFinding = useAnalysisStore(s => s.suppressFinding)
  const unsuppressFinding = useAnalysisStore(s => s.unsuppressFinding)
  const beginAnalyzing = useAnalysisStore(s => s.beginAnalyzing)
  const setAnalyzing = useAnalysisStore(s => s.setAnalyzing)
  const setReport = useAnalysisStore(s => s.setReport)
  const toast = useToast()
  const [showHint, setShowHint] = useState(false)
  const [showRelated, setShowRelated] = useState(false)
  const [related, setRelated] = useState<Finding[] | null>(null)
  const [relatedLoading, setRelatedLoading] = useState(false)
  const [relatedError, setRelatedError] = useState(false)
  const [applyingFix, setApplyingFix] = useState(false)
  const [showTriage, setShowTriage] = useState(false)
  const [preview, setPreview] = useState<{open: boolean, original: string, patched: string, fixType: string}>({
    open: false, original: '', patched: '', fixType: '',
  })
  const [showComments, setShowComments] = useState(false)
  const triageRef = useRef<HTMLDivElement>(null)

  // Close triage dropdown on click-outside so multiple cards can't have
  // dropdowns open simultaneously.
  useEffect(() => {
    if (!showTriage) return
    const handler = (e: MouseEvent) => {
      if (triageRef.current && !triageRef.current.contains(e.target as Node)) {
        setShowTriage(false)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [showTriage])
  const [comments, setComments] = useState<FindingComment[] | null>(null)
  const [commentBody, setCommentBody] = useState('')
  const [commentLoading, setCommentLoading] = useState(false)

  const triageMap = useAnalysisStore(s => s.triageMap)
  const setFindingTriage = useAnalysisStore(s => s.setFindingTriage)
  const triageStatus: TriageStatus = triageMap.get(findingKey(finding))?.status ?? 'open'

  const handleSetTriage = (status: TriageStatus) => {
    setShowTriage(false)
    if (status === triageStatus) return
    setFindingTriage(finding, status)
    if (status === 'suppressed') {
      toast.info('Finding suppressed', {
        action: {label: 'Undo', onClick: () => setFindingTriage(finding, 'open')},
      })
    }
  }

  const handleJump = () => {
    selectSubflow(finding.subflowId)
    selectBlock(finding.blockId)
  }

  const handleSuppress = () => {
    suppressFinding(finding, 'Dismissed by user')
    toast.warning('Finding suppressed', {
      action: {label: 'Undo', onClick: () => unsuppressFinding(finding)},
    })
  }

  // handleApplyFix is the end-to-end apply-fix: call the backend to edit the
  // flow's source file (desktop), replace the doc with the re-parsed result,
  // and re-analyze — so the fix is written into the file (not just a UI hint)
  // and the finding is resolved. Dispatches on finding.autoFix (e.g.
  // 'wrap-error-handler') or the explicit 'suppress' type.
  const handleApplyFix = async (fixType: string) => {
    if (!doc) return
    // Preview: fetch the before/after source text without writing
    try {
      const variable = fixType === 'init-variable' ? (finding.metadata?.variable as string | undefined) : undefined
      const property = fixType === 'replace-with-variable' ? (finding.metadata?.property as string | undefined) : undefined
      const result = await flowApi.previewFix(doc.id, finding.blockId, fixType, finding.ruleId, variable, property)
      setPreview({open: true, original: result.original, patched: result.patched, fixType})
    } catch (err) {
      // Preview failed (e.g. cloud mode where preview-fix is blocked) — apply
      // directly without a preview, preserving the original UX.
      await doApplyFix(fixType)
    }
  }

  const doApplyFix = async (fixType: string) => {
    if (!doc) return
    setApplyingFix(true)
    setPreview(p => ({...p, open: false}))
    const gen = beginAnalyzing()
    try {
      const variable = fixType === 'init-variable' ? (finding.metadata?.variable as string | undefined) : undefined
      const property = fixType === 'replace-with-variable' ? (finding.metadata?.property as string | undefined) : undefined
      const updated = await flowApi.applyFix(doc.id, finding.blockId, fixType, finding.ruleId, variable, property)
      setDocument(updated)
      const r = await analysisApi.analyzeFlow()
      if (r) setReport(updated.id, r as AnalysisReport)
      toast.success('Fix applied', {
        description: 'Flow file updated and re-analyzed.',
      })
    } catch (err) {
      toast.error('Could not apply fix', {description: String(err)})
    } finally {
      if (useAnalysisStore.getState().analyzingGen === gen) setAnalyzing(false)
      setApplyingFix(false)
    }
  }

  const fetchRelated = useCallback(async () => {
    setRelatedLoading(true)
    setRelatedError(false)
    try {
      const result = await analysisApi.getRelatedFindings(finding.blockId)
      setRelated(result.filter(f => f.id !== finding.id))
    } catch (err) {
      logger.warn('Failed to load related findings', err)
      setRelatedError(true)
    } finally {
      setRelatedLoading(false)
    }
  }, [finding.id, finding.blockId])

  const handleRelated = useCallback(() => {
    if (showRelated) {
      setShowRelated(false)
      return
    }
    setShowRelated(true)
    if (related === null && !relatedLoading) fetchRelated()
  }, [showRelated, related, relatedLoading, fetchRelated])

  const handleComments = useCallback(async () => {
    if (showComments) {
      setShowComments(false)
      return
    }
    setShowComments(true)
    if (comments === null && !commentLoading) {
      setCommentLoading(true)
      try {
        const key = findingKey(finding)
        const result = await analysisApi.listComments(doc?.id ?? '', key)
        setComments(result || [])
      } catch {
        setComments([])
      } finally {
        setCommentLoading(false)
      }
    }
  }, [showComments, comments, commentLoading, finding, doc])

  const handleSubmitComment = useCallback(async () => {
    if (!commentBody.trim() || !doc) return
    const body = commentBody.trim()
    setCommentBody('')
    try {
      const key = findingKey(finding)
      const comment = await analysisApi.addComment(doc.id, key, body)
      setComments(prev => [...(prev ?? []), comment])
    } catch (err) {
      toast.error('Failed to add comment', {description: String(err)})
    }
  }, [commentBody, doc, finding, toast])

  const loc = blockLookup.get(finding.blockId)
  const blockLabel = loc?.name ?? finding.blockId.slice(0, 8)
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
            {finding.confidence && finding.confidence !== 'medium' && (
              <span
                className={clsx(
                  'text-2xs uppercase tracking-wider px-1 py-0.5 rounded border',
                  finding.confidence === 'low'
                    ? 'text-amber-400 border-amber-500/30 bg-amber-500/5'
                    : 'text-emerald-400 border-emerald-500/30 bg-emerald-500/5'
                )}
                title={
                  finding.confidence === 'low'
                    ? 'Low-confidence heuristic — possible false positive'
                    : 'High-confidence detection'
                }
              >
                {finding.confidence}
              </span>
            )}
          </div>
          {subflowLabel && (
            <span className="text-2xs text-text-tertiary ml-2">in {subflowLabel}</span>
          )}
        </div>

        {triageStatus !== 'open' && (
          <span
            className={clsx(
              'text-2xs uppercase tracking-wider px-1.5 py-0.5 rounded border',
              triageStatus === 'suppressed' && 'bg-surface-3 text-text-tertiary border-border-subtle',
              triageStatus === 'acknowledged' && 'text-blue-400 border-blue-500/30 bg-blue-500/5',
              triageStatus === 'in_progress' && 'text-amber-400 border-amber-500/30 bg-amber-500/5',
              triageStatus === 'resolved' && 'text-emerald-400 border-emerald-500/30 bg-emerald-500/5',
            )}
          >
            {triageStatus.replace('_', ' ')}
          </span>
        )}

        <div className="relative" ref={triageRef}>
          <button
            onClick={() => setShowTriage(s => !s)}
            className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
            title="Set triage status"
          >
            <CircleDot size={10} />
          </button>
          {showTriage && (
            <div className="absolute right-0 top-full mt-1 z-20 bg-surface-2 border border-border-subtle rounded-md shadow-lg py-0.5 min-w-32">
              {([
                {s: 'open' as TriageStatus, label: 'Open'},
                {s: 'acknowledged' as TriageStatus, label: 'Acknowledged'},
                {s: 'in_progress' as TriageStatus, label: 'In Progress'},
                {s: 'resolved' as TriageStatus, label: 'Resolved'},
                {s: 'suppressed' as TriageStatus, label: 'Suppressed'},
              ]).map(({s, label}) => (
                <button
                  key={s}
                  onClick={() => handleSetTriage(s)}
                  className={clsx(
                    'block w-full text-left text-2xs px-2.5 py-1 hover:bg-surface-3 transition-colors',
                    triageStatus === s ? 'text-brand-400 font-medium' : 'text-text-secondary',
                  )}
                >
                  {label}
                </button>
              ))}
            </div>
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

        <button
          onClick={handleComments}
          className={clsx(
            'flex items-center gap-1 text-2xs px-1.5 py-1 rounded transition-colors shrink-0',
            showComments
              ? 'text-brand-400 bg-brand-500/10'
              : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-3'
          )}
          title="Comments on this finding"
          aria-label="Comments on this finding"
          aria-pressed={showComments}
        >
          <MessageSquare size={10} />
          {comments && comments.length > 0 && comments.length}
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

        {finding.autoFix && finding.autoFix !== 'suppress' && (
          <button
            onClick={() => handleApplyFix(finding.autoFix!)}
            disabled={applyingFix}
            className="flex items-center gap-1 text-2xs text-emerald-400 hover:text-emerald-300 px-1.5 py-1 rounded hover:bg-emerald-500/10 transition-colors shrink-0 disabled:opacity-50"
            title="Apply this fix to the flow source file and re-analyze (desktop)"
          >
            <Wrench size={10} />
            {applyingFix ? 'Applying…' : 'Apply fix'}
          </button>
        )}

        <button
          onClick={() => handleApplyFix('suppress')}
          disabled={applyingFix}
          className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-1.5 py-1 rounded hover:bg-brand-500/10 transition-colors shrink-0 disabled:opacity-50"
          title="Write a pad-ignore directive into the flow source file (desktop) and re-analyze"
        >
          <FilePen size={10} />
          {applyingFix ? 'Applying…' : 'Suppress in file'}
        </button>

        <button
          onClick={handleSuppress}
          aria-label="Suppress this finding"
          className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-secondary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors shrink-0"
          title="Suppress this finding (UI only)"
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
          ) : relatedError ? (
            <div className="flex items-center gap-2 text-2xs text-text-tertiary">
              <span>Couldn't load related findings.</span>
              <button onClick={fetchRelated} className="text-brand-400 hover:text-brand-300 font-medium">
                Retry
              </button>
            </div>
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

      {showComments && (
        <div className="mx-4 mb-2 ml-9 px-3 py-2 bg-surface-3 border border-border-subtle rounded space-y-1.5">
          <span className="text-2xs font-bold uppercase tracking-wider text-text-tertiary">
            Comments
          </span>
          {commentLoading ? (
            <span className="text-2xs text-text-tertiary">Loading…</span>
          ) : comments && comments.length > 0 ? (
            comments.map(c => (
              <div key={c.id} className="text-2xs space-y-0.5">
                <div className="flex items-center gap-2">
                  <span className="text-text-secondary font-medium">{c.authorName || c.authorId.slice(0, 8)}</span>
                  <span className="text-text-disabled">{new Date(c.createdAt).toLocaleDateString()}</span>
                </div>
                <p className="text-text-tertiary">{c.body}</p>
              </div>
            ))
          ) : (
            <span className="text-2xs text-text-tertiary">No comments yet.</span>
          )}
          <div className="flex items-center gap-1.5 pt-1">
            <input
              type="text"
              value={commentBody}
              onChange={e => setCommentBody(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); void handleSubmitComment() } }}
              placeholder="Add a comment…"
              className="flex-1 bg-surface-2 border border-border-subtle rounded px-2 py-1 text-2xs text-text-primary placeholder:text-text-disabled focus:outline-none focus:border-brand-500/50"
            />
            <button
              onClick={handleSubmitComment}
              disabled={!commentBody.trim()}
              className="text-2xs text-brand-400 hover:text-brand-300 px-2 py-1 rounded hover:bg-brand-500/10 transition-colors disabled:opacity-50"
            >
              Post
            </button>
          </div>
        </div>
      )}

      <PatchPreviewModal
        open={preview.open}
        original={preview.original}
        patched={preview.patched}
        fixType={preview.fixType}
        onApply={() => doApplyFix(preview.fixType)}
        onCancel={() => setPreview(p => ({...p, open: false}))}
      />
    </div>
  )
}

export default React.memo(FindingCard)
