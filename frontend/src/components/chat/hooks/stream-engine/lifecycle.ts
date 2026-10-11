import {useCallback, useEffect, useRef} from 'react'
import type {MutableRefObject} from 'react'
import {getErrorMessage} from '@/lib/getErrorMessage'
import i18n from '@/i18n'
import {chatApi, ApiError} from '@/api'
import {useChatStore, threadPinKey, MAX_CONCURRENT_STREAMS} from '@/stores/chatStore'
import {useStreamingMessage} from '@/hooks/useStreamingMessage'
import {useSettingsStore} from '@/stores/settingsStore'
import {logger} from '@/lib/logger'
import {storableModelLabel} from '@/lib/modelIds'
import {uuid} from '@/lib/uuid'
import {
  classifyChatFailure,
  budgetDetail,
  providerErrorInfo,
  retryAfterHint,
  serverRestartDelayMs,
} from '@/lib/chatErrors'
import type {ChatMessage} from '@/types'
import {buildChatRequest} from '../useChatRequestBuilder'
import type {AccMeta, ChatStoreState, SendOptions, StreamAcc, StreamMeta, ToastApi} from './types'
import {messageModel, resumedTurn, providerErrorLine} from './helpers'
import type {useCommit} from './commit'

type StreamingApi = ReturnType<typeof useStreamingMessage>

interface LifecycleDeps {
  toastRef: MutableRefObject<ToastApi | null>
  streamAccRef: MutableRefObject<Map<string, StreamAcc>>
  teardownRef: MutableRefObject<(streamId: string) => void>
  autoResendRef: MutableRefObject<(streamId: string) => string | null>
  registerStream: StreamingApi['registerStream']
  confirmStream: StreamingApi['confirmStream']
  resumeInto: StreamingApi['resumeInto']
  cancelStream: (streamId: string) => void
  dropAcc: (streamId: string) => void
  beginAcc: (streamId: string, threadId: string, meta: AccMeta) => void
  commitAssistantMessage: ReturnType<typeof useCommit>['commitAssistantMessage']
  notifyBudget: (detail: string) => void
  startStream: ChatStoreState['startStream']
  endStream: ChatStoreState['endStream']
  updateStream: ChatStoreState['updateStream']
  appendMessage: ChatStoreState['appendMessage']
  updateThread: ChatStoreState['updateThread']
}

