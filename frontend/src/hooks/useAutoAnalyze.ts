import {useEffect, useRef} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import type {AnalysisReport} from '@/types'

/**
 * When a flow finishes loading AND the user has Settings → Rules →
 * "Auto-analyze on flow open" enabled, fire the same analysis pipeline that
 * the Findings tab uses so results are ready by the time the user clicks
 * over. Skips if there's already a report cached for the doc id or another
 * analysis is in flight. Errors are swallowed — this is a background
 * convenience, not a user-initiated action.
 */
export function useAutoAnalyze(): void {
  const docId = useFlowStore(s => s.document?.id ?? null)
  const autoAnalyze = useSettingsStore(s => s.settings.analysis.autoAnalyzeOnOpen)
  const settingsLoaded = useSettingsStore(s => s.isLoaded)
  // Guards against the effect re-firing for the same doc id while the
  // network call is still pending (React StrictMode double-invokes effects).
  const inflightRef = useRef<string | null>(null)

  useEffect(() => {
    if (!settingsLoaded || !autoAnalyze || !docId) return
    if (inflightRef.current === docId) return

    const analysis = useAnalysisStore.getState()
    if (analysis.reports.has(docId)) return  // already analyzed
    if (analysis.isAnalyzing) return         // something else is running

    inflightRef.current = docId
    const gen = analysis.beginAnalyzing()
    analysis.setProgress({current: 0, total: 0, ruleName: ''})

    analysisApi.analyzeFlow()
      .then((r) => {
        if (r) useAnalysisStore.getState().setReport(docId, r as unknown as AnalysisReport)
      })
      .catch((err) => {
        logger.warn('auto-analyze failed:', err)
      })
      .finally(() => {
        if (useAnalysisStore.getState().analyzingGen === gen) {
          useAnalysisStore.getState().setAnalyzing(false)
        }
        if (inflightRef.current === docId) inflightRef.current = null
      })
  }, [docId, autoAnalyze, settingsLoaded])
}
