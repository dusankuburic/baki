import {useEffect, useRef} from 'react'
import {onFlowChanged, usePresenceStore} from '@/stores/presenceStore'
import {useFlowStore} from '@/stores/flowStore'
import {libraryApi} from '@/api/library'
import {logger} from '@/lib/logger'
import type {FlowDocument} from '@/types'

export const FLOW_CHANGE_DEBOUNCE_MS = 300

export function useFlowChangeSync(): void {
  const flowId = usePresenceStore(s => s.flowId)
  const status = usePresenceStore(s => s.status)
  const appliedRef = useRef<{flowId: string | null; version: number}>({flowId: null, version: 0})

  useEffect(() => {
    if (!flowId || status !== 'connected') return
    if (appliedRef.current.flowId !== flowId) appliedRef.current = {flowId, version: 0}
    let timer: ReturnType<typeof setTimeout> | null = null
    let inFlight: AbortController | null = null
    let pendingVersion = 0
    let reloadSeq = 0

    const reload = () => {
      timer = null
      const currentDoc = useFlowStore.getState().document
      if (!currentDoc || currentDoc.id !== flowId) return

      inFlight?.abort()
      const controller = new AbortController()
      inFlight = controller
      const seq = ++reloadSeq
      const target = pendingVersion

      Promise.all([libraryApi.get(flowId, controller.signal), libraryApi.getContent(flowId, controller.signal)])
        .then(([meta, content]) => {
          if (seq !== reloadSeq) return

          const version = meta.version ?? target
          appliedRef.current = {flowId, version}
          useFlowStore.setState({libraryVersion: version})

          useFlowStore.getState().applyRemoteDocumentUpdate(content as FlowDocument)
        })
        .catch(err => {
          if (seq !== reloadSeq || controller.signal.aborted) return
          logger.warn('Failed to reload flow after remote change', err)
        })
        .finally(() => {
          if (inFlight === controller) inFlight = null
        })
    }

    const unsubscribe = onFlowChanged((version: number) => {
      if (version === appliedRef.current.version || version === pendingVersion) return
      pendingVersion = version
      if (timer) clearTimeout(timer)
      timer = setTimeout(reload, FLOW_CHANGE_DEBOUNCE_MS)
    })

    return () => {
      unsubscribe()
      if (timer) clearTimeout(timer)
      reloadSeq++
      inFlight?.abort()
    }
  }, [flowId, status])
}
