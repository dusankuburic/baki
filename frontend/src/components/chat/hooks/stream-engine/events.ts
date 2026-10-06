import {useCallback, useRef} from 'react'
import type {MutableRefObject} from 'react'
import i18n from '@/i18n'
import {uuid} from '@/lib/uuid'
import {useChatStore} from '@/stores/chatStore'
import {classifyChatFailure, budgetDetail, providerErrorInfo} from '@/lib/chatErrors'
import type {DoneMeta, ErrorUsage} from '@/lib/chatEvent'
import type {ChatMessage} from '@/types'
import type {ChatStoreState, StreamAcc, ToastApi} from './types'
import {useCommit} from './commit'
import {providerErrorLine, messageModel, applyDoneMeta, ALIAS_PROVIDERS} from './helpers'

// The pieces of the engine the done/error paths need. Stable references, so
// the callbacks below keep their original identities.
interface EventDeps {
  toastRef: MutableRefObject<ToastApi | null>
  takeAcc: (streamId: string) => StreamAcc | null
  commitAssistantMessage: ReturnType<typeof useCommit>['commitAssistantMessage']
  bumpThreadTokens: ReturnType<typeof useCommit>['bumpThreadTokens']
  endStream: ChatStoreState['endStream']
  bumpProviderEpoch: ChatStoreState['bumpProviderEpoch']
  appendMessage: ChatStoreState['appendMessage']
}

// useStreamEvents handles the terminal SSE events of a stream: the done
// commit (plus the alias-provider listing refetch) and the error bubble
// (budget, provider or generic).
export function useStreamEvents({
  toastRef,
  takeAcc,
  commitAssistantMessage,
  bumpThreadTokens,
  endStream,
  bumpProviderEpoch,
  appendMessage,
}: EventDeps) {
  // Thread/deployment/served-model triples (joined with a NUL) whose
  // served model already triggered a provider-list refetch.
  const servedModelSeenRef = useRef(new Set<string>())

  // notifyBudget shows the dedicated daily-budget notice: the user needs to
  // know it resets at midnight, not that something broke.
  const notifyBudget = useCallback(
    (detail: string) => {
      toastRef.current?.warning(i18n.t('chat:errors.budgetTitle', {detail}), {
        description: i18n.t('chat:errors.budgetBody'),
      })
    },
    [toastRef],
  )

  const onDone = useCallback(
    (tokensOut: number, tokensIn: number, streamId: string, meta?: DoneMeta) => {
      const acc = takeAcc(streamId)
      if (!acc) return
      const slot = useChatStore.getState().streams[acc.threadId]
      commitAssistantMessage(acc, slot?.messageId, acc.text, {
        tokensIn,
        tokensOut,
        finishReason: 'stop',
        meta,
        save: !acc.serverOwned,
      })
      endStream(acc.threadId)
      // An alias provider just revealed which model answers this thread:
      // refetch the listing so the header can name it ("dep · gpt-4o"). The
      // epoch bump makes useProviderSetup refetch past the GET micro-cache,
      // which would otherwise replay the listing from before this turn.
      const servedKey = `${acc.threadId}\u0000${acc.model ?? ''}\u0000${meta?.servedModel ?? ''}`
      if (
        meta?.servedModel &&
        acc.provider &&
        ALIAS_PROVIDERS.has(acc.provider) &&
        !servedModelSeenRef.current.has(servedKey)
      ) {
        servedModelSeenRef.current.add(servedKey)
        // The set lives for the whole session and threads are unbounded;
        // past the cap it clears — the worst case is one extra listing
        // refetch, exactly what the set exists to avoid per triple.
        if (servedModelSeenRef.current.size > 500) servedModelSeenRef.current.clear()
        bumpProviderEpoch()
      }
    },
    [servedModelSeenRef, bumpProviderEpoch, commitAssistantMessage, endStream, takeAcc],
  )

  const onError = useCallback(
    (error: string, streamId: string, code?: string, usage?: ErrorUsage) => {
      const acc = takeAcc(streamId)
      if (!acc) return
      const slot = useChatStore.getState().streams[acc.threadId]
      let errorLine: string
      const providerError = providerErrorInfo(code)
      if (classifyChatFailure(error, code) === 'budget') {
        const detail = budgetDetail(error)
        notifyBudget(detail)
        errorLine = i18n.t('chat:errors.budgetBubble', {detail})
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
      // A billed failure reports its usage: same fields as a done, on
      // the bubble and in the thread totals. The error bubble itself is never
      // persisted — the thread totals are, which is what the meter reads.
      if (usage?.tokensIn != null) msg.tokensIn = usage.tokensIn
      if (usage?.tokensOut != null) msg.tokensOut = usage.tokensOut
      applyDoneMeta(msg, usage?.meta)
      // A loop that fails at iteration 5 still ran 4 tools — keep the
      // trail on the error bubble so the user sees what happened before the
      // failure (error messages are excluded from persistence by design).
      if (slot && slot.toolCalls.length > 0) {
        msg.toolCalls = slot.toolCalls.map(tc => ({...tc}))
      }
      // Append unconditionally: gating the error bubble on a doc
      // being CURRENTLY loaded swallowed stream failures after the user
      // switched flows — their question appeared unanswered, silently.
      appendMessage(acc.threadId, msg)
      bumpThreadTokens(acc.threadId, usage?.tokensIn, usage?.tokensOut)
      endStream(acc.threadId)
    },
    [appendMessage, bumpProviderEpoch, bumpThreadTokens, endStream, notifyBudget, takeAcc],
  )

  return {notifyBudget, onDone, onError}
}
