import {useCallback, useEffect, useRef} from 'react'
import type {MutableRefObject} from 'react'
import i18n from '@/i18n'
import {chatApi, flowApi, analysisApi, ApiError} from '@/api'
import {isReplicaUnreachable} from '@/lib/chatErrors'
import {logger} from '@/lib/logger'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {ChatEvent, FixDecisionEventItem, FixProposalPayload} from '@/lib/chatEvent'
import type {ToolCallRecord} from '@/types'
import type {ApprovalCard} from '@/stores/chatStore'
import {useChatStore} from '@/stores/chatStore'
import type {ChatStoreState, StreamAcc, ToastApi} from './types'
import {pendingCard, asCardStatus, asItemStatus, decisionTargets} from './helpers'

interface FixDeps {
  streamAccRef: MutableRefObject<Map<string, StreamAcc>>
  toastRef: MutableRefObject<ToastApi | null>
  setFixProposal: ChatStoreState['setFixProposal']
  patchFixProposal: ChatStoreState['patchFixProposal']
  replaceStreamTools: ChatStoreState['replaceStreamTools']
  replaceStreamFixes: ChatStoreState['replaceStreamFixes']
}

export function useFixEvents({
  streamAccRef,
  toastRef,
  setFixProposal,
  patchFixProposal,
  replaceStreamTools,
  replaceStreamFixes,
}: FixDeps) {
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

  const onFixProposal = useCallback(
    (proposal: FixProposalPayload, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      setFixProposal(acc.threadId, pendingCard(proposal))
    },
    [streamAccRef, setFixProposal],
  )

  const onFixDecision = useCallback(
    (
      proposalId: string,
      status: string,
      message: string | undefined,
      streamId: string,
      items?: FixDecisionEventItem[],
    ) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      const cardStatus = asCardStatus(status)
      const slot = useChatStore.getState().streams[acc.threadId]
      const card = slot?.approvals.find(c => c.proposalId === proposalId)
      const itemPatch = (index: number, status: string, message?: string) => ({
        index,
        patch: {status: asItemStatus(status), message},
      })
      let itemPatches = card
        ? decisionTargets(card.items, items ?? []).map(t => itemPatch(t.index, t.item.status, t.item.message))
        : []
      if (itemPatches.length === 0) {
        const soleItem = card?.items.length === 1 ? card.items[0] : undefined
        if (card && soleItem && soleItem.status === 'pending') {
          itemPatches = [
            itemPatch(
              0,
              cardStatus === 'declined' || cardStatus === 'timeout' || cardStatus === 'cancelled'
                ? 'error'
                : cardStatus,
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
      if (card?.source === 'fix' && (status === 'applied' || status === 'applied-unresolved')) {
        refreshAfterFix(acc.threadId)
      }
    },
    [streamAccRef, patchFixProposal, refreshAfterFix],
  )

  // The retry action of the unreachable-server toast calls the latest respond (it cannot name itself).
  const respondRef = useRef<(threadId: string, approved: boolean, proposalId?: string, excludedItemIndices?: number[]) => void>(
    () => {},
  )

  const respondFixProposal = useCallback(
    (threadId: string, approved: boolean, proposalId?: string, excludedItemIndices?: number[]) => {
      if (approved && useFlowStore.getState().readOnly) return
      const slot = useChatStore.getState().streams[threadId]
      if (!slot) return
      const card = proposalId
        ? slot.approvals.find(c => c.proposalId === proposalId)
        : [...slot.approvals].reverse().find(c => c.status === 'pending')
      if (!card || card.status !== 'pending') return
      patchFixProposal(threadId, card.proposalId, {status: approved ? 'applying' : 'declined'})
      const args = [slot.streamId, card.proposalId, approved, excludedItemIndices] as const
      const respond =
        card.source === 'approval'
          ? chatApi.respondApprovalDecision(...args).catch((err: unknown) => {
              if (err instanceof ApiError && err.status === 404) return chatApi.respondFixDecision(...args)
              throw err
            })
          : chatApi.respondFixDecision(...args)
      respond.catch(err => {
        if (isReplicaUnreachable(err)) {
          // The server running the stream did not answer: nothing was decided, so the card goes back to pending and the
          // decision can be sent again.
          logger.warn('Approval decision could not reach the server running the stream', err)
          patchFixProposal(threadId, card.proposalId, {status: 'pending'})
          toastRef.current?.warning(i18n.t('chat:fixCard.decisionUnreachable'), {
            action: {
              label: i18n.t('chat:fixCard.decisionRetry'),
              onClick: () => respondRef.current(threadId, approved, card.proposalId, excludedItemIndices),
            },
          })
          return
        }
        logger.warn('Approval decision failed', err)
        patchFixProposal(threadId, card.proposalId, {status: 'error', message: i18n.t('chat:fixCard.decisionFailed')})
      })
    },
    [patchFixProposal, toastRef],
  )
  useEffect(() => {
    respondRef.current = respondFixProposal
  }, [respondFixProposal])

  const onResumeState = useCallback(
    (events: ChatEvent[], streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      const calls: ToolCallRecord[] = []
      const cards: ApprovalCard[] = []
      const byId = new Map<string, ApprovalCard>()
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
          for (const {index, item} of decisionTargets(card.items, ev.items ?? [])) {
            const target = card.items[index]!
            target.status = asItemStatus(item.status)
            target.message = item.message
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
