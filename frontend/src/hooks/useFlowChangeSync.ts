import {useEffect, useRef} from 'react'
import {onFlowChanged, usePresenceStore} from '@/stores/presenceStore'
import {useFlowStore} from '@/stores/flowStore'
import {libraryApi} from '@/api/library'
import {logger} from '@/lib/logger'
import type {FlowDocument} from '@/types'

// A collaborator's edit arrives as a burst of flow.changed events (one per
// version; a multi-step edit lands several within a few hundred ms). Each
// used to start its own metadata + content reload — the content being the
// whole document, tens of MB for big flows — and only the sequence guard
// kept the last one, after every reload had been parsed. The burst is now
// coalesced with a trailing debounce, and a reload still in flight when the
// next one starts is aborted rather than parsed and discarded.
export const FLOW_CHANGE_DEBOUNCE_MS = 300

export function useFlowChangeSync(): void {
  const flowId = usePresenceStore(s => s.flowId)
  const status = usePresenceStore(s => s.status)
  // The version last APPLIED to the document, per flow. Only an applied
  // version may drop a repeated flow.changed: the server re-emits the current
  // version after a reconnect, and if the reload for it was aborted by the
  // reconnect (cleanup below) that re-emit is the only way the document
  // catches up before the next remote edit.
  const appliedRef = useRef<{flowId: string | null; version: number}>({flowId: null, version: 0})

  useEffect(() => {
    if (!flowId || status !== 'connected') return
    if (appliedRef.current.flowId !== flowId) appliedRef.current = {flowId, version: 0}
    let timer: ReturnType<typeof setTimeout> | null = null
    let inFlight: AbortController | null = null
    // Version the scheduled/in-flight reload targets, for THIS subscription.
    let pendingVersion = 0
    // Reload sequence for THIS subscription: a response is applied only if
    // it belongs to the newest reload, and the cleanup bumps it so nothing
    // lands after the flow or the connection changed.
    let reloadSeq = 0

    const reload = () => {
      timer = null
      const currentDoc = useFlowStore.getState().document
      if (!currentDoc || currentDoc.id !== flowId) return

      // Increment the sequence counter so that if two reload requests
      // fire in succession (v4 then v5), only the latest one's response is
      // applied — preventing out-of-order overwrites. The superseded fetch
      // is also aborted so its body is never downloaded and parsed.
      inFlight?.abort()
      const controller = new AbortController()
      inFlight = controller
      const seq = ++reloadSeq
      const target = pendingVersion

      // Fetch metadata to get the authoritative version, then content.
      Promise.all([libraryApi.get(flowId, controller.signal), libraryApi.getContent(flowId, controller.signal)])
        .then(([meta, content]) => {
          // Stale response guard: a newer reload request superseded this one.
          if (seq !== reloadSeq) return

          // Update version tracking so the next save uses the correct version.
          const version = meta.version ?? target
          appliedRef.current = {flowId, version}
          useFlowStore.setState({libraryVersion: version})

          // Same-flow content refresh: update the document WITHOUT resetting
          // per-flow UI state, so a collaborator's save doesn't kick the local
          // user out of their chat thread / selection / search.
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
      // Already applied, or already the target of the pending reload.
      if (version === appliedRef.current.version || version === pendingVersion) return
      pendingVersion = version
      if (timer) clearTimeout(timer)
      timer = setTimeout(reload, FLOW_CHANGE_DEBOUNCE_MS)
    })

    return () => {
      unsubscribe()
      if (timer) clearTimeout(timer)
      // A response landing after the flow changed (or the socket dropped)
      // must not be applied: bump the sequence and abort the download. The
      // applied version is left as is, so the re-emit after a reconnect
      // reloads what this abort dropped.
      reloadSeq++
      inFlight?.abort()
    }
  }, [flowId, status])
}
