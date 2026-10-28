import {useCallback, useEffect, useRef} from 'react'
import type {MutableRefObject} from 'react'
import i18n from '@/i18n'
import {chatApi, flowApi, analysisApi, ApiError} from '@/api'
import {isReplicaUnreachable, isServerRestarting} from '@/lib/chatErrors'
import {isRateLimited} from '@/lib/streamControl'
import {logger} from '@/lib/logger'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {ChatEvent, FixDecisionEventItem, FixProposalPayload} from '@/lib/chatEvent'
import {NOTICE_HISTORY_TRUNCATED, NOTICE_KNOWLEDGE} from '@/lib/chatEvent'
import type {KnowledgeStatus} from '@/lib/chatEvent'
import type {ToolCallRecord} from '@/types'
import type {ApprovalCard} from '@/stores/chatStore'
import {useChatStore} from '@/stores/chatStore'
import type {ChatStoreState, StreamAcc, ToastApi} from './types'
import type {ClosingStreams} from './closing'
import {reloadLibraryDocument} from '@/lib/libraryReload'
import {pendingCard, asCardStatus, asItemStatus, decisionTargets} from './helpers'

interface FixDeps {
  streamAccRef: MutableRefObject<Map<string, StreamAcc>>
  closing: ClosingStreams
  persistThread: (threadId: string, flowId: string) => void
  updateMessage: ChatStoreState['updateMessage']
  toastRef: MutableRefObject<ToastApi | null>
  setFixProposal: ChatStoreState['setFixProposal']
  patchFixProposal: ChatStoreState['patchFixProposal']
  replaceStreamTools: ChatStoreState['replaceStreamTools']
  replaceStreamFixes: ChatStoreState['replaceStreamFixes']
  setStreamMeta: ChatStoreState['setStreamMeta']
}

