import i18n from '@/i18n'
import {useCallback, useRef, useEffect, useMemo} from 'react'
import {chatApi, flowApi, analysisApi, ApiError} from '@/api'
import {
  useChatStore,
  conversationKey,
  threadPinKey,
  MAX_CONCURRENT_STREAMS,
  FixProposalCard,
  FixProposalItem,
  type ChatThread,
} from '@/stores/chatStore'
import {useStreamingMessage} from '@/hooks/useStreamingMessage'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useOptionalToast} from '@/components/shared/Toast'
import {logger} from '@/lib/logger'
import {storableModelLabel} from '@/lib/modelIds'
import {uuid} from '@/lib/uuid'
import {utf8ByteLength} from '@/lib/utf8'
import {classifyChatFailure, budgetDetail, providerErrorInfo, type ProviderErrorInfo} from '@/lib/chatErrors'
import type {ChatEvent, DoneMeta, ErrorUsage, FixProposalPayload, ToolResultPayload} from '@/lib/chatEvent'
import type {ChatMessage, ProviderID, ToolCallRecord} from '@/types'
import {conversationHistoryEnabled} from '@/lib/chatHistory'
import {buildChatRequest} from './useChatRequestBuilder'

// providerErrorLine is the error bubble's text for a provider failure: the
// localized title, then the server's message (which names the deployment,
// role or limit at fault) as the detail.
function providerErrorLine(info: ProviderErrorInfo, message: string): string {
  return i18n.t('chat:errors.providerBubble', {title: i18n.t(`chat:${info.titleKey}`), message})
}

// StreamMeta is what produced a stream: the provider and model it was sent
// with, and the model's friendly name then (an Azure AI Foundry entry's
// display name — its "m/<id>" means nothing to a reader). Captured at send
// time so the committed message is labelled with the model that actually
// answered, even if the thread's pick changed since.
export interface StreamMeta {
  provider: ProviderID
  model: string
  modelLabel?: string
}

// resumedTurn is what a re-attached stream is labelled with: the thread's
// record of the turn in flight (chatStore.InFlightTurn) — the model the turn
// was actually sent to, a stand-in's included — and the message id the
// server stores its answer under. The payload of a conversation load does
// not say which model the in-flight turn used, so without a record nothing
// is guessed (see resumeAfterReload).
function resumedTurn(thread: ChatThread, streamId: string): {meta: StreamMeta; messageId?: string} | null {
  const rec = thread.inFlight
  if (!rec || rec.streamId !== streamId) return null
  return {
    meta: {provider: rec.provider, model: rec.model, ...(rec.modelLabel ? {modelLabel: rec.modelLabel} : {})},
    ...(rec.messageId ? {messageId: rec.messageId} : {}),
  }
}

// What an accumulator is primed with: a stream's meta — none for a
// re-attached stream with no record of its turn — and whether the server
// owns the answer's stored copy (see resumeAfterReload).
type AccMeta = Partial<StreamMeta> & {serverOwned?: boolean}

// StreamAcc is one in-flight stream's local accumulation state: the growing
// text plus the RAF batching bookkeeping. Keyed by streamId in a ref so
// multiple threads can stream concurrently; the map entry doubles as the
// stale-event guard — a late event whose streamId has no entry is ignored.
interface StreamAcc extends AccMeta {
  threadId: string
  text: string
  raf: number | null
  first: boolean
  // linked: the thread's store slot has been seen carrying this streamId.
  // Only a linked acc whose slot later disappears is orphaned (see the store
  // subscription below); before that, the slot may simply not exist yet.
  linked: boolean
}

// The model fields a committed message (or error bubble) carries: none for
// a stream with no known provider (a re-attached one with no record).
function messageModel(acc: Partial<StreamMeta>): Pick<ChatMessage, 'provider' | 'model' | 'modelLabel'> {
  if (!acc.provider) return {}
  return {provider: acc.provider, model: acc.model ?? '', ...(acc.modelLabel ? {modelLabel: acc.modelLabel} : {})}
}

