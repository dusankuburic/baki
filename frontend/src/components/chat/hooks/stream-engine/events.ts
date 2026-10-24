import {useCallback, useRef} from 'react'
import type {MutableRefObject} from 'react'
import i18n from '@/i18n'
import {uuid} from '@/lib/uuid'
import {useChatStore} from '@/stores/chatStore'
import {classifyChatFailure, providerErrorInfo} from '@/lib/chatErrors'
import {adoptStreamVersion} from '@/lib/conversationSync'
import type {DoneMeta, ErrorUsage} from '@/lib/chatEvent'
import type {ChatMessage} from '@/types'
import type {ChatStoreState, StreamAcc} from './types'
import {useCommit} from './commit'
import {providerErrorLine, messageModel, applyDoneMeta, ALIAS_PROVIDERS} from './helpers'

interface EventDeps {
  takeAcc: (streamId: string) => StreamAcc | null
  commitAssistantMessage: ReturnType<typeof useCommit>['commitAssistantMessage']
  bumpThreadTokens: ReturnType<typeof useCommit>['bumpThreadTokens']
  endStream: ChatStoreState['endStream']
  bumpProviderEpoch: ChatStoreState['bumpProviderEpoch']
  appendMessage: ChatStoreState['appendMessage']
  autoResendRef: MutableRefObject<(streamId: string) => string | null>
}

export function useStreamEvents({
  takeAcc,
  commitAssistantMessage,
  bumpThreadTokens,
  endStream,
  bumpProviderEpoch,
  appendMessage,
  autoResendRef,
}: EventDeps) {
  const servedModelSeenRef = useRef(new Set<string>())

  const onDone = useCallback(
    (tokensOut: number, tokensIn: number, streamId: string, meta?: DoneMeta) => {
      const acc = takeAcc(streamId)
      if (!acc) return
      const slot = useChatStore.getState().streams[acc.threadId]
      // The server stored this turn (question and answer) and named the version it is at: nothing left to save.
      const adopted = adoptStreamVersion(acc.threadId, meta)
      commitAssistantMessage(acc, slot?.messageId, acc.text, {
        tokensIn,
        tokensOut,
        finishReason: 'stop',
        meta,
        save: !acc.serverOwned && !adopted,
      })
      endStream(acc.threadId)
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
    [servedModelSeenRef, bumpProviderEpoch, commitAssistantMessage, endStream, takeAcc],
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
      if (!acc) return
      adoptStreamVersion(acc.threadId, usage?.meta)
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
        ...(providerError && code ? {errorCode: code} : {}),
        ...(usage?.shared ? {errorShared: true} : {}),
      }
      if (usage?.tokensIn != null) msg.tokensIn = usage.tokensIn
      if (usage?.tokensOut != null) msg.tokensOut = usage.tokensOut
      applyDoneMeta(msg, usage?.meta)
      if (slot && slot.toolCalls.length > 0) {
        msg.toolCalls = slot.toolCalls.map(tc => ({...tc}))
      }
      appendMessage(acc.threadId, msg)
      bumpThreadTokens(acc.threadId, usage?.tokensIn, usage?.tokensOut)
      endStream(acc.threadId)
    },
    [appendMessage, autoResendRef, bumpProviderEpoch, bumpThreadTokens, endStream, takeAcc],
  )

  return {onDone, onError}
}
