import {useCallback, useRef} from 'react'
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
import {classifyChatFailure, budgetDetail, providerErrorInfo} from '@/lib/chatErrors'
import type {ChatMessage} from '@/types'
import {buildChatRequest} from '../useChatRequestBuilder'
import type {AccMeta, ChatStoreState, SendOptions, StreamAcc, StreamMeta, ToastApi} from './types'
import {messageModel, resumedTurn, providerErrorLine} from './helpers'
import type {useCommit} from './commit'

type StreamingApi = ReturnType<typeof useStreamingMessage>

// The pieces of the engine a turn's lifecycle needs. Stable references, so
// the callbacks below keep their original identities.
interface LifecycleDeps {
  toastRef: MutableRefObject<ToastApi | null>
  streamAccRef: MutableRefObject<Map<string, StreamAcc>>
  teardownRef: MutableRefObject<(streamId: string) => void>
  registerStream: StreamingApi['registerStream']
  resumeInto: StreamingApi['resumeInto']
  cancelStream: (streamId: string) => void
  dropAcc: (streamId: string) => void
  beginAcc: (streamId: string, threadId: string, meta: AccMeta) => void
  commitAssistantMessage: ReturnType<typeof useCommit>['commitAssistantMessage']
  notifyBudget: (detail: string) => void
  startStream: ChatStoreState['startStream']
  endStream: ChatStoreState['endStream']
  appendMessage: ChatStoreState['appendMessage']
  updateThread: ChatStoreState['updateThread']
}

