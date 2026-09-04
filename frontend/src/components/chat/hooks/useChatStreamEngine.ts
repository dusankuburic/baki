import i18n from '@/i18n'
import {useCallback, useRef, useEffect, useMemo} from 'react'
import {chatApi, flowApi, analysisApi} from '@/api'
import {useChatStore, FixProposalCard, FixProposalItem} from '@/stores/chatStore'
import {useStreamingMessage} from '@/hooks/useStreamingMessage'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {logger} from '@/lib/logger'
import {uuid} from '@/lib/uuid'
import {utf8ByteLength} from '@/lib/utf8'
import type {ChatEvent, FixProposalPayload, ToolResultPayload} from '@/lib/chatEvent'
import type {ChatMessage, FlowDocument, ProviderID, ToolCallRecord} from '@/types'

// StreamAcc is one in-flight stream's local accumulation state: the growing
// text plus the RAF batching bookkeeping. Keyed by streamId in a ref so
// multiple threads can stream concurrently; the map entry doubles as the
// stale-event guard — a late event whose streamId has no entry is ignored.
interface StreamAcc {
  threadId: string
  text: string
  raf: number | null
  first: boolean
}

interface UseChatStreamEngineOptions {
  doc: FlowDocument | null
  provider: ProviderID
  selectedModel: string
  getMessages: (threadId: string) => readonly ChatMessage[]
}

