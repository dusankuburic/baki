import {useEffect, useRef} from 'react'
import {onFlowChanged, usePresenceStore} from '@/stores/presenceStore'
import {useFlowStore} from '@/stores/flowStore'
import {libraryApi} from '@/api/library'
import {logger} from '@/lib/logger'
import type {FlowDocument} from '@/types'

export function useFlowChangeSync(): void {
  const flowId = usePresenceStore(s => s.flowId)
  const status = usePresenceStore(s => s.status)
  const lastVersionRef = useRef(0)
  const reloadSeqRef = useRef(0)

  useEffect(() => {
    if (!flowId || status !== 'connected') return

    return onFlowChanged((version: number) => {
      if (version === lastVersionRef.current) return
      lastVersionRef.current = version

      const currentDoc = useFlowStore.getState().document
      if (!currentDoc || currentDoc.id !== flowId) return

      // Increment the sequence counter so that if two reload requests
      // fire in rapid succession (v4 then v5), only the latest one's
      // response is applied — preventing out-of-order overwrites.
      const seq = ++reloadSeqRef.current

      // Fetch metadata to get the authoritative version, then content.
      Promise.all([libraryApi.get(flowId), libraryApi.getContent(flowId)])
        .then(([meta, content]) => {
          // Stale response guard: a newer reload request superseded this one.
          if (seq !== reloadSeqRef.current) return

          // Update version tracking so the next save uses the correct version.
          useFlowStore.setState({
            libraryVersion: meta.version ?? version,
          })

          // Same-flow content refresh: update the document WITHOUT resetting
          // per-flow UI state, so a collaborator's save doesn't kick the local
          // user out of their chat thread / selection / search.
          useFlowStore.getState().applyRemoteDocumentUpdate(content as FlowDocument)
        })
        .catch(err => {
          if (seq !== reloadSeqRef.current) return
          logger.warn('Failed to reload flow after remote change', err)
        })
    })
  }, [flowId, status])
}
