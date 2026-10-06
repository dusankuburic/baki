import {useCallback} from 'react'
import type {MutableRefObject} from 'react'
import i18n from '@/i18n'
import {chatApi, flowApi, analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {ChatEvent, FixProposalPayload} from '@/lib/chatEvent'
import type {ToolCallRecord} from '@/types'
import type {FixProposalCard as FixProposalCardState} from '@/stores/chatStore'
import {useChatStore} from '@/stores/chatStore'
import type {ChatStoreState, StreamAcc} from './types'
import {pendingCard, asCardStatus, asItemStatus} from './helpers'

// The store actions and the accumulator map the fix/tool paths need. Stable
// references, so the callbacks below keep their original identities.
interface FixDeps {
  streamAccRef: MutableRefObject<Map<string, StreamAcc>>
  setFixProposal: ChatStoreState['setFixProposal']
  patchFixProposal: ChatStoreState['patchFixProposal']
  replaceStreamTools: ChatStoreState['replaceStreamTools']
  replaceStreamFixes: ChatStoreState['replaceStreamFixes']
}

// useFixEvents drives the agentic side of a stream: the flow refresh after a
// fix lands, the approval cards (live + optimistic respond), and the resume
// journal replay that rebuilds the tool trail and the cards.
export function useFixEvents({
  streamAccRef,
  setFixProposal,
  patchFixProposal,
  replaceStreamTools,
  replaceStreamFixes,
}: FixDeps) {
  // refreshAfterFix reloads the flow a chat-driven fix just changed: findings
  // re-analyze by flow id, and when the fixed flow is the currently-open
  // local document, the canvas refreshes via loadFlowFromPath (cloud canvases
  // refresh through the websocket flow-change sync). Both are best-effort —
  // the model's outcome text and the card state stand on their own.
  const refreshAfterFix = useCallback(
    (threadId: string) => {
      const thread = useChatStore.getState().threads.find(t => t.id === threadId)
      if (!thread) return
      const curDoc = useFlowStore.getState().document
      analysisApi
        .analyzeFlowById(thread.flowId)
        .then(report => {
          useAnalysisStore.getState().setReport(thread.flowId, report)
        })
        .catch(err => logger.warn('Post-fix re-analysis failed', err))
      if (curDoc && curDoc.id === thread.flowId && curDoc.filePath) {
        flowApi
          .loadFlowFromPath(curDoc.filePath)
          .then(updated => {
            if (updated) useFlowStore.getState().applyRemoteDocumentUpdate(updated)
          })
          .catch(err => logger.warn('Post-fix document reload failed', err))
      }
    },
    [],
  )

  // onFixProposal shows an approval card (single or batch) on the stream's
  // thread.
  const onFixProposal = useCallback(
    (proposal: FixProposalPayload, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      setFixProposal(acc.threadId, pendingCard(proposal))
    },
    [streamAccRef, setFixProposal],
  )

  // onFixDecision reflects the proposal's resolution (applying/applied/
  // declined/timeout/error) onto the card — and on a successful apply,
  // refreshes the affected flow's findings + canvas so the user sees the fix
  // (the mutation happened server-side, invisible to the open document).
  const onFixDecision = useCallback(
    (
      proposalId: string,
      status: string,
      message: string | undefined,
      streamId: string,
      items?: {ruleId: string; status: string; message?: string}[],
    ) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      const cardStatus = asCardStatus(status)
      // Per-item outcome patches (batch decisions carry items[]). A
      // single-fix decision carries none — its one item mirrors the card, so
      // the card status propagates to the item.
      const itemFromWire = (rid: string, status: string, message?: string) => ({
        ruleId: rid,
        patch: {status: asItemStatus(status), message},
      })
      let itemPatches = (items ?? []).map(it => itemFromWire(it.ruleId, it.status, it.message))
      if (itemPatches.length === 0) {
        const slot = useChatStore.getState().streams[acc.threadId]
        const card = slot?.fixProposals.find(c => c.proposalId === proposalId)
        const soleItem = card?.items.length === 1 ? card.items[0] : undefined
        if (card && soleItem && soleItem.status === 'pending') {
          itemPatches = [
            itemFromWire(
              soleItem.ruleId,
              cardStatus === 'declined' || cardStatus === 'timeout' ? 'error' : cardStatus,
              message,
            ),
          ]
        }
      }
      patchFixProposal(
        acc.threadId,
        proposalId,
        {status: cardStatus, message},
        itemPatches.length > 0 ? itemPatches : undefined,
      )
      if (status === 'applied' || status === 'applied-unresolved') {
        refreshAfterFix(acc.threadId)
      }
    },
    [streamAccRef, patchFixProposal, refreshAfterFix],
  )

  // respondFixProposal delivers the user's Approve/Dismiss for a thread's
  // pending proposal. Optimistically marks applying/declined so the buttons
  // lock immediately; the backend's fix_decision event is authoritative.
  const respondFixProposal = useCallback(
    (threadId: string, approved: boolean, proposalId?: string, excludedItemIndices?: number[]) => {
      // View-only flows never dispatch an APPLY decision: approval
      // writes source; declining is always allowed.
      if (approved && useFlowStore.getState().readOnly) return
      const slot = useChatStore.getState().streams[threadId]
      if (!slot) return
      // Default to the newest pending card (the card the user just clicked);
      // an explicit proposalId targets a specific stacked card.
      const card = proposalId
        ? slot.fixProposals.find(c => c.proposalId === proposalId)
        : [...slot.fixProposals].reverse().find(c => c.status === 'pending')
      if (!card || card.status !== 'pending') return
      patchFixProposal(threadId, card.proposalId, {status: approved ? 'applying' : 'declined'})
      chatApi.respondFixDecision(slot.streamId, card.proposalId, approved, excludedItemIndices).catch(err => {
        logger.warn('Fix decision failed', err)
        patchFixProposal(threadId, card.proposalId, {status: 'error', message: i18n.t('chat:fixCard.decisionFailed')})
      })
    },
    [patchFixProposal],
  )

  // onResumeState rebuilds the slot's agentic state from a resume journal —
  // the authoritative replay after a reconnect. Wholesale replace (never
  // append) so a resume that races live SSE delivery stays idempotent.
  const onResumeState = useCallback(
    (events: ChatEvent[], streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      const calls: ToolCallRecord[] = []
      const cards: FixProposalCardState[] = []
      const byId = new Map<string, FixProposalCardState>()
      for (const ev of events) {
        if (ev.kind === 'tool-result') {
          calls.push({name: ev.name, label: ev.label, ok: ev.ok, durationMs: ev.durationMs, summary: ev.summary})
        } else if (ev.kind === 'fix-proposal') {
          const card = pendingCard(ev)
          byId.set(card.proposalId, card)
          cards.push(card)
        } else if (ev.kind === 'fix-decision') {
          const card = byId.get(ev.proposalId)
          if (!card) continue
          card.status = asCardStatus(ev.status)
          card.message = ev.message
          for (const it of ev.items ?? []) {
            const target = card.items.find(x => x.ruleId === it.ruleId)
            if (target) {
              target.status = asItemStatus(it.status)
              target.message = it.message
            }
          }
        }
      }
      replaceStreamTools(acc.threadId, calls)
      replaceStreamFixes(acc.threadId, cards)
    },
    [streamAccRef, replaceStreamTools, replaceStreamFixes],
  )

  return {refreshAfterFix, onFixProposal, onFixDecision, respondFixProposal, onResumeState}
}