export interface SendOptions extends StreamMeta {
  files?: string[]
  excludeContext?: boolean
  // Send the thread's (locally truncated) history instead of letting the
  // server replay its stored copy — resend/regenerate.
  includeHistory?: boolean
  // Pin the thread to this provider/model once the send is accepted (its
  // first accepted send is what fixes a new thread's model). False while the
  // thread's own model is only temporarily unavailable and this turn goes to
  // the fallback: the thread keeps its pin and returns to that model once it
  // can be used again. Default true.
  pinThread?: boolean
  // For a turn composed earlier (queued, previewed): the thread's pin then
  // (chatStore.threadPinKey). The send pins only while the thread still has
  // it — a pick made since wins over the captured pair, which is still what
  // this turn goes to.
  pinnedAs?: string
  // A resend (Retry / Regenerate) took the thread's last exchange off to
  // send it again: the thread's messages from before that. A send the
  // server refuses (the model is gone, the per-caller cap) puts them back
  // as they were — the old answer is not lost (and not deleted from the
  // server by the next save), and the question needs no trip back to the
  // composer, which may hold another draft.
  restore?: readonly ChatMessage[]
}

// TurnModel is the model half of a send: the thread's pair as resolved when
// the turn was composed (queued, previewed or sent).
export type TurnModel = Pick<SendOptions, 'provider' | 'model' | 'modelLabel' | 'pinThread'>

type ToastApi = NonNullable<ReturnType<typeof useOptionalToast>>

// ALIAS_PROVIDERS resolve the selected model to another one at answer time
// (Azure AI Foundry: a deployment, `model-router`, an agent). Their model
// listing names the served model once one is known, so the first done that
// reports one refetches /api/providers/list — once per
// thread+deployment+served-model triple, not per turn.
const ALIAS_PROVIDERS: ReadonlySet<ProviderID> = new Set<ProviderID>(['azure-foundry'])

