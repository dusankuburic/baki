import {useCallback, useRef} from 'react'
import type {MutableRefObject} from 'react'
import i18n from '@/i18n'
import {logger} from '@/lib/logger'
import {saveThreadConversation} from '@/lib/conversationSync'
import {uuid} from '@/lib/uuid'
import {useChatStore} from '@/stores/chatStore'
import {conversationKey} from '@/stores/chatStore'
import {conversationHistoryEnabled} from '@/lib/chatHistory'
import type {ChatMessage} from '@/types'
import type {DoneMeta} from '@/lib/chatEvent'
import type {ChatStoreState, StreamAcc, ToastApi} from './types'
import {messageModel, applyDoneMeta} from './helpers'

interface CommitDeps {
  toastRef: MutableRefObject<ToastApi | null>
  appendMessage: ChatStoreState['appendMessage']
  updateThread: ChatStoreState['updateThread']
}

export function useCommit({toastRef, appendMessage, updateThread}: CommitDeps) {
  const saveFailedRef = useRef(new Set<string>())

  const persistThread = useCallback(
    (threadId: string, flowId: string) => {
      if (!conversationHistoryEnabled()) return
      const st = useChatStore.getState()
      const thread = st.threads.find(t => t.id === threadId)
      if (!thread || !st.isHistorySynced(threadId)) return
      saveThreadConversation(threadId, flowId, conversationKey(thread))
        .then(() => {
          saveFailedRef.current.delete(threadId)
        })
        .catch(err => {
          logger.warn('Failed to save conversation', err)
          if (saveFailedRef.current.has(threadId)) return
          saveFailedRef.current.add(threadId)
          toastRef.current?.error(i18n.t('chat:errors.saveFailed'), {
            description: i18n.t('chat:errors.saveFailedBody'),
          })
        })
    },
    [toastRef, saveFailedRef],
  )

  const bumpThreadTokens = useCallback(
    (threadId: string, tokensIn?: number, tokensOut?: number) => {
      if (tokensIn == null && tokensOut == null) return
      const thread = useChatStore.getState().threads.find(t => t.id === threadId)
      if (!thread) return
      updateThread(threadId, {
        tokensIn: (thread.tokensIn ?? 0) + (tokensIn ?? 0),
        tokensOut: (thread.tokensOut ?? 0) + (tokensOut ?? 0),
      })
    },
    [updateThread],
  )

  const commitAssistantMessage = useCallback(
    (
      acc: Pick<StreamAcc, 'threadId' | 'provider' | 'model' | 'modelLabel'>,
      messageId: string | undefined,
      content: string,
      opts: {
        tokensIn?: number
        tokensOut?: number
        finishReason: ChatMessage['finishReason']
        meta?: DoneMeta
        save?: boolean
      },
    ) => {
      const threadId = acc.threadId
      const thread = useChatStore.getState().threads.find(t => t.id === threadId)
      if (!thread) return
      const slot = useChatStore.getState().streams[threadId]
      const msg: ChatMessage = {
        id: messageId || uuid(),
        role: 'assistant',
        content,
        timestamp: new Date().toISOString(),
        ...messageModel(acc),
        tokensIn: opts.tokensIn,
        tokensOut: opts.tokensOut,
        finishReason: opts.finishReason,
      }
      applyDoneMeta(msg, opts.meta)
      if (slot && slot.toolCalls.length > 0) {
        msg.toolCalls = slot.toolCalls.map(tc => ({...tc}))
      }
      // The omitted-history count stays on the message, so the chip survives a reload.
      if (slot?.truncated) msg.truncated = slot.truncated
      const fixCards = slot ? slot.approvals.filter(card => card.source === 'fix') : []
      if (fixCards.length > 0) {
        msg.fixProposals = fixCards.map(card => ({
          proposalId: card.proposalId,
          ...(card.tool ? {tool: card.tool} : {}),
          ...(card.previewKind ? {previewKind: card.previewKind} : {}),
          ruleId: card.ruleId ?? '',
          fixType: card.fixType ?? '',
          blockLabel: card.blockLabel ?? '',
          line: card.line ?? 0,
          summary: card.summary ?? '',
          status: card.status,
          message: card.message,
          items: card.items.map(it => ({
            ruleId: it.ruleId ?? '',
            fixType: it.fixType ?? '',
            blockLabel: it.blockLabel ?? '',
            line: it.line ?? 0,
            summary: it.summary,
            status: it.status,
            message: it.message,
          })),
        }))
      }
      appendMessage(threadId, msg)
      if ((msg.toolCalls?.length ?? 0) > 0 || (slot?.approvals.length ?? 0) > 0) {
        updateThread(threadId, {
          usedTools: true,
          appliedFixes:
            thread.appliedFixes ||
            (msg.fixProposals ?? []).some(p => p.status === 'applied' || p.status === 'applied-unresolved'),
        })
      }
      if (opts.save !== false) persistThread(threadId, thread.flowId)
      bumpThreadTokens(threadId, opts.tokensIn, opts.tokensOut)
    },
    [appendMessage, bumpThreadTokens, persistThread, updateThread],
  )

  return {persistThread, bumpThreadTokens, commitAssistantMessage}
}
