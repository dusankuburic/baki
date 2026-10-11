import {useEffect, useRef} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'

export function useAutoAnalyze(): void {
  const docId = useFlowStore(s => s.document?.id ?? null)
  const autoAnalyze = useSettingsStore(s => s.settings.analysis.autoAnalyzeOnOpen)
  const settingsLoaded = useSettingsStore(s => s.isLoaded)
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const inflightRef = useRef<string | null>(null)
  const attemptedRef = useRef<Set<string>>(new Set())

  useEffect(() => {
    if (!settingsLoaded || !autoAnalyze || !docId) return
    if (inflightRef.current === docId) return
    if (attemptedRef.current.has(docId)) return

    const analysis = useAnalysisStore.getState()
    if (analysis.reports.has(docId)) return
    if (analysis.isAnalyzing) return

    inflightRef.current = docId
    attemptedRef.current.add(docId)
    const gen = analysis.beginAnalyzing()
    analysis.setProgress({current: 0, total: 0, ruleName: ''})

    analysisApi
      .analyzeFlow()
      .then(r => {
        if (r) useAnalysisStore.getState().setReport(docId, r)
      })
      .catch(err => {
        logger.warn('auto-analyze failed:', err)
      })
      .finally(() => {
        if (useAnalysisStore.getState().analyzingGen === gen) {
          useAnalysisStore.getState().setAnalyzing(false)
        }
        if (inflightRef.current === docId) inflightRef.current = null
      })
  }, [docId, autoAnalyze, settingsLoaded, isAnalyzing])
}
