import {useEffect} from 'react'
import {useFlowStore, beginDocLoad, isDocLoadCurrent} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useGovernanceStore} from '@/stores/governanceStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {subscribeToEvents, subscribeEventGap} from '@/api/client'
import {flowApi} from '@/api'
import {libraryApi} from '@/api/library'
import type {FlowDocument as DomainFlowDocument} from '@/types'
import {getFlowDocumentEnvelopeSchema} from '@/api/schemas'
import {logger} from '@/lib/logger'

const SETTINGS_REFETCH_DELAY_MS = 1500

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
    const refetchSettings = () => {
      if (settingsRefetch) clearTimeout(settingsRefetch)
      settingsRefetch = setTimeout(() => {
        settingsRefetch = null
        if (!cancelled) void useSettingsStore.getState().loadFromBackend()
      }, SETTINGS_REFETCH_DELAY_MS)
    }
    // The server could not replay what a reconnect missed: the durable events that matter here (governance alerts,
    // settings changes) may be among them, so refetch what they would have updated.
    const offGap = subscribeEventGap(() => {
      const gs = useGovernanceStore.getState()
      void gs.refreshUnread()
      if (gs.panelOpen) void gs.reloadList()
      refetchSettings()
    })
    void subscribeToEvents(ev => {
      const d = ev.data as Record<string, unknown> | undefined
      if (ev.name === 'flow:parse-progress') {
        useFlowStore.getState().setParsing(true, (d?.percent as number) ?? 0)
      } else if (ev.name === 'flow:loaded') {
        if (!d) return
        const eventFlowId = (d.id as string | undefined) ?? undefined
        if (!eventFlowId) return
        const currentDoc = useFlowStore.getState().document
        if (currentDoc && currentDoc.id !== eventFlowId) {
          return
        }
        if (currentDoc) {
          return
        }
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
            logger.warn('flow:loaded refetch failed, dropping event', err)
          })
      } else if (ev.name === 'flow:load-error') {
        useFlowStore.getState().setParseError((d?.error as string) ?? 'Unknown error')
      } else if (ev.name === 'analysis:progress') {
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
        const gs = useGovernanceStore.getState()
        void gs.refreshUnread()
        if (gs.panelOpen) void gs.reloadList()
      } else if (ev.name === 'settings:changed') {
        refetchSettings()
      }
    }).then(fn => {
      if (!cancelled) unsub = fn
      else fn()
    })
    return () => {
      cancelled = true
      offGap()
      if (settingsRefetch) clearTimeout(settingsRefetch)
      unsub?.()
    }
  }, [openDocument])
}
