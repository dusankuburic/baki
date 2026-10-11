import {useTranslation} from 'react-i18next'
import {useState} from 'react'
import {analysisApi, flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useToast} from '@/components/shared'
import type {Finding, AnalysisReport, FlowDocument} from '@/types'
import {getErrorMessage} from '@/lib/getErrorMessage'

interface PreviewState {
  open: boolean
  original: string
  patched: string
  fixType: string
}

const VARIABLE_FIXES = new Set(['init-variable', 'mask-sensitive-variable'])
const PROPERTY_FIXES = new Set(['replace-with-variable', 'parameterize-sql'])

export function fixArgs(finding: Finding, fixType: string): {variable?: string; property?: string} {
  return {
    variable: VARIABLE_FIXES.has(fixType) ? (finding.metadata?.variable as string | undefined) : undefined,
    property: PROPERTY_FIXES.has(fixType) ? (finding.metadata?.property as string | undefined) : undefined,
  }
}

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
    try {
      const {variable, property} = fixArgs(finding, fixType)
      const result = await flowApi.previewFix(doc.id, finding.blockId, fixType, finding.ruleId, variable, property)
      setPreview({open: true, original: result.original, patched: result.patched, fixType})
    } catch {
      await doApplyFix(fixType, false)
    }
  }

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
      toast.error(failTitle, {description: getErrorMessage(err)})
    } finally {
      if (useAnalysisStore.getState().analyzingGen === gen) setAnalyzing(false)
      setApplyingFix(false)
    }
  }

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