export function useTurnLifecycle({
  toastRef,
  streamAccRef,
  teardownRef,
  autoResendRef,
  registerStream,
  confirmStream,
  resumeInto,
  cancelStream,
  dropAcc,
  beginAcc,
  commitAssistantMessage,
  notifyBudget,
  startStream,
  endStream,
  updateStream,
  appendMessage,
  updateThread,
}: LifecycleDeps) {
  const threadGenRef = useRef(new Map<string, number>())
  const bumpGen = useCallback(
    (threadId: string) => {
      const next = (threadGenRef.current.get(threadId) ?? 0) + 1
      threadGenRef.current.set(threadId, next)
      return next
    },
    [threadGenRef],
  )
  const isCurrentGen = useCallback(
    (threadId: string, gen: number) => threadGenRef.current.get(threadId) === gen,
    [threadGenRef],
  )

  const endStreamIfOwn = useCallback(
    (threadId: string, streamId: string) => {
      if (useChatStore.getState().streams[threadId]?.streamId === streamId) endStream(threadId)
    },
    [endStream],
  )

  const stopAndCommit = useCallback(
    (streamId: string, threadId: string, messageId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (acc) {
        dropAcc(streamId)
        if (acc.text) {
          commitAssistantMessage({...acc, threadId}, messageId, acc.text, {finishReason: 'interrupted'})
        }
      }
      cancelStream(streamId)
    },
    [streamAccRef, commitAssistantMessage, cancelStream, dropAcc],
  )

  const send = useCallback(
    (threadId: string, text: string, opts: SendOptions): boolean => {
      const st = useChatStore.getState()
      const thread = st.threads.find(t => t.id === threadId)
      if (!thread) return false
      if (st.streams[threadId]) return false
      if (!st.canStartStream()) {
        toastRef.current?.warning(i18n.t('chat:errors.busyTitle'), {
          description: i18n.t('chat:errors.busyBody', {count: MAX_CONCURRENT_STREAMS}),
        })
        return false
      }

      const modelLabel = storableModelLabel(opts.modelLabel, opts.model)
      const meta: StreamMeta = {provider: opts.provider, model: opts.model, ...(modelLabel ? {modelLabel} : {})}
      const userMsg: ChatMessage = {
        id: uuid(),
        role: 'user',
        content: text,
        timestamp: new Date().toISOString(),
        contextBlockId: thread.contextBlockId ?? undefined,
      }
      const msgId = uuid()
      const req = buildChatRequest({
        thread,
        provider: opts.provider,
        model: opts.model,
        aiSettings: useSettingsStore.getState().settings.ai,
        messages: st.getMessages(threadId),
        text,
        overrideFiles: opts.files,
        excludeContext: opts.excludeContext,
        includeHistory: opts.includeHistory,
        userMessageId: userMsg.id,
        assistantMessageId: msgId,
      })

      const isFirstMessage = st.getMessages(threadId).length === 0
      const hadTitle = !!thread.title
      appendMessage(threadId, userMsg)
      if (isFirstMessage && !hadTitle && text.trim()) {
        updateThread(threadId, {title: text.replace(/\n+/g, ' ').trim().slice(0, 50)})
      }

      const repickedSince = opts.pinnedAs !== undefined && opts.pinnedAs !== threadPinKey(thread)
      if (opts.pinThread !== false && meta.model && !repickedSince) {
        st.setThreadModel(threadId, {provider: meta.provider, model: meta.model, modelLabel: meta.modelLabel})
      }
      st.setModelRefusal(threadId, null)

      const appendErrorBubble = (content: string, errorCode?: string) =>
        appendMessage(threadId, {
          id: uuid(),
          role: 'assistant',
          content,
          timestamp: new Date().toISOString(),
          ...messageModel(meta),
          finishReason: 'error',
          ...(errorCode ? {errorCode} : {}),
        })

      const withdrawUserMessage = () => {
        const now = useChatStore.getState()
        if (opts.restore) {
          now.setThreadMessages(threadId, opts.restore)
          return
        }
        now.removeMessage(threadId, userMsg.id)
        if (!hadTitle) updateThread(threadId, {title: ''})
        now.setDraft(threadId, text)
        now.setStagedPrompt({threadId, text})
      }

      const myGen = bumpGen(threadId)
      const sid = uuid()
      const abortSend = () => {
        dropAcc(sid)
        cancelStream(sid)
        endStreamIfOwn(threadId, sid)
      }
      startStream(threadId, sid, msgId)
      beginAcc(sid, threadId, {
        ...meta,
        serverOwned: req.messages === undefined,
        resend: {
          text,
          userMessageId: userMsg.id,
          opts: {...meta, files: opts.files, excludeContext: opts.excludeContext},
        },
        ...(opts.autoRetry ? {autoRetried: true} : {}),
      })
      updateThread(threadId, {inFlight: {streamId: sid, messageId: msgId, ...meta}})

      const run = async () => {
        try {
          const registered = await registerStream(sid,  false)
          if (!registered || !isCurrentGen(threadId, myGen)) {
            abortSend()
            return
          }
          req.clientStreamId = sid
          let returnedId: string
          try {
            returnedId = await chatApi.streamChatMessage(req)
          } catch (e) {
            const acc = streamAccRef.current.get(sid)
            const restarting = e instanceof ApiError && classifyChatFailure(e.message, e.code) === 'serverRestarting'
            if (!restarting || !acc || acc.autoRetried) throw e
            acc.autoRetried = true
            updateStream(threadId, {toolStatus: i18n.t('chat:errors.serverRestarting')})
            await new Promise(resolve => setTimeout(resolve, serverRestartDelayMs(retryAfterHint(e))))
            if (!isCurrentGen(threadId, myGen)) {
              abortSend()
              return
            }
            teardownRef.current(sid)
            if (!(await registerStream(sid,  false)) || !isCurrentGen(threadId, myGen)) {
              abortSend()
              return
            }
            updateStream(threadId, {toolStatus: null})
            returnedId = await chatApi.streamChatMessage(req)
          }
          if (!isCurrentGen(threadId, myGen)) {
            abortSend()
            return
          }
          if (!returnedId) {
            abortSend()
            appendErrorBubble(i18n.t('chat:errors.noStream'))
            return
          }
          confirmStream(sid)
        } catch (e: unknown) {
          if (!isCurrentGen(threadId, myGen)) return
          const errMsg = getErrorMessage(e) || i18n.t('chat:errors.sendFailed')
          const kind = classifyChatFailure(errMsg, e instanceof ApiError ? e.code : null)
          abortSend()
          if (kind === 'capacity') {
            withdrawUserMessage()
            toastRef.current?.warning(i18n.t('chat:errors.busyTitle'), {
              description: i18n.t('chat:errors.busyBody', {count: MAX_CONCURRENT_STREAMS}),
            })
          } else if (kind === 'modelNotAvailable') {
            withdrawUserMessage()
            const store = useChatStore.getState()
            store.bumpProviderEpoch()
            if (meta.model) {
              store.setModelRefusal(threadId, {
                provider: meta.provider,
                model: meta.model,
                ...(meta.modelLabel ? {modelLabel: meta.modelLabel} : {}),
                epoch: useChatStore.getState().providerEpoch,
              })
            }
            toastRef.current?.warning(i18n.t('chat:errors.modelNotAvailable'), {
              description: i18n.t(
                opts.restore ? 'chat:errors.modelNotAvailableResend' : 'chat:errors.modelNotAvailableSend',
              ),
            })
          } else if (kind === 'budget') {
            const detail = budgetDetail(errMsg)
            notifyBudget(detail)
            appendErrorBubble(i18n.t('chat:errors.budgetBubble', {detail}))
          } else if (kind === 'serverRestarting') {
            appendErrorBubble(i18n.t('chat:errors.generic', {message: i18n.t('chat:errors.serverRestarted')}))
          } else if (e instanceof ApiError && providerErrorInfo(e.code)) {
            const info = providerErrorInfo(e.code)!
            if (info.refreshProviders) useChatStore.getState().bumpProviderEpoch()
            appendErrorBubble(providerErrorLine(info, errMsg), e.code ?? undefined)
          } else {
            logger.warn('chat stream create failed', e)
            appendErrorBubble(i18n.t('chat:errors.generic', {message: errMsg}))
          }
        }
      }
      void run()
      return true
    },
    [
      appendMessage,
      beginAcc,
      bumpGen,
      cancelStream,
      confirmStream,
      dropAcc,
      endStreamIfOwn,
      isCurrentGen,
      notifyBudget,
      registerStream,
      startStream,
      streamAccRef,
      teardownRef,
      toastRef,
      updateStream,
      updateThread,
    ],
  )

  const autoResendTimersRef = useRef(new Map<string, ReturnType<typeof setTimeout>>())
  useEffect(() => {
    const timers = autoResendTimersRef.current
    return () => {
      for (const t of timers.values()) clearTimeout(t)
      timers.clear()
    }
  }, [])

  const scheduleAutoResend = useCallback(
    (streamId: string): string | null => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc?.resend || acc.autoRetried) return null
      const threadId = acc.threadId
      const st = useChatStore.getState()
      const slot = st.streams[threadId]
      if (!slot || slot.streamId !== streamId || slot.approvals.length > 0) return null
      if (!st.isHistorySynced(threadId)) return null
      const msgs = st.getMessages(threadId)
      if (msgs[msgs.length - 1]?.id !== acc.resend.userMessageId) return null

      acc.autoRetried = true
      teardownRef.current(streamId)
      if (acc.raf !== null) {
        cancelAnimationFrame(acc.raf)
        acc.raf = null
      }
      updateStream(threadId, {
        text: acc.text,
        isThinking: !acc.text,
        toolStatus: i18n.t('chat:errors.serverRestarting'),
      })
      const resend = acc.resend
      const gen = threadGenRef.current.get(threadId) ?? 0
      const timer = setTimeout(() => {
        autoResendTimersRef.current.delete(streamId)
        if (!isCurrentGen(threadId, gen) || streamAccRef.current.get(streamId) !== acc) return
        dropAcc(streamId)
        endStreamIfOwn(threadId, streamId)
        const now = useChatStore.getState()
        const before = now.getMessages(threadId)
        now.removeMessage(threadId, resend.userMessageId)
        const accepted = send(threadId, resend.text, {
          ...resend.opts,
          includeHistory: true,
          pinThread: false,
          restore: before,
          autoRetry: true,
        })
        if (!accepted) now.setThreadMessages(threadId, before)
      }, serverRestartDelayMs())
      autoResendTimersRef.current.set(streamId, timer)
      return threadId
    },
    [dropAcc, endStreamIfOwn, isCurrentGen, send, streamAccRef, teardownRef, updateStream],
  )
  useEffect(() => {
    autoResendRef.current = scheduleAutoResend
  }, [autoResendRef, scheduleAutoResend])

  const resumeAfterReload = useCallback(
    (threadId: string, streamId: string, onGone?: () => void): boolean => {
      const st = useChatStore.getState()
      const thread = st.threads.find(t => t.id === threadId)
      if (!thread || st.streams[threadId] || !st.canStartStream()) return false
      const turn = resumedTurn(thread, streamId)
      startStream(threadId, streamId, turn?.messageId ?? uuid())
      beginAcc(streamId, threadId, turn ? turn.meta : {serverOwned: true})
      const gone = () => {
        dropAcc(streamId)
        teardownRef.current(streamId)
        endStreamIfOwn(threadId, streamId)
        onGone?.()
      }
      void (async () => {
        let registered: boolean
        try {
          registered = await registerStream(streamId,  false)
        } catch (e) {
          logger.warn('chat stream reattach failed', e)
          gone()
          return
        }
        if (!registered) {
          gone()
          return
        }
        resumeInto(streamId, {mode: 'full', onGone: gone})
      })()
      return true
    },
    [beginAcc, dropAcc, endStreamIfOwn, registerStream, resumeInto, startStream, teardownRef],
  )

  const stop = useCallback(
    (threadId: string) => {
      const slot = useChatStore.getState().streams[threadId]
      if (!slot) return
      const sid = slot.streamId
      bumpGen(threadId)
      if (sid && sid !== 'pending') {
        stopAndCommit(sid, threadId, slot.messageId)
      }
      endStream(threadId)
    },
    [bumpGen, endStream, stopAndCommit],
  )

  const abandon = useCallback(
    (threadId: string) => {
      const slot = useChatStore.getState().streams[threadId]
      if (!slot?.streamId || slot.streamId === 'pending') return
      bumpGen(threadId)
      dropAcc(slot.streamId)
      cancelStream(slot.streamId)
    },
    [bumpGen, cancelStream, dropAcc],
  )

  return {bumpGen, isCurrentGen, endStreamIfOwn, stopAndCommit, send, resumeAfterReload, stop, abandon}
}