export function useFixEvents({
  streamAccRef,
  closing,
  persistThread,
  updateMessage,
  toastRef,
  setFixProposal,
  patchFixProposal,
  replaceStreamTools,
  replaceStreamFixes,
  setStreamMeta,
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
      } else if (curDoc && curDoc.id === thread.flowId) {
        // A library flow: reloaded here, not only through the presence socket's flow.changed, which a disconnected
        // or reconnecting socket never delivers (R-frontend-8).
        reloadLibraryDocument(thread.flowId).catch(err => logger.warn('Post-fix document reload failed', err))
      }
    },
    [],
  )

  // The decision of a stream stopped mid-apply: the message was committed at the stop with the card "applying", and
  // takes the outcome now, saved again (R-approvals-5).
  const settleCommittedCard = useCallback(
    (streamId: string, proposalId: string, status: string, message?: string, items?: FixDecisionEventItem[]) => {
      const entry = closing.closing(streamId)
      if (!entry) return
      const cardStatus = asCardStatus(status)
      if (cardStatus === 'applying') return
      updateMessage(entry.threadId, entry.messageId, m => ({
        ...m,
        fixProposals: m.fixProposals?.map(fp => {
          if (fp.proposalId !== proposalId) return fp
          const rows = fp.items ?? []
          const next = rows.map(it => ({...it}))
          const targets = decisionTargets(
            rows.map(it => ({
              id: it.id ?? '',
              label: it.blockLabel,
              ruleId: it.ruleId,
              summary: it.summary,
              status: 'pending' as const,
            })),
            items ?? [],
          )
          for (const {index, item} of targets) {
            next[index] = {...next[index]!, status: asItemStatus(item.status), message: item.message}
          }
          if (targets.length === 0 && next.length === 1) {
            const sole =
              cardStatus === 'declined' || cardStatus === 'timeout' || cardStatus === 'cancelled' ? 'error' : cardStatus
            next[0] = {...next[0]!, status: sole, message}
          }
          return {...fp, status: cardStatus, message: message ?? fp.message, items: next}
        }),
      }))
      const thread = useChatStore.getState().threads.find(t => t.id === entry.threadId)
      if (thread) persistThread(entry.threadId, thread.flowId)
      if (cardStatus === 'applied' || cardStatus === 'applied-unresolved') refreshAfterFix(entry.threadId)
    },
    [closing, persistThread, refreshAfterFix, updateMessage],
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
      if (!acc) {
        settleCommittedCard(streamId, proposalId, status, message, items)
        return
      }
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
        // The server's echo of a decision the card already shows carries no message; keep the card's reason.
        {status: cardStatus, message: message || (card?.status === cardStatus ? card.message : undefined)},
        itemPatches.length > 0 ? itemPatches : undefined,
      )
      if (card?.source === 'fix' && (status === 'applied' || status === 'applied-unresolved')) {
        refreshAfterFix(acc.threadId)
      }
    },
    [streamAccRef, patchFixProposal, refreshAfterFix, settleCommittedCard],
  )

  // The retry action of the unreachable-server toast calls the latest respond (it cannot name itself).
  const respondRef = useRef<(threadId: string, approved: boolean, proposalId?: string, excludedItemIndices?: number[]) => void>(
    () => {},
  )

  const respondFixProposal = useCallback(
    (threadId: string, approvedClick: boolean, proposalId?: string, excludedItemIndices?: number[]) => {
      const slot = useChatStore.getState().streams[threadId]
      if (!slot) return
      const card = proposalId
        ? slot.approvals.find(c => c.proposalId === proposalId)
        : [...slot.approvals].reverse().find(c => c.status === 'pending')
      if (!card || card.status !== 'pending') return
      // A read-only flow can't take the change: an Approve is sent as a decline, so the card and the model learn
      // of it now instead of waiting out the server's decision timer.
      const readOnlyDecline = approvedClick && useFlowStore.getState().readOnly
      const approved = approvedClick && !readOnlyDecline
      const sent = approved ? 'applying' : 'declined'
      patchFixProposal(threadId, card.proposalId, {
        status: sent,
        ...(readOnlyDecline ? {message: i18n.t('chat:fixCard.readOnly')} : {}),
      })
      const args = [slot.streamId, card.proposalId, approved, excludedItemIndices] as const
      const respond =
        card.source === 'approval'
          ? chatApi.respondApprovalDecision(...args).catch((err: unknown) => {
              if (err instanceof ApiError && err.status === 404) return chatApi.respondFixDecision(...args)
              throw err
            })
          : chatApi.respondFixDecision(...args)
      respond.catch(err => {
        // The server may have settled the card meanwhile (its decision timed out, the stream ended): that outcome
        // stands, and nothing is left to send again.
        const now = useChatStore.getState().streams[threadId]?.approvals.find(c => c.proposalId === card.proposalId)
        if (now?.status !== sent) {
          logger.warn('Approval decision failed after the card was settled', err)
          return
        }
        const restarting = isServerRestarting(err)
        const limited = isRateLimited(err)
        if (isReplicaUnreachable(err) || restarting || limited) {
          // The server running the stream did not answer, or is restarting and refused the approve: nothing was
          // decided, so the card goes back to pending and the decision can be sent again.
          logger.warn('Approval decision could not reach the server running the stream', err)
          patchFixProposal(threadId, card.proposalId, {status: 'pending'})
          const notice = restarting
            ? 'chat:fixCard.decisionRestarting'
            : limited
              ? 'chat:fixCard.decisionRateLimited'
              : 'chat:fixCard.decisionUnreachable'
          toastRef.current?.warning(i18n.t(notice), {
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
      let truncated = 0
      let knowledge: KnowledgeStatus | undefined
      for (const ev of events) {
        if (ev.kind === 'notice') {
          if (ev.notice === NOTICE_HISTORY_TRUNCATED && ev.keptFrom != null) truncated = Math.max(truncated, ev.keptFrom)
          if (ev.notice === NOTICE_KNOWLEDGE && ev.status) knowledge = ev.status
        } else if (ev.kind === 'tool-result') {
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
      if (truncated > 0) setStreamMeta(acc.threadId, {truncated})
      if (knowledge) setStreamMeta(acc.threadId, {knowledge})
    },
    [streamAccRef, replaceStreamTools, replaceStreamFixes, setStreamMeta],
  )

  return {refreshAfterFix, onFixProposal, onFixDecision, respondFixProposal, onResumeState}
}
