import {useCallback, useRef} from 'react'
import type {MutableRefObject} from 'react'
import i18n from '@/i18n'
import {chatApi} from '@/api'
import {logger} from '@/lib/logger'
import {uuid} from '@/lib/uuid'
import {useChatStore} from '@/stores/chatStore'
import {conversationKey} from '@/stores/chatStore'
import {conversationHistoryEnabled} from '@/lib/chatHistory'
import type {ChatMessage} from '@/types'
import type {DoneMeta} from '@/lib/chatEvent'
import type {ChatStoreState, StreamAcc, ToastApi} from './types'
import {messageModel, applyDoneMeta} from './helpers'

// The store actions and the toast ref the commit path needs. Stable
// references, so the callbacks below keep their original identities.
interface CommitDeps {
  toastRef: MutableRefObject<ToastApi | null>
  appendMessage: ChatStoreState['appendMessage']
  updateThread: ChatStoreState['updateThread']
}

// useCommit turns a finished stream into a permanent assistant message: the
// full-replace save, the thread token badges and the commit itself (tool
// trail, fix-proposal outcomes, served model and billed rates pinned on).
export function useCommit({toastRef, appendMessage, updateThread}: CommitDeps) {
  // Threads whose last save failed (one toast per thread until a save lands).
  const saveFailedRef = useRef(new Set<string>())

  // persistThread full-replace saves a thread (transient error bubbles
  // filtered out) — but only once its server history has loaded: saving a
  // thread whose history never arrived would overwrite the stored
  // conversation with just the turns on screen. The server persists each turn
  // itself, so skipping is safe. History off: nothing is stored, and the
  // request builder sends the thread's messages with each turn instead.
  const persistThread = useCallback(
    (threadId: string, flowId: string) => {
      if (!conversationHistoryEnabled()) return
      const st = useChatStore.getState()
      const thread = st.threads.find(t => t.id === threadId)
      if (!thread || !st.isHistorySynced(threadId)) return
      const persistable = st.getMessages(threadId).filter(m => m.finishReason !== 'error')
      chatApi
        .saveConversation(flowId, conversationKey(thread), persistable)
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

  // bumpThreadTokens adds a finished turn's usage to the thread's badge
  // totals. Shared by the done commit and the error path, so a BILLED
  // failure — PROVIDER_OUTPUT_LIMIT after a 16k reasoning budget —
  // reaches the meter too. Reads the thread fresh: two streams on one
  // thread committing close together must not drop each other's tokens.
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

  // commitAssistantMessage appends the streamed text as a permanent assistant
  // message and persists the thread. Saves under the thread's own flowId —
  // never the open document's: a done event can land after the user switched
  // documents. The stream slot's tool trail and fix-proposal outcome (if any)
  // are pinned onto the message here — the slot dies with endStream, the
  // message is what survives the reload. So are the served model and billed
  // rates the done event reported (opts.meta). `save: false` leaves the
  // server's stored copy alone (a re-attached turn the server stores itself).
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
        // The model and the name it went by when this turn was sent: history
        // and exports keep naming a model that is later renamed or removed.
        ...messageModel(acc),
        tokensIn: opts.tokensIn,
        tokensOut: opts.tokensOut,
        finishReason: opts.finishReason,
      }
      applyDoneMeta(msg, opts.meta)
      if (slot && slot.toolCalls.length > 0) {
        msg.toolCalls = slot.toolCalls.map(tc => ({...tc}))
      }
      // EVERY proposal card of the stream is pinned (sequential fixes stack),
      // each with its per-item outcomes (batches).
      if (slot && slot.fixProposals.length > 0) {
        msg.fixProposals = slot.fixProposals.map(card => ({
          proposalId: card.proposalId,
          ruleId: card.ruleId,
          fixType: card.fixType,
          blockLabel: card.blockLabel,
          line: card.line,
          summary: card.summary,
          status: card.status,
          message: card.message,
          items: card.items.map(it => ({...it})),
        }))
      }
      appendMessage(threadId, msg)
      // Thread-tab badges: this conversation was agentic — flag it once
      // tools ran or a fix landed; updateThread is idempotent per flag.
      if ((msg.toolCalls?.length ?? 0) > 0 || (msg.fixProposals?.length ?? 0) > 0) {
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