// useChatStreamEngine owns every chat stream: sending a turn, the per-stream
// text accumulator, the SSE handler wiring (via useStreamingMessage), stop /
// abandon, and the per-thread generation tokens used to discard stale sends.
//
// It is mounted ONCE, by ChatStreamProvider, at the app shell — never by the
// chat panel. The panel mounts and unmounts (inspector collapse, mobile
// drawer, pop-out, breakpoint change); owning the streams there meant every
// unmount cancelled the replies in flight and left their threads stuck on
// "Stop". Everything the engine needs therefore comes from the stores or from
// the arguments of a send — nothing from the panel.
export function useChatStreamEngine() {
  const toast = useOptionalToast()
  const toastRef = useRef<ToastApi | null>(toast)
  useEffect(() => {
    toastRef.current = toast
  }, [toast])

  const appendMessage = useChatStore(s => s.appendMessage)
  const updateThread = useChatStore(s => s.updateThread)
  const updateStream = useChatStore(s => s.updateStream)
  const setStreamMeta = useChatStore(s => s.setStreamMeta)
  const addToolCall = useChatStore(s => s.addToolCall)
  const setFixProposal = useChatStore(s => s.setFixProposal)
  const patchFixProposal = useChatStore(s => s.patchFixProposal)
  const replaceStreamTools = useChatStore(s => s.replaceStreamTools)
  const replaceStreamFixes = useChatStore(s => s.replaceStreamFixes)
  const startStream = useChatStore(s => s.startStream)
  const endStream = useChatStore(s => s.endStream)
  const bumpProviderEpoch = useChatStore(s => s.bumpProviderEpoch)

  const streamAccRef = useRef(new Map<string, StreamAcc>())
  // Thread/deployment/served-model triples (joined with a NUL) whose
  // served model already triggered a provider-list refetch.
  const servedModelSeenRef = useRef(new Set<string>())
  const teardownRef = useRef<(streamId: string) => void>(() => {})
  const cancelRef = useRef<(streamId: string) => void>(() => {})
  // Threads whose last save failed (one toast per thread until a save lands).
  const saveFailedRef = useRef(new Set<string>())

  // Per-thread generation token: each send bumps it; a stale send detects
  // it's no longer current after an await and self-cancels.
  const threadGenRef = useRef(new Map<string, number>())
  const bumpGen = useCallback((threadId: string) => {
    const next = (threadGenRef.current.get(threadId) ?? 0) + 1
    threadGenRef.current.set(threadId, next)
    return next
  }, [])
  const isCurrentGen = useCallback((threadId: string, gen: number) => threadGenRef.current.get(threadId) === gen, [])

  const flushAcc = useCallback(
    (streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      acc.raf = null
      // One atomic store update (text + tokens) instead of two set() calls —
      // halves per-frame subscriber notifications during streaming.
      updateStream(acc.threadId, {text: acc.text, tokens: Math.ceil(acc.text.length / 4)})
    },
    [updateStream],
  )

  const onChunk = useCallback(
    (text: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      acc.text += text
      // U1: answer text resuming after a tool phase clears the stale
      // "Using tool…" pulse — without this the last tool's label stayed lit
      // under the entire final answer.
      const slot = useChatStore.getState().streams[acc.threadId]
      const patch: Partial<import('@/stores/chatStore').StreamSlot> = {tokens: Math.ceil(acc.text.length / 4)}
      if (slot?.toolStatus != null) patch.toolStatus = null
      // F-40: answer text on screen means no thinking indicator either — a
      // re-armed one (onToolResult) can never stick past the next chunk.
      if (slot?.isThinking) patch.isThinking = false
      if (!acc.first) {
        // First chunk: flush synchronously so the slot text appears in the same
        // React render that clears the thinking indicator — no empty-frame gap.
        acc.first = true
        patch.text = acc.text
        patch.isThinking = false
        updateStream(acc.threadId, patch)
      } else {
        if (patch.toolStatus !== undefined || patch.isThinking !== undefined) {
          updateStream(acc.threadId, {toolStatus: null, isThinking: false})
        }
        if (acc.raf === null) {
          acc.raf = requestAnimationFrame(() => flushAcc(streamId))
        }
      }
    },
    [flushAcc, updateStream],
  )

  const onReplace = useCallback(
    (text: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      acc.text = text
      acc.first = true
      updateStream(acc.threadId, {text, isThinking: false, tokens: Math.ceil(text.length / 4)})
    },
    [updateStream],
  )

  const onToolStatus = useCallback(
    (label: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      setStreamMeta(acc.threadId, {isThinking: false, toolStatus: label})
    },
    [setStreamMeta],
  )

  // onToolResult records one finished tool execution on the thread's stream
  // slot — the live view of the trail that commitAssistantMessage pins onto
  // the permanent assistant message.
  const onToolResult = useCallback(
    (record: ToolResultPayload, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      addToolCall(acc.threadId, {
        name: record.name,
        label: record.label,
        ok: record.ok,
        durationMs: record.durationMs,
        summary: record.summary,
      })
      // The finished call's "running" label must not keep pulsing through
      // the (possibly minutes-long) silent reasoning phase that follows.
      // Clear it only while it is still THIS call's — a second tool may
      // already be running under the same label — and re-arm the thinking
      // indicator only before any answer text exists (a second indicator
      // above an already-visible bubble would be wrong).
      const slot = useChatStore.getState().streams[acc.threadId]
      if (slot && slot.toolStatus === record.label) {
        setStreamMeta(acc.threadId, {toolStatus: null, isThinking: !acc.first})
      }
    },
    [addToolCall, setStreamMeta],
  )

  // takeAcc removes and returns a finished stream's accumulation state,
  // cancelling any pending flush. The store slot must still match the
  // streamId — a thread closed mid-stream drops its slot, and its result is
  // then discarded rather than committed to a dead thread.
  const takeAcc = useCallback((streamId: string): StreamAcc | null => {
    // Torn down even with no accumulator left: a subscription that registered
    // after the acc was dropped (registerStream was mid-flight) must not linger.
    teardownRef.current(streamId)
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return null
    streamAccRef.current.delete(streamId)
    if (acc.raf !== null) {
      cancelAnimationFrame(acc.raf)
      acc.raf = null
    }
    const slot = useChatStore.getState().streams[acc.threadId]
    if (!slot || slot.streamId !== streamId) return null
    return acc
  }, [])

  // dropAcc clears an acc entry on a send that never completed (stale-gen
  // return, create-POST failure). It cancels any pending RAF so a straggler
  // flush can't fire after the slot is gone. Unlike takeAcc it does NOT touch
  // the store slot or tear down SSE (the caller does cancelStream/endStream).
  const dropAcc = useCallback((streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return
    if (acc.raf !== null) cancelAnimationFrame(acc.raf)
    streamAccRef.current.delete(streamId)
  }, [])

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

  // beginAcc primes the accumulator for a stream that's about to start, so
  // the first chunk event finds its key.
  const beginAcc = useCallback((streamId: string, threadId: string, meta: AccMeta) => {
    const linked = useChatStore.getState().streams[threadId]?.streamId === streamId
    streamAccRef.current.set(streamId, {threadId, text: '', raf: null, first: false, linked, ...meta})
  }, [])

  // persistThread full-replace saves a thread (transient error bubbles
  // filtered out) — but only once its server history has loaded: saving a
  // thread whose history never arrived would overwrite the stored
  // conversation with just the turns on screen. The server persists each turn
  // itself, so skipping is safe. History off: nothing is stored, and the
  // request builder sends the thread's messages with each turn instead.
  const persistThread = useCallback((threadId: string, flowId: string) => {
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
  }, [])

  // bumpThreadTokens adds a finished turn's usage to the thread's badge
  // totals. Shared by the done commit and the error path (F-15), so a
  // BILLED failure — PROVIDER_OUTPUT_LIMIT after a 16k reasoning budget —
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
      if (opts.meta?.servedModel) msg.servedModel = opts.meta.servedModel
      if (opts.meta?.inputCostPerM !== undefined) msg.inputCostPerM = opts.meta.inputCostPerM
      if (opts.meta?.outputCostPerM !== undefined) msg.outputCostPerM = opts.meta.outputCostPerM
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
      // Thread-tab badges (F10): this conversation was agentic — flag it once
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
        bumpProviderEpoch()
      }
    },
    [bumpProviderEpoch, commitAssistantMessage, endStream, takeAcc],
  )

  // notifyBudget shows the dedicated daily-budget notice: the user needs to
  // know it resets at midnight, not that something broke.
  const notifyBudget = useCallback((detail: string) => {
    toastRef.current?.warning(i18n.t('chat:errors.budgetTitle', {detail}), {
      description: i18n.t('chat:errors.budgetBody'),
    })
  }, [])

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
      // A billed failure reports its usage (F-15): same fields as a done, on
      // the bubble and in the thread totals. The error bubble itself is never
      // persisted — the thread totals are, which is what the meter reads.
      if (usage?.tokensIn != null) msg.tokensIn = usage.tokensIn
      if (usage?.tokensOut != null) msg.tokensOut = usage.tokensOut
      if (usage?.meta?.servedModel) msg.servedModel = usage.meta.servedModel
      if (usage?.meta?.inputCostPerM !== undefined) msg.inputCostPerM = usage.meta.inputCostPerM
      if (usage?.meta?.outputCostPerM !== undefined) msg.outputCostPerM = usage.meta.outputCostPerM
      // U3: a loop that fails at iteration 5 still ran 4 tools — keep the
      // trail on the error bubble so the user sees what happened before the
      // failure (error messages are excluded from persistence by design).
      if (slot && slot.toolCalls.length > 0) {
        msg.toolCalls = slot.toolCalls.map(tc => ({...tc}))
      }
      // Append unconditionally (F1.9): gating the error bubble on a doc
      // being CURRENTLY loaded swallowed stream failures after the user
      // switched flows — their question appeared unanswered, silently.
      appendMessage(acc.threadId, msg)
      bumpThreadTokens(acc.threadId, usage?.tokensIn, usage?.tokensOut)
      endStream(acc.threadId)
    },
    [appendMessage, bumpProviderEpoch, bumpThreadTokens, endStream, notifyBudget, takeAcc],
  )

  // onAppend adds a delta-resume tail to the stream's accumulated text and
  // flushes it synchronously to the slot (no RAF — a resume delivers a known
  // tail after a reconnect, not a stream of live chunks). Used by
  // useStreamingMessage.resumeInto in 'delta' mode.
  const onAppend = useCallback(
    (delta: string, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc || !delta) return
      acc.text += delta
      acc.first = true
      if (acc.raf !== null) {
        cancelAnimationFrame(acc.raf)
        acc.raf = null
      }
      updateStream(acc.threadId, {text: acc.text, isThinking: false, tokens: Math.ceil(acc.text.length / 4)})
    },
    [updateStream],
  )

  // getAccLength reports the accumulated text's UTF-8 BYTE length for delta
  // resume — the backend slices by byte offset, not UTF-16 .length.
  const getAccLength = useCallback((streamId: string) => {
    const acc = streamAccRef.current.get(streamId)
    return acc ? utf8ByteLength(acc.text) : 0
  }, [])

  // refreshAfterFix reloads the flow a chat-driven fix just changed: findings
  // re-analyze by flow id, and when the fixed flow is the currently-open
  // local document, the canvas refreshes via loadFlowFromPath (cloud canvases
  // refresh through the websocket flow-change sync). Both are best-effort —
  // the model's outcome text and the card state stand on their own.
  const refreshAfterFix = useCallback((threadId: string) => {
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
  }, [])

  // onFixProposal shows an approval card (single or batch) on the stream's
  // thread; the flat fields mirror items[0] for single-fix prompts.
  const onFixProposal = useCallback(
    (proposal: FixProposalPayload, streamId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (!acc) return
      const first = proposal.items[0]
      setFixProposal(acc.threadId, {
        proposalId: proposal.proposalId,
        status: 'pending',
        items: proposal.items.map(it => ({...it, status: 'pending' as const})),
        ruleId: first?.ruleId ?? '',
        fixType: first?.fixType ?? '',
        blockLabel: first?.blockLabel ?? '',
        line: first?.line ?? 0,
        summary: first?.summary ?? '',
      })
    },
    [setFixProposal],
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
      // Only map statuses the card type knows; unknown statuses still clear
      // the buttons via 'error'.
      const known = ['applying', 'applied', 'applied-unresolved', 'declined', 'timeout', 'error'] as const
      const cardStatus = (known as readonly string[]).includes(status) ? (status as FixProposalCard['status']) : 'error'
      // Per-item outcome patches (batch decisions carry items[]). A
      // single-fix decision carries none — its one item mirrors the card, so
      // the card status propagates to the item.
      const itemKnown = ['applied', 'applied-unresolved', 'error', 'already-resolved'] as const
      const itemFromWire = (rid: string, status: string, message?: string) => ({
        ruleId: rid,
        patch: {
          status: ((itemKnown as readonly string[]).includes(status) ? status : 'error') as FixProposalItem['status'],
          message,
        },
      })
      let itemPatches = (items ?? []).map(it => itemFromWire(it.ruleId, it.status, it.message))
      if (itemPatches.length === 0) {
        const slot = useChatStore.getState().streams[acc.threadId]
        const card = slot?.fixProposals.find(c => c.proposalId === proposalId)
        if (card && card.items.length === 1 && card.items[0].status === 'pending') {
          itemPatches = [
            itemFromWire(
              card.items[0].ruleId,
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
    [patchFixProposal, refreshAfterFix],
  )

  // respondFixProposal delivers the user's Approve/Dismiss for a thread's
  // pending proposal. Optimistically marks applying/declined so the buttons
  // lock immediately; the backend's fix_decision event is authoritative.
  const respondFixProposal = useCallback(
    (threadId: string, approved: boolean, proposalId?: string, excludedItemIndices?: number[]) => {
      // View-only flows never dispatch an APPLY decision (F1.5): approval
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
      const cards: FixProposalCard[] = []
      const byId = new Map<string, FixProposalCard>()
      for (const ev of events) {
        if (ev.kind === 'tool-result') {
          calls.push({name: ev.name, label: ev.label, ok: ev.ok, durationMs: ev.durationMs, summary: ev.summary})
        } else if (ev.kind === 'fix-proposal') {
          const first = ev.items[0]
          const card: FixProposalCard = {
            proposalId: ev.proposalId,
            status: 'pending',
            items: ev.items.map(it => ({...it, status: 'pending' as const})),
            ruleId: first?.ruleId ?? '',
            fixType: first?.fixType ?? '',
            blockLabel: first?.blockLabel ?? '',
            line: first?.line ?? 0,
            summary: first?.summary ?? '',
          }
          byId.set(card.proposalId, card)
          cards.push(card)
        } else if (ev.kind === 'fix-decision') {
          const card = byId.get(ev.proposalId)
          if (!card) continue
          const known = ['applying', 'applied', 'applied-unresolved', 'declined', 'timeout', 'error'] as const
          card.status = (known as readonly string[]).includes(ev.status)
            ? (ev.status as FixProposalCard['status'])
            : 'error'
          card.message = ev.message
          for (const it of ev.items ?? []) {
            const target = card.items.find(x => x.ruleId === it.ruleId)
            if (target) {
              const itemKnown = ['applied', 'applied-unresolved', 'error', 'already-resolved'] as const
              target.status = (
                (itemKnown as readonly string[]).includes(it.status) ? it.status : 'error'
              ) as FixProposalItem['status']
              target.message = it.message
            }
          }
        }
      }
      replaceStreamTools(acc.threadId, calls)
      replaceStreamFixes(acc.threadId, cards)
    },
    [replaceStreamTools, replaceStreamFixes],
  )

  const handler = useMemo(
    () => ({
      onChunk,
      onReplace,
      onDone,
      onError,
      onToolStatus,
      onToolResult,
      onFixProposal,
      onFixDecision,
      onResumeState,
      onAppend,
      getAccLength,
    }),
    [
      onChunk,
      onReplace,
      onDone,
      onError,
      onToolStatus,
      onToolResult,
      onFixProposal,
      onFixDecision,
      onResumeState,
      onAppend,
      getAccLength,
    ],
  )

  const {registerStream, cancel, teardownStream, resumeInto} = useStreamingMessage(handler)
  useEffect(() => {
    teardownRef.current = teardownStream
  }, [teardownStream])
  useEffect(() => {
    cancelRef.current = cancel
  }, [cancel])

  const cancelStream = useCallback((streamId: string) => cancelRef.current(streamId), [])

  // stopAndCommit cancels a stream the user stopped mid-generation, keeping
  // whatever text was already generated as a permanent 'interrupted' message
  // instead of discarding it.
  const stopAndCommit = useCallback(
    (streamId: string, threadId: string, messageId: string) => {
      const acc = streamAccRef.current.get(streamId)
      if (acc) {
        if (acc.raf !== null) cancelAnimationFrame(acc.raf)
        streamAccRef.current.delete(streamId)
        if (acc.text) {
          commitAssistantMessage({...acc, threadId}, messageId, acc.text, {finishReason: 'interrupted'})
        }
      }
      cancelStream(streamId)
    },
    [commitAssistantMessage, cancelStream],
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
          provider: meta.provider,
          model: meta.model,
          ...(meta.modelLabel ? {modelLabel: meta.modelLabel} : {}),
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
      startStream(threadId, sid, msgId)
      beginAcc(sid, threadId, meta)
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
            dropAcc(sid)
            cancelStream(sid)
            endStreamIfOwn(threadId, sid)
            return
          }
          // Create the stream with the client-generated ID. The backend stores it
          // and — because clientStreamId is set — closes ctl.started immediately,
          // so the worker emits over SSE to the already-subscribed listener.
          req.clientStreamId = sid
          const returnedId = await chatApi.streamChatMessage(req)
          if (!isCurrentGen(threadId, myGen)) {
            // Cancelled while the create POST was in flight; tear down the stream
            // the backend just recorded. The slot is ended only if it is still
            // this send's — Stop already freed it, and a queued follow-up may
            // own it by now.
            dropAcc(sid)
            cancelStream(sid)
            endStreamIfOwn(threadId, sid)
            return
          }
          if (!returnedId) {
            dropAcc(sid)
            cancelStream(sid)
            endStreamIfOwn(threadId, sid)
            appendErrorBubble(i18n.t('chat:errors.noStream'))
            return
          }
          // Stream is live; chunks (or a fail-fast error/done) arrive over SSE and
          // are dispatched by the handlers above.
        } catch (e: unknown) {
          if (!isCurrentGen(threadId, myGen)) return
          const errMsg = (e instanceof Error ? e.message : String(e)) || i18n.t('chat:errors.sendFailed')
          // Classify by the envelope's machine-readable code first, falling back
          // to the message for older backends.
          const kind = classifyChatFailure(errMsg, e instanceof ApiError ? e.code : null)
          // Tear down the SSE subscription we opened and cancel any backend stream
          // that the create POST may have recorded before throwing.
          dropAcc(sid)
          cancelStream(sid)
          endStreamIfOwn(threadId, sid)
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
      updateThread,
    ],
  )

  // resumeAfterReload re-attaches a stream the server reports as still
  // in-flight for a conversation the client just (re)loaded (F-21): a page
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
    [beginAcc, dropAcc, endStreamIfOwn, registerStream, resumeInto, startStream],
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

  // A slot can disappear without going through stop/abandon: the logout
  // reset, clearFlowThreads, or a thread closed through the store directly.
  // Its accumulator must go too, and the backend stream be cancelled — else
  // the provider keeps generating (and billing) for a listener that is gone.
  useEffect(
    () =>
      useChatStore.subscribe((state, prev) => {
        if (state.streams === prev.streams) return
        for (const [sid, acc] of streamAccRef.current) {
          const slot = state.streams[acc.threadId]
          if (slot && slot.streamId === sid) {
            acc.linked = true
            continue
          }
          if (!acc.linked) continue
          // The stream went away from under an in-flight send (logout, the
          // thread closed): invalidate that send too, or it goes on to create
          // the backend stream for a slot that no longer exists.
          bumpGen(acc.threadId)
          dropAcc(sid)
          cancelStream(sid)
        }
      }),
    [bumpGen, cancelStream, dropAcc],
  )

  // The host unmounting (app teardown) must not leave slots claiming to
  // stream: end every thread that still has an accumulator. The backend
  // streams themselves are cancelled by useStreamingMessage's own cleanup.
  useEffect(() => {
    const accs = streamAccRef.current
    return () => {
      const threads = new Set<string>()
      for (const acc of accs.values()) {
        if (acc.raf !== null) cancelAnimationFrame(acc.raf)
        threads.add(acc.threadId)
      }
      accs.clear()
      for (const tid of threads) useChatStore.getState().endStream(tid)
    }
  }, [])

  return useMemo(
    () => ({
      send,
      stop,
      abandon,
      respondFixProposal,
      resumeAfterReload,
      // Lower-level pieces, exposed for tests and the resend path.
      registerStream,
      cancelStream,
      beginAcc,
      dropAcc,
      stopAndCommit,
      commitAssistantMessage,
      bumpGen,
      isCurrentGen,
    }),
    [
      send,
      stop,
      abandon,
      respondFixProposal,
      resumeAfterReload,
      registerStream,
      cancelStream,
      beginAcc,
      dropAcc,
      stopAndCommit,
      commitAssistantMessage,
      bumpGen,
      isCurrentGen,
    ],
  )
}

export type ChatStreamEngine = ReturnType<typeof useChatStreamEngine>
