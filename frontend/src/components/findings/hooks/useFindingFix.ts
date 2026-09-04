import {useTranslation} from 'react-i18next'
import {useState} from 'react'
import {analysisApi, flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useToast} from '@/components/shared'
import type {Finding, AnalysisReport, FlowDocument} from '@/types'

interface PreviewState {
  open: boolean
  original: string
  patched: string
  fixType: string
}

// handleApplyFix/doApplyFix implement the end-to-end apply-fix flow: call the
// backend to edit the flow's source file (desktop), replace the doc with the
// re-parsed result, and re-analyze — so the fix is written into the file (not
// just a UI hint) and the finding is resolved. Dispatches on finding.autoFix
// (e.g. 'wrap-error-handler') or the explicit 'suppress' type.
export function useFindingFix(finding: Finding, doc: FlowDocument | null) {
  const {t} = useTranslation('findings')
  const setDocument = useFlowStore(s => s.setDocument)
  const beginAnalyzing = useAnalysisStore(s => s.beginAnalyzing)
  const setAnalyzing = useAnalysisStore(s => s.setAnalyzing)
  const setReport = useAnalysisStore(s => s.setReport)
  const toast = useToast()

  const [applyingFix, setApplyingFix] = useState(false)
  const [preview, setPreview] = useState<PreviewState>({open: false, original: '', patched: '', fixType: ''})

  const handleApplyFix = async (fixType: string) => {
    if (!doc) return
    // Preview: fetch the before/after source text without writing
    try {
      const variable = fixType === 'init-variable' ? (finding.metadata?.variable as string | undefined) : undefined
      const property =
        fixType === 'replace-with-variable' || fixType === 'parameterize-sql'
          ? (finding.metadata?.property as string | undefined)
          : undefined
      const result = await flowApi.previewFix(doc.id, finding.blockId, fixType, finding.ruleId, variable, property)
      setPreview({open: true, original: result.original, patched: result.patched, fixType})
    } catch {
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
      const property =
        fixType === 'replace-with-variable' || fixType === 'parameterize-sql'
          ? (finding.metadata?.property as string | undefined)
          : undefined
      const updated = await flowApi.applyFix(doc.id, finding.blockId, fixType, finding.ruleId, variable, property)
      setDocument(updated)
      // Fix was applied successfully. Re-analysis is best-effort — if it
      // fails the editor already shows the fixed source; the stale report
      // refreshes on the next manual analysis.
      try {
        const r = await analysisApi.analyzeFlow()
        if (r) setReport(updated.id, r as AnalysisReport)
        toast.success(t('toasts.fixApplied'), {description: t('toasts.fixAppliedReanalyzed')})
      } catch {
        toast.success(t('toasts.fixApplied'), {description: t('toasts.fixAppliedNeedsAnalyze')})
      }
    } catch (err) {
      toast.error(t('toasts.fixFailed'), {description: String(err)})
    } finally {
      if (useAnalysisStore.getState().analyzingGen === gen) setAnalyzing(false)
      setApplyingFix(false)
    }
  }

  const closePreview = () => setPreview(p => ({...p, open: false}))

  return {applyingFix, preview, handleApplyFix, doApplyFix, closePreview}
}