// useChatStreamEngine owns the per-stream text accumulator, the SSE handler
// wiring (via useStreamingMessage), and the per-thread generation tokens used
// to detect and discard stale sends.
export function useChatStreamEngine({doc, provider, selectedModel, getMessages}: UseChatStreamEngineOptions) {
  const appendMessage = useChatStore(s => s.appendMessage)
  const updateThread = useChatStore(s => s.updateThread)
  const updateStream = useChatStore(s => s.updateStream)
  const setStreamMeta = useChatStore(s => s.setStreamMeta)
  const addToolCall = useChatStore(s => s.addToolCall)
  const setFixProposal = useChatStore(s => s.setFixProposal)
  const patchFixProposal = useChatStore(s => s.patchFixProposal)
  const replaceStreamTools = useChatStore(s => s.replaceStreamTools)
  const replaceStreamFixes = useChatStore(s => s.replaceStreamFixes)
  const endStream = useChatStore(s => s.endStream)

  // Refs mirror the latest doc/provider/selectedModel so the long-lived SSE
  // handlers see current values; re-subscribing on change would drop streams.
  const docRef = useRef(doc)
  useEffect(() => {
    docRef.current = doc
  })
  const providerRef = useRef(provider)
  useEffect(() => {
    providerRef.current = provider
  })
  const selectedModelRef = useRef(selectedModel)
  useEffect(() => {
    selectedModelRef.current = selectedModel
  })

  const streamAccRef = useRef(new Map<string, StreamAcc>())
  const teardownRef = useRef<(streamId: string) => void>(() => {})
  const cancelRef = useRef<(streamId: string) => void>(() => {})

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
      if (!acc.first) {
        // First chunk: flush synchronously so the slot text appears in the same
        // React render that clears the thinking indicator — no empty-frame gap.
        acc.first = true
        patch.text = acc.text
        patch.isThinking = false
        updateStream(acc.threadId, patch)
      } else {
        if (patch.toolStatus !== undefined) updateStream(acc.threadId, {toolStatus: null})
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
    },
    [addToolCall],
  )

  // takeAcc removes and returns a finished stream's accumulation state,
  // cancelling any pending flush. The store slot must still match the
  // streamId — a thread closed mid-stream drops its slot, and its result is
  // then discarded rather than committed to a dead thread.
  const takeAcc = useCallback((streamId: string): StreamAcc | null => {
    const acc = streamAccRef.current.get(streamId)
    if (!acc) return null
    streamAccRef.current.delete(streamId)
    if (acc.raf !== null) {
      cancelAnimationFrame(acc.raf)
      acc.raf = null
    }
    teardownRef.current(streamId)
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

  // beginAcc primes the accumulator for a stream that's about to start, so
  // the first chunk event finds its key.
  const beginAcc = useCallback((streamId: string, threadId: string) => {
    streamAccRef.current.set(streamId, {threadId, text: '', raf: null, first: false})
  }, [])

  // commitAssistantMessage appends the streamed text as a permanent assistant
  // message and persists the thread (transient error bubbles filtered out).
  // Saves under the thread's own flowId — NOT docRef.current: a done event can
  // land after the user switched documents. The stream slot's tool trail and
  // fix-proposal outcome (if any) are pinned onto the message here — the slot
  // dies with endStream, the message is what survives the reload.
  const commitAssistantMessage = useCallback(
    (
      threadId: string,
      messageId: string | undefined,
      content: string,
      opts: {tokensIn?: number; tokensOut?: number; finishReason: ChatMessage['finishReason']},
    ) => {
      const thread = useChatStore.getState().threads.find(t => t.id === threadId)
      if (!thread) return
      const slot = useChatStore.getState().streams[threadId]
      const msg: ChatMessage = {
        id: messageId || uuid(),
        role: 'assistant',
        content,
        timestamp: new Date().toISOString(),
        provider: providerRef.current,
        model: selectedModelRef.current,
        tokensIn: opts.tokensIn,
        tokensOut: opts.tokensOut,
        finishReason: opts.finishReason,
      }
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
      const persistable = (getMessages(threadId) as ChatMessage[]).filter(m => m.finishReason !== 'error')
      chatApi.saveConversation(thread.flowId, thread.contextBlockId || 'flow', persistable).catch(err => {
        logger.warn('Failed to save conversation', err)
      })
      if (opts.tokensIn != null || opts.tokensOut != null) {
        updateThread(threadId, {
          tokensIn: (thread.tokensIn ?? 0) + (opts.tokensIn ?? 0),
          tokensOut: (thread.tokensOut ?? 0) + (opts.tokensOut ?? 0),
        })
      }
    },
    [appendMessage, getMessages, updateThread],
  )

  const onDone = useCallback(
    (tokensOut: number, tokensIn: number, streamId: string) => {
      const acc = takeAcc(streamId)
      if (!acc) return
      const slot = useChatStore.getState().streams[acc.threadId]
      commitAssistantMessage(acc.threadId, slot?.messageId, acc.text, {tokensIn, tokensOut, finishReason: 'stop'})
      endStream(acc.threadId)
    },
    [commitAssistantMessage, endStream, takeAcc],
  )

  const onError = useCallback(
    (error: string, streamId: string) => {
      const acc = takeAcc(streamId)
      if (!acc) return
      const slot = useChatStore.getState().streams[acc.threadId]
      const errorLine = i18n.t('chat:errors.generic', {message: error})
      const displayContent = acc.text ? acc.text + '\n\n---\n' + errorLine : errorLine
      const msg: ChatMessage = {
        id: slot?.messageId || uuid(),
        role: 'assistant',
        content: displayContent,
        timestamp: new Date().toISOString(),
        provider: providerRef.current,
        model: selectedModelRef.current,
        finishReason: 'error',
      }
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
      endStream(acc.threadId)
    },
    [appendMessage, endStream, takeAcc],
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
    const curDoc = docRef.current
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
        patchFixProposal(threadId, card.proposalId, {status: 'error', message: String(err)})
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

  const {registerStream, cancel, teardownStream} = useStreamingMessage(handler)
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
          commitAssistantMessage(threadId, messageId, acc.text, {finishReason: 'interrupted'})
        }
      }
      cancelStream(streamId)
    },
    [commitAssistantMessage, cancelStream],
  )

  return {
    registerStream,
    cancelStream,
    beginAcc,
    dropAcc,
    stopAndCommit,
    commitAssistantMessage,
    bumpGen,
    isCurrentGen,
    respondFixProposal,
  }
}