// useTurnLifecycle owns one turn's life: sending (send), re-attaching after
// a reload (resumeAfterReload), and the user-driven exits (stop, abandon,
// stopAndCommit) — plus the per-thread generation tokens used to discard
// stale sends.
export function useTurnLifecycle({
  toastRef,
  streamAccRef,
  teardownRef,
  registerStream,
  resumeInto,
  cancelStream,
  dropAcc,
  beginAcc,
  commitAssistantMessage,
  notifyBudget,
  startStream,
  endStream,
  appendMessage,
  updateThread,
}: LifecycleDeps) {
  // Per-thread generation token: each send bumps it; a stale send detects
  // it's no longer current after an await and self-cancels.
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

  // endStreamIfOwn clears a thread's slot only while it still belongs to this
  // send. A send that lost its generation (Stop, the thread closed, logout)
  // must not end whatever stream replaced it: a queued follow-up drains the
  // instant Stop frees the thread, and the stale send's late cleanup used to
  // delete the follow-up's slot — the engine then cancelled it as orphaned
  // while the backend generated (and billed) a reply nobody was listening to.
  const endStreamIfOwn = useCallback(
    (threadId: string, streamId: string) => {
      if (useChatStore.getState().streams[threadId]?.streamId === streamId) endStream(threadId)
    },
    [endStream],
  )

  // stopAndCommit cancels a stream the user stopped mid-generation, keeping
  // whatever text was already generated as a permanent 'interrupted' message
  // instead of discarding it.
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

  // send runs one turn on a thread: append the user message, open the
  // stream's SSE listener, then create the backend stream. Reports
  // SYNCHRONOUSLY whether the message was accepted (false = nothing was
  // appended: unknown thread, already streaming, or the concurrency cap — the
  // caller keeps its draft); the network work continues detached, so a caller
  // can reset its composer the moment the message is on screen instead of
  // after two round-trips.
  const send = useCallback(
    (threadId: string, text: string, opts: SendOptions): boolean => {
      const st = useChatStore.getState()
      const thread = st.threads.find(t => t.id === threadId)
      if (!thread) return false
      // Per-thread guard: one stream per thread (enforced structurally by the
      // store's streams map, but checked here to no-op a double-send race).
      if (st.streams[threadId]) return false
      // Global cap (mirrors backend maxConcurrentStreamsPerScope): reject early
      // with a toast so the user gets immediate feedback rather than a rejected
      // POST after a round-trip.
      if (!st.canStartStream()) {
        toastRef.current?.warning(i18n.t('chat:errors.busyTitle'), {
          description: i18n.t('chat:errors.busyBody', {count: MAX_CONCURRENT_STREAMS}),
        })
        return false
      }

      // A label is only ever a name: never the id itself or a registry id —
      // it is stored with the reply, the pin and the in-flight record, and
      // would then beat the listing's name for good.
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
      // Built BEFORE the user message is appended: when history travels with
      // the request, the backend appends userMessage itself.
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
      // The store titles an untitled thread from the first user message it
      // sees; remembered so a withdrawn message takes its title back with it.
      const hadTitle = !!thread.title
      appendMessage(threadId, userMsg)
      if (isFirstMessage && !hadTitle && text.trim()) {
        updateThread(threadId, {title: text.replace(/\n+/g, ' ').trim().slice(0, 50)})
      }
      // No save-on-send — the backend reconstructs history; save-on-done
      // persists the full user+assistant pair.

      // The accepted send pins the thread to the model it went to — a new
      // thread keeps it from now on, whatever the Default for new chats
      // becomes — unless it is a stand-in for the thread's own model (see
      // SendOptions.pinThread). An empty model (the server's default) pins
      // nothing. Pinned before the network, so a model the server then
      // refuses is the one the thread's notice names.
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

      // withdrawUserMessage takes back a message the backend never accepted:
      // it must not sit on screen as a question with no answer (and be saved
      // as one), so it returns to the composer — the thread's draft, plus the
      // one-shot staged prompt an open composer consumes at once. @-files and
      // the exclude flag are not carried back. A resend puts the exchange it
      // took off back instead (see SendOptions.restore).
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
      // The common teardown of a send that will not go (or stay) live: drop
      // the accumulator, cancel any backend stream the create POST recorded,
      // and free the slot only if it is still this send's.
      const abortSend = () => {
        dropAcc(sid)
        cancelStream(sid)
        endStreamIfOwn(threadId, sid)
      }
      startStream(threadId, sid, msgId)
      // A turn sent without history is server-owned: the server stores the
      // question as the stream starts and the finished answer — tool trail,
      // fix outcomes, finish reason, usage and served model included — as
      // it ends, so the save on done (a full-replace POST of the whole
      // thread) is skipped. A turn that carried its history (resend, history
      // off) owns persistence itself, as before.
      beginAcc(sid, threadId, {...meta, serverOwned: req.messages === undefined})
      // Recorded with the thread (persisted) until the turn ends, so a reload
      // mid-turn re-attaches it labelled with the model it went to.
      updateThread(threadId, {inFlight: {streamId: sid, messageId: msgId, ...meta}})

      const run = async () => {
        try {
          // Subscribe + wait for the SSE connection to be 'open' BEFORE creating
          // the stream so no chunk is lost; registerStream returns false if the
          // engine was unmounted mid-registration — then do NOT create the stream.
          const registered = await registerStream(sid, /*begin*/ false)
          if (!registered || !isCurrentGen(threadId, myGen)) {
            abortSend()
            return
          }
          // Create the stream with the client-generated ID. The backend stores it
          // and — because clientStreamId is set — closes ctl.started immediately,
          // so the worker emits over SSE to the already-subscribed listener.
          req.clientStreamId = sid
          const returnedId = await chatApi.streamChatMessage(req)
          if (!isCurrentGen(threadId, myGen)) {
            // Cancelled while the create POST was in flight; tear down the
            // stream the backend just recorded (abortSend frees the slot only
            // if it is still this send's — a queued follow-up may own it).
            abortSend()
            return
          }
          if (!returnedId) {
            abortSend()
            appendErrorBubble(i18n.t('chat:errors.noStream'))
            return
          }
          // Stream is live; chunks (or a fail-fast error/done) arrive over SSE and
          // are dispatched by the handlers above.
        } catch (e: unknown) {
          if (!isCurrentGen(threadId, myGen)) return
          const errMsg = getErrorMessage(e) || i18n.t('chat:errors.sendFailed')
          // Classify by the envelope's machine-readable code first, falling back
          // to the message for older backends.
          const kind = classifyChatFailure(errMsg, e instanceof ApiError ? e.code : null)
          // Tear down the SSE subscription we opened and cancel any backend
          // stream that the create POST may have recorded before throwing.
          abortSend()
          if (kind === 'capacity') {
            // The backend is at its per-caller cap — another tab or device, or
            // a Stop it had not processed when a queued follow-up drained.
            // Transient, so no *Error* bubble in history: the message goes back
            // to the composer with the busy notice.
            withdrawUserMessage()
            toastRef.current?.warning(i18n.t('chat:errors.busyTitle'), {
              description: i18n.t('chat:errors.busyBody', {count: MAX_CONCURRENT_STREAMS}),
            })
          } else if (kind === 'modelNotAvailable') {
            // The model was removed (or stopped being shared) after the
            // picker listed it. Nothing was generated, so — like the
            // capacity path — the message goes back to the composer instead
            // of leaving an error in history; the listing is refetched, and
            // the thread's notice (raised once that listing lands, or its
            // refetch fails) offers "Choose model". Said at once here: the
            // text moving back to the composer is no explanation.
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
          } else if (e instanceof ApiError && providerErrorInfo(e.code)) {
            // A provider failure the create POST already knew about (no
            // credentials for this provider in the flow's scope): same
            // bubble as the stream's error event.
            const info = providerErrorInfo(e.code)!
            if (info.refreshProviders) useChatStore.getState().bumpProviderEpoch()
            appendErrorBubble(providerErrorLine(info, errMsg), e.code ?? undefined)
          } else {
            // Surface any other create failure as an error bubble so the user
            // knows their message didn't go through.
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
      dropAcc,
      endStreamIfOwn,
      isCurrentGen,
      notifyBudget,
      registerStream,
      startStream,
      toastRef,
      updateThread,
    ],
  )

  // resumeAfterReload re-attaches a stream the server reports as still
  // in-flight for a conversation the client just (re)loaded: a page
  // reload, Tauri window reopen or crash mid-turn no longer loses the reply
  // — the server keeps the stream resumable and persists the finished turn.
  // Mirrors send(): slot + accumulator first, then the SSE subscription,
  // then one authoritative full resume (the user turn is already in the
  // loaded history — persistInitialUserTurn stored it at stream start). If
  // the stream turns out to be gone (403/404 — retention expired or the
  // stream finished and was evicted), drop the slot and let the caller
  // refetch the history, where the server's persisted copy of the turn
  // lands if it exists.
  //
  // The conversation payload does not say which model the in-flight turn
  // used: the thread's record of it does (resumedTurn), and the answer is
  // committed under the message id the server stores it under. Without a
  // record nothing is guessed — neither the thread's pin (the turn may have
  // gone to a stand-in) nor the settings: the answer is committed with no
  // model, and not saved over the server's copy, which the server stored
  // with the model the turn was sent to.
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
        // registerStream rejects when the event connection does not open in
        // time. Unhandled, that left the slot thinking with no stream behind
        // it — queued sends never drained and only Stop recovered.
        let registered: boolean
        try {
          registered = await registerStream(streamId, /*begin*/ false)
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

  // stop ends a thread's in-flight reply at the user's request, keeping the
  // text generated so far as an 'interrupted' message.
  const stop = useCallback(
    (threadId: string) => {
      const slot = useChatStore.getState().streams[threadId]
      if (!slot) return
      const sid = slot.streamId
      // Bump the per-thread gen so any in-flight send sees it's stale and
      // self-cancels.
      bumpGen(threadId)
      if (sid && sid !== 'pending') {
        // stopAndCommit cancels the SSE sub + backend stream, keeping whatever
        // text had streamed before the stop instead of discarding it.
        stopAndCommit(sid, threadId, slot.messageId)
      }
      endStream(threadId)
    },
    [bumpGen, endStream, stopAndCommit],
  )

  // abandon drops a thread's in-flight reply without committing it (the
  // thread is being closed) and cancels it on the backend.
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
