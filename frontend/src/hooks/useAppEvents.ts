import {useEffect} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useGovernanceStore} from '@/stores/governanceStore'
import {subscribeToEvents} from '@/api/client'
import type {FlowDocument as DomainFlowDocument} from '@/types'

export function useAppEvents(deps: {openDocument: (doc: DomainFlowDocument | null) => void}) {
  const {openDocument} = deps

  useEffect(() => {
    let unsub: (() => void) | null = null
    let cancelled = false
    subscribeToEvents(ev => {
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
        openDocument(d as unknown as DomainFlowDocument)
      } else if (ev.name === 'flow:load-error') {
        useFlowStore.getState().setParseError((d?.error as string) ?? 'Unknown error')
      } else if (ev.name === 'analysis:progress') {
        useAnalysisStore.getState().setProgress({
          current: (d?.current as number) ?? 0,
          total: (d?.total as number) ?? 0,
          ruleName: (d?.ruleName as string) ?? '',
        })
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
