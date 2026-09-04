import {useEffect, useRef, useCallback} from 'react'
import {flowApi} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {isTauri} from '@/platform/guards'
import {logger} from '@/lib/logger'
import type {FlowDocument} from '@/types'

// Desktop watcher cadence: how often the cheap stat signal is polled. The
// analyze→fix-in-PAD→recheck loop's painful step is remembering to click
// Reimport; 4s makes the reload feel immediate without a hot poll.
const WATCH_INTERVAL_MS = 4000

interface SourceMeta {
  size: number
  modTime: string
  files: number
}

// useFileWatcher auto-reimports a LOCAL desktop flow when its file changes on
// disk (the user edited in Power Automate Desktop and saved). Detection polls
// GET /api/flow/source-meta — size + mtime, no content read. Own writes are
// ignored by re-seeding the baseline whenever the store's document identity
// changes (every app mutation ends in setDocument), so only EXTERNAL edits
// fire the reload. Cloud flows are skipped (Files=0; their updates arrive on
// the websocket flow-change channel).
export function useFileWatcher(doc: FlowDocument | null) {
  const baselineRef = useRef<SourceMeta | null>(null)
  const settlingRef = useRef(false)
  const inFlightRef = useRef(false)

  // Re-seed on doc identity change: covers the initial load and every
  // app-driven write (fix/save/restore all end in a new document object).
  useEffect(() => {
    baselineRef.current = null // force a re-seed on the next poll
    settlingRef.current = false
  }, [doc])

  const reload = useCallback(async (flowId: string) => {
    if (inFlightRef.current) return
    inFlightRef.current = true
    const st = useAnalysisStore.getState()
    const gen = st.beginAnalyzing()
    try {
      const fresh = await flowApi.reimport(flowId)
      // Same-flow refresh (F1.1): a disk change must not reset the user's
      // subflow/chat/selection — applyRemoteDocumentUpdate preserves them.
      useFlowStore.getState().applyRemoteDocumentUpdate(fresh)
      const r = await analysisApi.analyzeFlowById(fresh.id)
      if (r) useAnalysisStore.getState().setReport(fresh.id, r)
      logger.warn('File changed on disk — reimported', flowId)
    } catch (err) {
      // A save-in-progress or transient lock: re-seed so we don't loop on
      // the same failing state; the next user action re-syncs.
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
    // Desktop + local file only. Cloud docs have no FilePath; their changes
    // arrive via the websocket flow-change sync instead.
    if (!isTauri() || !doc?.filePath || !doc.id) return

    const flowId = doc.id
    let cancelled = false

    const tick = async () => {
      if (cancelled || inFlightRef.current) return
      let meta: SourceMeta
      try {
        meta = await flowApi.getSourceMeta(flowId)
      } catch {
        return // backend restarting or auth hiccup: try next tick
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
      // Editors write in bursts ( PAD saves bump mtime several times):
      // require one UNCHANGED poll after the last change before reloading,
      // then seed the new baseline so the reload itself doesn't re-trigger.
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
