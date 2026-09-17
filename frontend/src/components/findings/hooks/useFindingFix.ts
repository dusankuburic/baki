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

// Fixers that read the finding's metadata. mask-sensitive-variable was missing
// from the variable list, so "Apply fix" on sensitive-exposure sent no variable,
// the server's fixer returned an empty patch, and the card reported success on
// an unchanged flow (the server now also refuses an empty patch).
const VARIABLE_FIXES = new Set(['init-variable', 'mask-sensitive-variable'])
const PROPERTY_FIXES = new Set(['replace-with-variable', 'parameterize-sql'])

export function fixArgs(finding: Finding, fixType: string): {variable?: string; property?: string} {
  return {
    variable: VARIABLE_FIXES.has(fixType) ? (finding.metadata?.variable as string | undefined) : undefined,
    property: PROPERTY_FIXES.has(fixType) ? (finding.metadata?.property as string | undefined) : undefined,
  }
}

// handleApplyFix/doApplyFix implement the end-to-end apply-fix flow: call the
// backend to edit the flow's source file (desktop), replace the doc with the
// re-parsed result, and re-analyze — so the fix is written into the file (not
// just a UI hint) and the finding is resolved. Dispatches on finding.autoFix
// (e.g. 'wrap-error-handler'); handleSuppressInSource writes a pad-ignore.
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
      const {variable, property} = fixArgs(finding, fixType)
      const result = await flowApi.previewFix(doc.id, finding.blockId, fixType, finding.ruleId, variable, property)
      setPreview({open: true, original: result.original, patched: result.patched, fixType})
    } catch {
      // Preview failed — apply directly without a preview, preserving the
      // original UX. No diff was shown, so this is NOT the unsafe opt-in: a
      // behaviour-changing fixer is refused by the server here (400) and the
      // toast says why.
      await doApplyFix(fixType, false)
    }
  }

  // writeAndReanalyze runs one source-writing call, swaps in the re-parsed
  // document and re-analyzes. Re-analysis is best-effort — if it fails the
  // editor already shows the written source; the stale report refreshes on the
  // next manual analysis.
  const writeAndReanalyze = async (write: () => Promise<FlowDocument>, successTitle: string, failTitle: string) => {
    setApplyingFix(true)
    const gen = beginAnalyzing()
    try {
      const updated = await write()
      setDocument(updated)
      try {
        const r = await analysisApi.analyzeFlow()
        if (r) setReport(updated.id, r as AnalysisReport)
        toast.success(successTitle, {description: t('toasts.fixAppliedReanalyzed')})
      } catch {
        toast.success(successTitle, {description: t('toasts.fixAppliedNeedsAnalyze')})
      }
    } catch (err) {
      toast.error(failTitle, {description: String(err)})
    } finally {
      if (useAnalysisStore.getState().analyzingGen === gen) setAnalyzing(false)
      setApplyingFix(false)
    }
  }

  // reviewed: true when the caller has shown the preview diff for this fix —
  // that is the only path allowed to send `unsafe: true`, the opt-in the
  // server requires for behaviour-changing fixers.
  const doApplyFix = async (fixType: string, reviewed: boolean) => {
    if (!doc) return
    setPreview(p => ({...p, open: false}))
    const {variable, property} = fixArgs(finding, fixType)
    await writeAndReanalyze(
      () => flowApi.applyFix(doc.id, finding.blockId, fixType, finding.ruleId, variable, property, reviewed),
      t('toasts.fixApplied'),
      t('toasts.fixFailed'),
    )
  }

  // handleSuppressInSource (desktop) writes `# pad-ignore[rule]` before the
  // block through flowApi.suppressInSource, so the suppression travels with
  // the file (analyzer, CLI gate, CI) instead of living in this session only.
  // That wrapper had no caller: "Suppress in file" went through the generic
  // preview/apply-fix pair, a diff dialog for a one-line comment.
  const handleSuppressInSource = async () => {
    if (!doc) return
    await writeAndReanalyze(
      () => flowApi.suppressInSource(doc.id, finding.blockId, finding.ruleId),
      t('toasts.suppressedInFile'),
      t('toasts.suppressInFileFailed'),
    )
  }

  const closePreview = () => setPreview(p => ({...p, open: false}))

  return {applyingFix, preview, handleApplyFix, doApplyFix, handleSuppressInSource, closePreview}
}
