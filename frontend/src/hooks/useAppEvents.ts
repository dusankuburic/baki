import {useEffect} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useGovernanceStore} from '@/stores/governanceStore'
import {subscribeToEvents} from '@/api/client'
import type {FlowDocument as DomainFlowDocument} from '@/types'
import {getFlowDocumentEnvelopeSchema} from '@/api/schemas'
import {logger} from '@/lib/logger'

export function useAppEvents(deps: {openDocument: (doc: DomainFlowDocument | null) => void}) {
  const {openDocument} = deps

  useEffect(() => {
    let unsub: (() => void) | null = null
    let cancelled = false
    void subscribeToEvents(ev => {
      const d = ev.data as Record<string, unknown> | undefined
      if (ev.name === 'flow:parse-progress') {
        useFlowStore.setState({parseProgress: (d?.percent as number) ?? 0, isParsing: true})
      } else if (ev.name === 'flow:loaded') {
        if (!d) return
        // Honor the initial load (no document open yet) or a refresh of the
        // currently-open flow. A flow:loaded for a DIFFERENT flow is stale or
        // unsolicited (another tab, an admin/batch action, or a late event for
        // a flow the user already navigated away from) and must NOT hijack the
        // editor — client-initiated loads call openDocument directly with the
        // API result, so they don't depend on this SSE event.
        const eventFlowId = (d.id as string | undefined) ?? undefined
        const currentDocId = useFlowStore.getState().document?.id
        if (eventFlowId && currentDocId && eventFlowId !== currentDocId) {
          return
        }
        // Boundary-validate the SSE payload before it enters the editor
        // store: a malformed event (proxy error page, truncated JSON, another
        // event's data shape) must not hijack the editor. Top-level envelope
        // check only — see FlowDocumentEnvelopeSchema's doc comment.
        //
        // The schema factory dynamic-imports zod once (memoized); successive
        // flow:loaded events await the same promise, so their continuations
        // run in arrival order. The doc-id guard is RE-CHECKED inside the
        // continuation: between event arrival and this deferred open, the
        // user may have synchronously opened a different flow (the window is
        // widest on the first event, while the zod chunk is still loading) —
        // without the re-check, this late open would hijack the editor away
        // from the user's choice.
        void getFlowDocumentEnvelopeSchema()
          .then(envelope => {
            const nowDocId = useFlowStore.getState().document?.id
            if (eventFlowId && nowDocId && eventFlowId !== nowDocId) return
            const parsed = envelope.safeParse(d)
            if (!parsed.success) {
              logger.warn('flow:loaded event failed envelope validation, dropping', parsed.error.issues[0])
              return
            }
            openDocument(parsed.data as unknown as DomainFlowDocument)
          })
          .catch(err => {
            // Transient chunk-load failure (deploy invalidated the hashed
            // chunk): drop this event; the next flow:loaded retries the load.
            logger.warn('flow:loaded schema load failed, dropping event', err)
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
      }
    }).then(fn => {
      if (!cancelled) unsub = fn
      else fn()
    })
    return () => {
      cancelled = true
      unsub?.()
    }
  }, [openDocument])
}
