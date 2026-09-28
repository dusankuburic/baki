import {useEffect} from 'react'
import {useFlowStore, beginDocLoad, isDocLoadCurrent} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useGovernanceStore} from '@/stores/governanceStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {subscribeToEvents} from '@/api/client'
import {flowApi} from '@/api'
import {libraryApi} from '@/api/library'
import type {FlowDocument as DomainFlowDocument} from '@/types'
import {getFlowDocumentEnvelopeSchema} from '@/api/schemas'
import {logger} from '@/lib/logger'

// settings:changed arrives once per save — including this tab's own saves,
// which the store debounces by 1s — and an org-settings save fans out to every
// member. Coalescing to a trailing refetch that outlasts that debounce keeps a
// slider drag or burst of toggles from reloading settings underneath the edit.
const SETTINGS_REFETCH_DELAY_MS = 1500

// The `flow:loaded` SSE event carries only a lightweight summary
// ({id, name, filePath?, isFolder?}) — never the document itself: the hub
// pins queued frames per connected client, so document-sized events meant one
// upload could pin gigabytes across a few stalled connections. This fetches
// the full document over HTTP when a tab actually needs it:
// - filePath present → local mode: re-load from the on-disk source
//   (load-folder for folder flows — they cannot load as a single file);
// - no filePath → cloud mode: the library content endpoint.
// The tab that triggered the load never comes through here (it applied the
// HTTP response of its own POST directly); this serves the OTHER tabs of the
// same user (EmitTo) or of the same local backend (broadcast).
function fetchFlowForEvent(d: Record<string, unknown>): Promise<DomainFlowDocument | null> {
  const filePath = typeof d.filePath === 'string' ? d.filePath : ''
  const id = typeof d.id === 'string' ? d.id : ''
  if (filePath) {
    return d.isFolder ? flowApi.loadFlowFolder(filePath) : flowApi.loadFlowFromPath(filePath)
  }
  return libraryApi.getContent(id)
}

export function useAppEvents(deps: {openDocument: (doc: DomainFlowDocument | null) => void}) {
  const {openDocument} = deps

  useEffect(() => {
    let unsub: (() => void) | null = null
    let cancelled = false
    let settingsRefetch: ReturnType<typeof setTimeout> | null = null
    void subscribeToEvents(ev => {
      const d = ev.data as Record<string, unknown> | undefined
      if (ev.name === 'flow:parse-progress') {
        useFlowStore.getState().setParsing(true, (d?.percent as number) ?? 0)
      } else if (ev.name === 'flow:loaded') {
        if (!d) return
        const eventFlowId = (d.id as string | undefined) ?? undefined
        if (!eventFlowId) return
        // Honor the initial load (no document open yet). A flow:loaded for a
        // flow OTHER than the currently-open one is stale or unsolicited
        // (another tab, an admin/batch action, or a late event for a flow the
        // user already navigated away from) and must NOT hijack the editor —
        // client-initiated loads call openDocument directly with the API
        // result, so they don't depend on this SSE event.
        const currentDoc = useFlowStore.getState().document
        if (currentDoc && currentDoc.id !== eventFlowId) {
          return
        }
        // A same-id event is an echo of a load whose HTTP response this tab
        // (or the refetch below) already applied — the acting tab always holds
        // the document from its own POST. Refetching on the echo would also
        // ping-pong forever between tabs: the local refetch endpoints
        // re-emit flow:loaded themselves.
        if (currentDoc) {
          return
        }
        // No document open: fetch the full document, boundary-validate it,
        // then open. Both guards are RE-CHECKED after the awaits: between
        // event arrival and this deferred open, the user may have opened a
        // different flow — without the re-check, this late open would hijack
        // the editor away from the user's choice.
        const gen = beginDocLoad()
        void Promise.all([fetchFlowForEvent(d), getFlowDocumentEnvelopeSchema()])
          .then(([doc, envelope]) => {
            if (cancelled || !doc || !isDocLoadCurrent(gen)) return
            const nowDocId = useFlowStore.getState().document?.id
            if (nowDocId && nowDocId !== eventFlowId) return
            const parsed = envelope.safeParse(doc)
            if (!parsed.success) {
              logger.warn('flow:loaded refetch failed envelope validation, dropping', parsed.error.issues[0])
              return
            }
            openDocument(parsed.data as unknown as DomainFlowDocument)
          })
          .catch(err => {
            // Transient chunk-load failure (deploy invalidated the hashed
            // chunk) or refetch failure (flow deleted; a local upload with no
            // on-disk source): drop this event; the next flow:loaded retries.
            logger.warn('flow:loaded refetch failed, dropping event', err)
          })
      } else if (ev.name === 'flow:load-error') {
        useFlowStore.getState().setParseError((d?.error as string) ?? 'Unknown error')
      } else if (ev.name === 'analysis:progress') {
        // Quantize: only write to the store when the integer percent or rule
        // name changes. StatusBar and FindingsTab subscribe to this object,
        // so unquantized writes re-render them on every one of the ~41 rule
        // events even when the visible percent didn't move.
        const current = (d?.current as number) ?? 0
        const total = (d?.total as number) ?? 0
        const ruleName = (d?.ruleName as string) ?? ''
        const prev = useAnalysisStore.getState().progress
        const prevPct = prev && prev.total > 0 ? Math.floor((prev.current / prev.total) * 100) : -1
        const pct = total > 0 ? Math.floor((current / total) * 100) : -1
        if (!prev || pct !== prevPct || prev.ruleName !== ruleName) {
          useAnalysisStore.getState().setProgress({current, total, ruleName})
        }
      } else if (ev.name === 'governance:alert') {
        // Real-time alert push from the scanner. The payload is a hint; the
        // authoritative, RLS-scoped unread count is re-fetched via REST so no
        // cross-tenant data leaks. If the bell panel is open, reload the list.
        const gs = useGovernanceStore.getState()
        void gs.refreshUnread()
        if (gs.panelOpen) void gs.reloadList()
      } else if (ev.name === 'settings:changed') {
        // Settings saved elsewhere (another tab or device, or an org admin).
        // The payload is not applied directly: re-reading through the store's
        // loader merges defaults exactly as the start-up load does.
        if (settingsRefetch) clearTimeout(settingsRefetch)
        settingsRefetch = setTimeout(() => {
          settingsRefetch = null
          if (!cancelled) void useSettingsStore.getState().loadFromBackend()
        }, SETTINGS_REFETCH_DELAY_MS)
      }
    }).then(fn => {
      if (!cancelled) unsub = fn
      else fn()
    })
    return () => {
      cancelled = true
      if (settingsRefetch) clearTimeout(settingsRefetch)
      unsub?.()
    }
  }, [openDocument])
}
