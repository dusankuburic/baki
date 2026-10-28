import {useCallback, useRef} from 'react'
import type {MutableRefObject} from 'react'
import i18n from '@/i18n'
import {uuid} from '@/lib/uuid'
import {useChatStore, conversationKey} from '@/stores/chatStore'
import {classifyChatFailure, providerErrorInfo} from '@/lib/chatErrors'
import {adoptStreamVersion, syncThreadVersion} from '@/lib/conversationSync'
import {logger} from '@/lib/logger'
import type {DoneMeta, ErrorUsage} from '@/lib/chatEvent'
import type {ChatMessage} from '@/types'
import type {ChatStoreState, StreamAcc} from './types'
import {useCommit, fixSnapshotsOf} from './commit'
import type {ClosingStreams} from './closing'
import {providerErrorLine, messageModel, applyDoneMeta, ALIAS_PROVIDERS} from './helpers'

interface EventDeps {
  takeAcc: (streamId: string) => StreamAcc | null
  commitAssistantMessage: ReturnType<typeof useCommit>['commitAssistantMessage']
  bumpThreadTokens: ReturnType<typeof useCommit>['bumpThreadTokens']
  persistThread: ReturnType<typeof useCommit>['persistThread']
  updateThread: ChatStoreState['updateThread']
  closing: ClosingStreams
  endStream: ChatStoreState['endStream']
  bumpProviderEpoch: ChatStoreState['bumpProviderEpoch']
  appendMessage: ChatStoreState['appendMessage']
  autoResendRef: MutableRefObject<(streamId: string) => string | null>
}

export function useStreamEvents({
  takeAcc,
  commitAssistantMessage,
  bumpThreadTokens,
  persistThread,
  updateThread,
  closing,
  endStream,
  bumpProviderEpoch,
  appendMessage,
  autoResendRef,
}: EventDeps) {
  const servedModelSeenRef = useRef(new Set<string>())

  const onDone = useCallback(
    (tokensOut: number, tokensIn: number, streamId: string, meta?: DoneMeta) => {
      const acc = takeAcc(streamId)
      if (!acc) {
        closing.settle(streamId)
        return
      }
      const slot = useChatStore.getState().streams[acc.threadId]
      // The server stored this turn (question and answer) and named the version it is at: nothing left to save.
      const adopted = adoptStreamVersion(acc.threadId, meta)
      const clientSaves = (!acc.serverOwned && !adopted) || meta?.answerStored === false
      commitAssistantMessage(acc, slot?.messageId, acc.text, {
        tokensIn,
        tokensOut,
        finishReason: 'stop',
        meta,
        // A server-owned turn whose answer the server could not store is saved here; a failed save toasts.
        save: clientSaves,
      })
      endStream(acc.threadId)
      // A server-owned turn the client could not adopt, and does not save: one read refreshes its version (R-docs-8).
      // A save reconciles the version itself, so it never runs beside one.
      if (acc.serverOwned && !adopted && !clientSaves) {
        const thread = useChatStore.getState().threads.find(t => t.id === acc.threadId)
        if (thread) {
          syncThreadVersion(acc.threadId, thread.flowId, conversationKey(thread)).catch(err =>
            logger.warn('Could not refresh the conversation version after a stream', err),
          )
        }
      }
      const servedKey = `${acc.threadId}\u0000${acc.model ?? ''}\u0000${meta?.servedModel ?? ''}`
      if (
        meta?.servedModel &&
        acc.provider &&
        ALIAS_PROVIDERS.has(acc.provider) &&
        !servedModelSeenRef.current.has(servedKey)
      ) {
        servedModelSeenRef.current.add(servedKey)
        if (servedModelSeenRef.current.size > 500) servedModelSeenRef.current.clear()
        bumpProviderEpoch()
      }
    },
    [servedModelSeenRef, bumpProviderEpoch, closing, commitAssistantMessage, endStream, takeAcc],
  )

  const onError = useCallback(
    (error: string, streamId: string, code?: string, usage?: ErrorUsage) => {
      const kind = classifyChatFailure(error, code)
      if (kind === 'serverRestarting') {
        const resendThread = autoResendRef.current(streamId)
        if (resendThread) {
          // The question the server stored before it went down is in this version; the re-sent turn saves on it.
          adoptStreamVersion(resendThread, usage?.meta)
          bumpThreadTokens(resendThread, usage?.tokensIn, usage?.tokensOut)
          return
        }
      }
      const acc = takeAcc(streamId)
      if (!acc) {
        // A stream stopped mid-apply ends here once its last decision has settled the committed card.
        closing.settle(streamId)
        return
      }
      const adopted = adoptStreamVersion(acc.threadId, usage?.meta)
      const slot = useChatStore.getState().streams[acc.threadId]
      let errorLine: string
      const providerError = providerErrorInfo(code)
      if (kind === 'serverRestarting') {
        errorLine = i18n.t('chat:errors.generic', {message: i18n.t('chat:errors.serverRestarted')})
      } else if (providerError) {
        errorLine = providerErrorLine(providerError, error)
        if (providerError.refreshProviders) bumpProviderEpoch()
      } else {
        errorLine = i18n.t('chat:errors.generic', {message: error})
      }
      const displayContent = acc.text ? acc.text + '\n\n---\n' + errorLine : errorLine
      const msg: ChatMessage = {
        id: slot?.messageId || uuid(),
        role: 'assistant',
        content: displayContent,
        timestamp: new Date().toISOString(),
        ...messageModel(acc),
        finishReason: 'error',
        // Every coded failure keeps its code: the bubble branches on it (Compact for CONTEXT_LIMIT, R-provider-16).
        ...(code ? {errorCode: code} : {}),
        ...(usage?.shared ? {errorShared: true} : {}),
      }
      if (usage?.tokensIn != null) msg.tokensIn = usage.tokensIn
      if (usage?.tokensOut != null) msg.tokensOut = usage.tokensOut
      applyDoneMeta(msg, usage?.meta)
      if (slot && slot.toolCalls.length > 0) {
        msg.toolCalls = slot.toolCalls.map(tc => ({...tc}))
      }
      // The fix cards stay with the message: a fix applied before the failure changed the flow (R-approvals-4).
      const fixes = fixSnapshotsOf(slot)
      if (fixes.length > 0) {
        msg.fixProposals = fixes
        if (fixes.some(p => p.status === 'applied' || p.status === 'applied-unresolved')) {
          updateThread(acc.threadId, {usedTools: true, appliedFixes: true})
        }
      }
      appendMessage(acc.threadId, msg)
      // A turn that left something behind is saved, unless the server stored it (it owns the turn's history).
      if (!acc.serverOwned && !adopted && (fixes.length > 0 || msg.toolCalls || acc.text)) {
        const thread = useChatStore.getState().threads.find(t => t.id === acc.threadId)
        if (thread) persistThread(acc.threadId, thread.flowId)
      }
      bumpThreadTokens(acc.threadId, usage?.tokensIn, usage?.tokensOut)
      endStream(acc.threadId)
    },
    [
      appendMessage,
      autoResendRef,
      bumpProviderEpoch,
      bumpThreadTokens,
      closing,
      endStream,
      persistThread,
      takeAcc,
      updateThread,
    ],
  )

  return {onDone, onError}
}
