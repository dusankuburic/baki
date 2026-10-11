import {useEffect, useRef, useCallback} from 'react'
import {flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {isTauri} from '@/platform/guards'
import {logger} from '@/lib/logger'
import type {FlowDocument} from '@/types'

const WATCH_INTERVAL_MS = 4000

interface SourceMeta {
  size: number
  modTime: string
  files: number
}

export function useFileWatcher(doc: FlowDocument | null) {
  const baselineRef = useRef<SourceMeta | null>(null)
  const settlingRef = useRef(false)
  const inFlightRef = useRef(false)

  useEffect(() => {
    baselineRef.current = null
    settlingRef.current = false
  }, [doc])

  const reload = useCallback(async (flowId: string) => {
    if (inFlightRef.current) return
    inFlightRef.current = true
    const st = useAnalysisStore.getState()
    const gen = st.beginAnalyzing()
    try {
      const fresh = await flowApi.reimport(flowId)
      useFlowStore.getState().applyRemoteDocumentUpdate(fresh)
      const r = await analysisApi.analyzeFlowById(fresh.id)
      if (r) useAnalysisStore.getState().setReport(fresh.id, r)
      logger.warn('File changed on disk — reimported', flowId)
    } catch (err) {
      baselineRef.current = null
      logger.warn('Auto-reimport failed', err)
    } finally {
      if (useAnalysisStore.getState().analyzingGen === gen) {
        useAnalysisStore.getState().setAnalyzing(false)
      }
      inFlightRef.current = false
    }
  }, [])

  useEffect(() => {
    if (!isTauri() || !doc?.filePath || !doc.id) return

    const flowId = doc.id
    let cancelled = false

    const tick = async () => {
      if (cancelled || inFlightRef.current) return
      let meta: SourceMeta
      try {
        meta = await flowApi.getSourceMeta(flowId)
      } catch {
        return
      }
      if (cancelled || !meta) return

      if (baselineRef.current === null) {
        baselineRef.current = meta
        return
      }
      const base = baselineRef.current
      const changed = meta.files !== base.files || meta.size !== base.size || meta.modTime !== base.modTime
      if (!changed) {
        settlingRef.current = false
        return
      }
      if (!settlingRef.current) {
        settlingRef.current = true
        return
      }
      settlingRef.current = false
      baselineRef.current = meta
      void reload(flowId)
    }

    const timer = setInterval(tick, WATCH_INTERVAL_MS)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [doc?.id, doc?.filePath, reload])
}
