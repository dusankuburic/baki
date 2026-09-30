import {useTranslation} from 'react-i18next'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import {useProviderSetup} from './hooks/useProviderSetup'
import {useAIChat} from './hooks/useAIChat'
import {logger} from '@/lib/logger'
import {buildBlockLookup} from '@/lib/tree'
import {findModel, modelNameOf, storableModelLabel} from '@/lib/modelIds'
import {isRegistryEntry} from '@/lib/modelGroups'
import {purposeLabel, purposeLabelKey, type PurposeLabelOf} from '@/lib/modelPurpose'
import {resolveThreadModel} from '@/lib/modelSelection'
import {
  MessageBubble,
  ChatMessageList,
  SuggestedPrompts,
  ChatInput,
  ApiKeyMissingState,
  PromptTemplates,
  ChatThreadBar,
  FixProposalCard,
} from '.'
import {useState, useEffect, useMemo, useCallback, useRef, lazy, Suspense} from 'react'
import {AlertTriangle, RefreshCw} from 'lucide-react'
import {useKeyboard} from '@/hooks/useKeyboard'
import {useConfirm, useToast, Spinner} from '@/components/shared'
import {isImeKeydown} from '@/lib/imeKey'
import {useChatStore, threadModelOf} from '@/stores/chatStore'
import {chatApi} from '@/api'
import LiveToolTrail from './LiveToolTrail'
import StreamingBubble from './StreamingBubble'
import type {ChatMessage, ChatTextSize, ModelDetail} from '@/types'
import ChatHeader from './ChatHeader'
import ChatContextBar from './ChatContextBar'
import EmptyChatState from './EmptyChatState'
import ChatErrorBoundary from './ChatErrorBoundary'
import ChatSearchBar from './ChatSearchBar'
import {AZURE_ACCOUNTS_ANCHOR, type PickerProvider} from './ModelPicker'
import ModelUnavailableNotice, {modelNoticeSentences, type ModelNoticeView} from './ModelUnavailableNotice'
import {useChatPanel} from './ChatPanelShell'

// Rarely-rendered overlays load on first open: the context-preview modal only
// appears when the user confirms a context-heavy send, and the help popover
// only on explicit help clicks. Keeping them out of the AITab chunk shaves
// its initial fetch.
const ContextPreviewModal = lazy(() => import('./ContextPreviewModal'))
const ChatHelpPopover = lazy(() => import('./ChatHelpPopover'))

// Welcome copy is keyed by provider id under chat:welcome.*, with a fallback
// for providers that have no dedicated line; above it, the thread's model and
// what it is for.

const EMPTY_MODELS: ModelDetail[] = []

export default function AITab() {
  const {t} = useTranslation('chat')
  const {t: tc} = useTranslation('common')
  const _selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const _document = useFlowStore(s => s.document)
  const _analysisReport = useAnalysisStore(s => (_document ? s.reports.get(_document.id) : undefined))
  // O(1) block-id → meta index built once per document (see FindingsTab);
  // a per-selection tree walk would be O(blocks) on every canvas click.
  const _blockLookup = useMemo(() => (_document ? buildBlockLookup(_document) : null), [_document])
  const selectedBlock = useMemo(
    () => (_blockLookup && _selectedBlockId ? (_blockLookup.get(_selectedBlockId) ?? null) : null),
    [_blockLookup, _selectedBlockId],
  )
  const chatTextSize = useSettingsStore(s => s.settings.appearance.chatTextSize)
  const updateAppearance = useSettingsStore(s => s.updateAppearance)
  const chatPanel = useChatPanel()

  // Per-thread model memory: `selection` is the ACTIVE thread's provider and
  // model (its pin, else its last reply's pair, else the Default for new
  // chats) — every turn this panel sends goes there.
  const {
    configured,
    providersError,
    retryProviders,
    providers,
    listingLoaded,
    selection,
    notice,
    dismissNotice,
    turnModel,
    pickModel,
    threadModelDetail,
    newChatDefaults,
    orgName,
    legacyDefaultNotice,
    dismissLegacyDefaultNotice,
    settings: selectionSettings,
  } = useProviderSetup()

  const labelOf: PurposeLabelOf = useCallback(p => tc(purposeLabelKey(p)), [tc])
  // A listed model's purpose label; none for a catalogue model.
  const purposeOf = useCallback(
    (m: ModelDetail | undefined) => (m && isRegistryEntry(m) ? purposeLabel(m, labelOf) : undefined),
    [labelOf],
  )
  const providerName = useCallback((id: string) => providers.find(p => p.id === id)?.name || id, [providers])
  const listedModels = useCallback(
    (providerId: string | undefined) => providers.find(p => p.id === providerId)?.models ?? EMPTY_MODELS,
    [providers],
  )

  // The header picker's list: the configured providers and their models.
  const pickerProviders: PickerProvider[] = useMemo(
    () =>
      providers
        .filter(p => p.configured)
        .map(p => ({id: p.id, name: p.name, models: p.models, ...(p.shared ? {shared: true} : {})})),
    [providers],
  )

  // The listing's name for a message's model, for a message that kept no
  // label of its own (one that is only an id counts as none).
  const listedModelName = useCallback(
    (m: ChatMessage): string | undefined => {
      if (m.role !== 'assistant' || !m.model || storableModelLabel(m.modelLabel, m.model)) return undefined
      const entry = findModel(m.model, listedModels(m.provider))
      return entry ? entry.displayName || entry.id : undefined
    },
    [listedModels],
  )
  // An exported turn names its model the same way ("removed model" for a
  // registry id nothing names any more), never by its "m/<id>".
  const exportModelName = useCallback(
    (m: ChatMessage): string | undefined => {
      if (!m.model) return undefined
      return modelNameOf(m.model, m.modelLabel, listedModels(m.provider)) ?? t('message.removedModel')
    },
    [listedModels, t],
  )

  const openAzureAccounts = useCallback(() => useUIStore.getState().openSettings('accounts', AZURE_ACCOUNTS_ANCHOR), [])

  // The composer's guidance: the thread is on Azure AI Foundry, and Azure
  // offers nothing to chat with — no model added yet, or none usable (every
  // resource needs its credentials again). Sending would go out with no
  // model at all, so while it holds nothing sends: not the composer, and not
  // the suggested prompts, templates or Retry/Regenerate either (they are
  // disabled with the same reason).
  const azure = providers.find(p => p.id === 'azure-foundry')
  const guidance = useMemo(() => {
    if (!listingLoaded || selection.provider !== 'azure-foundry' || selection.model || !azure?.configured) return null
    const chatModels = azure.models.filter(m => m.kind !== 'embedding')
    if (chatModels.length === 0) {
      return {text: t('composer.noAzureModels'), actionLabel: t('composer.addModels'), onAction: openAzureAccounts}
    }
    const needCredentials = chatModels.every(m => m.status === 'needs_credentials' || m.status === 'unreadable')
    return {
      text: needCredentials ? t('composer.azureNeedsCredentials') : t('composer.azureUnusable'),
      actionLabel: t('composer.openAccounts'),
      onAction: openAzureAccounts,
    }
  }, [listingLoaded, selection.provider, selection.model, azure, t, openAzureAccounts])
  const sendBlockedReason = guidance?.text

  // The model picker's trigger: focus goes back to it when a notice's
  // Dismiss takes the row (and the focused button) away.
  const pickerTriggerRef = useRef<HTMLButtonElement | null>(null)

  const {
    doc,
    selectedBlockId,
    readOnly,
    activeThreadId,
    activeThread,
    activeThreadMessages,
    flowThreads,
    isCurrentThreadStreaming,
    showThinking,
    sourceFiles,
    historyState,
    retryHistoryLoad,
    contextPreview,
    pendingMessage,
    fixProposals,
    respondFixProposal,
    switchThread,
    handleSend,
    handleQueue,
    cancelQueued,
    queuedForActiveThread,
    handlePreviewContext,
    handleResend,
    handleExport,
    handleCreateThread,
    handleCloseThread,
    handleRenameThread,
    handleClearContext,
    handleCompact,
    handleClearThread,
    setThreadContextBlock,
    setThreadSourceFiles,
    setThreadUseTools,
    handleCancelStream,
    clearContextPreview,
    confirmContextPreview,
  } = useAIChat({turnModel, exportModelName, sendBlocked: guidance !== null})

  // Each tab's tooltip names its thread's model: the active thread's as
  // resolved above, the others' from their pins, else — a thread from before
  // per-thread memory — from their own loaded history (an unpinned one with
  // none starts on the Default for new chats).
  const conversations = useChatStore(s => s.conversations)
  const threadModelLabels = useMemo(() => {
    const out: Record<string, string> = {}
    for (const th of flowThreads) {
      const sel =
        th.id === activeThreadId
          ? selection
          : resolveThreadModel({
              pin: threadModelOf(th),
              messages: conversations.get(th.id),
              settings: selectionSettings,
              providers,
              listingLoaded,
            })
      if (!sel.label) continue
      const purpose = sel.model ? purposeOf(findModel(sel.model, listedModels(sel.provider))) : undefined
      out[th.id] = purpose ? t('threads.modelPurpose', {model: sel.label, purpose}) : sel.label
    }
    return out
  }, [
    flowThreads,
    activeThreadId,
    selection,
    conversations,
    selectionSettings,
    providers,
    listingLoaded,
    purposeOf,
    listedModels,
    t,
  ])

  const [suggestedPrompts, setSuggestedPrompts] = useState<string[]>([])
  const [msgSearch, setMsgSearch] = useState('')
  const [searchOpen, setSearchOpen] = useState(false)
  const [helpOpen, setHelpOpen] = useState(false)
  // Which match the user is standing on. The query is stored ALONGSIDE the
  // index so a new query resets to the first hit by derivation rather than by
  // an effect that would render one frame with a stale cursor. `nonce` makes
  // stepping onto the same index twice scroll again.
  const [matchCursor, setMatchCursor] = useState<{query: string; index: number; nonce: number}>({
    query: '',
    index: 0,
    nonce: 0,
  })
  const toggleSearch = useCallback(() => setSearchOpen(v => !v), [])
  const closeSearch = useCallback(() => {
    setSearchOpen(false)
    setMsgSearch('')
  }, [])
  const showHelp = useCallback(() => setHelpOpen(true), [])

  // Chat-scope shortcuts (mod+enter lives in ChatInput, next to the draft it
  // sends). containerRef keeps them to the chat panel: mod+l or Escape pressed
  // in the canvas or a findings comment box is not a request to touch the chat.
  // Enabled only while the panel is rendered: with no root mounted useKeyboard
  // has no container to check and would claim these keys app-wide.
  const chatRootRef = useRef<HTMLDivElement>(null)
  const {confirm} = useConfirm()
  const toast = useToast()
  // Every clear path — Ctrl+L, the /clear command — asks first, like the
  // header's Clear action: Ctrl+L is browser muscle memory (focus the address
  // bar), and /clear used to fire straight from an autocomplete keypress.
  const confirmClear = useCallback(() => {
    // Nothing to confirm while the thread is generating: the clear would be
    // refused anyway (handleClearThread), so the dialog used to ask and then
    // do nothing.
    if (isCurrentThreadStreaming) {
      toast.warning(t('errors.busyTitle'), {description: t('errors.clearWhileStreaming')})
      return
    }
    void confirm({
      title: t('toolbar.confirmClearTitle'),
      message: t('toolbar.confirmClear'),
      confirmLabel: t('toolbar.clear'),
      danger: true,
    }).then(ok => {
      if (ok) handleClearThread()
    })
  }, [confirm, t, handleClearThread, isCurrentThreadStreaming, toast])

  // Closing a thread drops it from the thread list for good (its history is
  // no longer reachable), so a thread with messages — or not yet loaded, or
  // still generating — asks first. An empty thread closes straight away.
  const confirmCloseThread = useCallback(
    (threadId: string) => {
      const st = useChatStore.getState()
      const msgs = st.conversations.get(threadId)
      // A draft counts: closeThread deletes it with the thread.
      const worthAsking = msgs === undefined || msgs.length > 0 || !!st.streams[threadId] || !!st.drafts[threadId]
      if (!worthAsking) {
        handleCloseThread(threadId)
        return
      }
      const title = st.threads.find(th => th.id === threadId)?.title || t('threads.untitled')
      void confirm({
        title: t('threads.confirmCloseTitle'),
        message: t('threads.confirmClose', {name: title}),
        confirmLabel: t('threads.close'),
        danger: true,
      }).then(ok => {
        if (ok) handleCloseThread(threadId)
      })
    },
    [confirm, t, handleCloseThread],
  )

  useKeyboard({
    scope: 'chat',
    containerRef: chatRootRef,
    enabled: configured === true,
    handlers: {
      'ai.clear.chat': () => {
        if (!readOnly) confirmClear()
      },
      'ai.cancel.stream': e => {
        // An open @/slash menu or the find bar handles Escape first. Next a
        // fix awaiting approval: it is declined and the reply itself goes on
        // (the card no longer takes focus while the user types, so its own
        // Escape handler is not in the path). A bare Escape stops the reply;
        // with nothing streaming it leaves focus mode. Escapes inside the
        // portaled dialogs (Compose editor, preview, confirm) never reach
        // here: useKeyboard requires the focus to be inside the chat root.
        if (e.defaultPrevented || isImeKeydown(e)) return
        if (isCurrentThreadStreaming) {
          e.preventDefault()
          const pendingCard = fixProposals.find(c => c.status === 'pending')
          if (pendingCard && activeThreadId) {
            respondFixProposal(activeThreadId, false, pendingCard.proposalId)
            return
          }
          handleCancelStream()
          return
        }
        if (chatPanel?.isFocused) {
          e.preventDefault()
          chatPanel.toggleFocus()
        }
      },
      'ai.focus': () => {
        if (chatPanel?.canFocus) chatPanel.toggleFocus()
      },
    },
  })

  // Text size cycles S → M → L from the header menu; Settings › Appearance
  // offers the same three.
  const cycleTextSize = useCallback(() => {
    const order: ChatTextSize[] = ['sm', 'md', 'lg']
    const next = order[(order.indexOf(chatTextSize) + 1) % order.length]
    void updateAppearance({chatTextSize: next})
  }, [chatTextSize, updateAppearance])

  const contextBlockId = activeThread?.contextBlockId ?? null

  const hasFindings = useMemo(() => {
    if (!contextBlockId || !_analysisReport) return false
    return _analysisReport.findings.some(f => f.blockId === contextBlockId)
  }, [contextBlockId, _analysisReport])

  useEffect(() => {
    let active = true
    chatApi
      .getSuggestedPrompts(!!selectedBlockId, hasFindings)
      .then((ps: string[] | null) => {
        if (active) setSuggestedPrompts(ps || [])
      })
      .catch(err => {
        logger.warn('Failed to load suggested prompts', err)
      })
    return () => {
      active = false
    }
  }, [selectedBlockId, hasFindings])

  // Search no longer FILTERS the conversation — it indexes it. Matches keep
  // their surrounding turns, and the search bar steps through them.
  const searchQuery = searchOpen ? msgSearch.trim() : ''
  const matchIndices = useMemo(() => {
    if (!searchQuery) return []
    const q = searchQuery.toLowerCase()
    const out: number[] = []
    activeThreadMessages.forEach((m, i) => {
      if (m.content.toLowerCase().includes(q)) out.push(i)
    })
    return out
  }, [activeThreadMessages, searchQuery])

  const stepMatch = useCallback(
    (delta: number) => {
      setMatchCursor(prev => {
        const count = matchIndices.length
        if (count === 0) return prev
        const from = prev.query === searchQuery ? prev.index : 0
        return {query: searchQuery, index: (from + delta + count) % count, nonce: prev.nonce + 1}
      })
    },
    [matchIndices.length, searchQuery],
  )
  const nextMatch = useCallback(() => stepMatch(1), [stepMatch])
  const prevMatch = useCallback(() => stepMatch(-1), [stepMatch])

  // A cursor belonging to an older query reads as 0, so a new search always
  // starts at its first hit without a reset pass.
  const rawCursor = matchCursor.query === searchQuery ? matchCursor.index : 0
  const safeCursor = matchIndices.length > 0 ? Math.min(rawCursor, matchIndices.length - 1) : 0
  const activeMatchIndex = matchIndices.length > 0 ? matchIndices[safeCursor] : -1
  const scrollTo = useMemo(
    () =>
      activeMatchIndex >= 0 && matchCursor.nonce > 0 ? {index: activeMatchIndex, nonce: matchCursor.nonce} : undefined,
    [activeMatchIndex, matchCursor.nonce],
  )

  // Identify the regeneratable turn by message ID, not by index. Comparing a
  // render index against a position in the FILTERED array attached Regenerate
  // to whichever message happened to sit last in the search results. Only
  // the thread's LAST message qualifies: an assistant message with a newer
  // user message after it is no turn Retry/Regenerate can act on (handleResend
  // bails), so it must not offer a dead button.
  const lastAssistantId = useMemo(() => {
    const last = activeThreadMessages[activeThreadMessages.length - 1]
    return last?.role === 'assistant' ? last.id : null
  }, [activeThreadMessages])

  const clearBlockScope = useCallback(() => setThreadContextBlock(null), [setThreadContextBlock])
  const onSelectPrompt = useCallback((text: string) => handleSend(text, [], false), [handleSend])
  const toggleTools = useCallback(
    () => setThreadUseTools(!(activeThread?.useTools ?? false)),
    [setThreadUseTools, activeThread],
  )

  const streamFooter = useMemo(
    () => (
      <>
        {showThinking && (
          <MessageBubble
            message={{id: 'thinking', role: 'assistant', content: '', timestamp: new Date().toISOString()}}
            isThinking
          />
        )}
        {isCurrentThreadStreaming && <StreamingBubble />}
        {/* Agentic activity rides INSIDE the scroll region. Pinned below it,
            the tool trail and approval cards grew row by row mid-stream and
            squeezed the conversation as the answer arrived. */}
        {isCurrentThreadStreaming && <LiveToolTrail />}
        {isCurrentThreadStreaming &&
          activeThreadId &&
          fixProposals.map(card => (
            <FixProposalCard
              key={card.proposalId}
              proposal={card}
              onRespond={(approved, proposalId, excludedItemIndices) =>
                respondFixProposal(activeThreadId, approved, proposalId, excludedItemIndices)
              }
            />
          ))}
      </>
    ),
    [showThinking, isCurrentThreadStreaming, fixProposals, activeThreadId, respondFixProposal],
  )

  // The notice row: the thread's own (removed / unavailable / refused), else
  // the one-time word about a saved Azure default that names nothing.
  const noticeView: ModelNoticeView | null =
    notice ?? (legacyDefaultNotice ? {kind: 'legacyDefault', ...legacyDefaultNotice} : null)
  // Announced through the status region below — the row is not a live
  // region — so a notice is read once, when it is raised (and only while
  // the row is on screen: not over the "add an API key" state).
  const noticeText =
    configured && activeThread && noticeView ? modelNoticeSentences(t, noticeView, providerName).join(' ') : ''
  // Rendered by EVERY branch below, at the same place, so it is mounted —
  // empty — from the panel's first render on: a live region inserted
  // already holding its text (a notice raised as the listing first loads) is
  // not announced.
  const statusRegion = (
    <div role="status" className="sr-only">
      {noticeText}
    </div>
  )

  if (configured === null) {
    // Still checking the providers — render nothing rather than flash the
    // "add an API key" screen; a failed check gets a way to retry.
    if (!providersError) {
      return (
        <>
          {statusRegion}
          <div className="flex-1 flex items-center justify-center">
            <Spinner />
          </div>
        </>
      )
    }
    return (
      <>
        {statusRegion}
        <div className="flex flex-col items-center justify-center h-full gap-3 p-6 text-center" role="alert">
          <AlertTriangle size={20} className="text-semantic-warning" />
          <div>
            <p className="text-sm font-medium text-text-primary">{t('empty.providersFailedTitle')}</p>
            <p className="text-xs text-text-tertiary mt-1">{t('empty.providersFailedBody')}</p>
          </div>
          <button
            onClick={retryProviders}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-surface-3 border border-border-default text-xs text-text-secondary hover:text-text-primary hover:bg-surface-4 transition-colors"
          >
            <RefreshCw size={12} />
            {t('empty.retry')}
          </button>
        </div>
      </>
    )
  }
  if (!configured) {
    return (
      <>
        {statusRegion}
        <ApiKeyMissingState />
      </>
    )
  }

  const selectedSourceFiles = activeThread?.selectedSourceFiles ?? []
  const messages = activeThreadMessages
  const historyLoading = historyState === 'loading' && messages.length === 0
  const showWelcome = messages.length === 0 && !isCurrentThreadStreaming && !historyLoading
  const threadPurpose = purposeOf(threadModelDetail)

  // Dismiss takes the row away with the button that has focus: focus moves
  // to the model picker's trigger, which names the model the thread now uses.
  const dismissModelNotice = () => {
    if (notice) dismissNotice()
    else dismissLegacyDefaultNotice()
    pickerTriggerRef.current?.focus()
  }

  const main = (
    // The thread-keyed boundary wraps only the scroll region below: keying
    // this outer one remounted the header, tab bar and composer on every
    // thread switch, which threw keyboard focus off the thread tabs.
    <ChatErrorBoundary>
      {/* chat-root establishes the container that every density rule below
          keys off. The panel is anywhere from 280px to most of the window by
          drag (or the whole window in focus mode), independent of the
          viewport, so viewport breakpoints cannot describe it. tabIndex: a
          click on message text lands focus in the chat, so the chat-scoped
          shortcuts (Escape, mod+l, mod+.) work after reading. */}
      <div
        ref={chatRootRef}
        tabIndex={-1}
        data-chat-text={chatTextSize}
        className="chat-root flex flex-col h-full min-h-0 outline-none"
      >
        <div className="flex-shrink-0">
          <ChatHeader
            providers={pickerProviders}
            provider={selection.provider}
            model={selection.model}
            modelLabel={selection.label}
            modelDetail={threadModelDetail}
            defaultModels={newChatDefaults}
            onPickModel={pickModel}
            orgName={orgName}
            reviewPrompt={!!azure?.suggestDiscovery}
            pickerDisabled={!activeThread}
            isStreaming={isCurrentThreadStreaming}
            messageCount={messages.length}
            useTools={activeThread?.useTools ?? false}
            onToggleTools={toggleTools}
            onNewChat={handleCreateThread}
            onClearContext={readOnly ? undefined : handleClearContext}
            onCompact={readOnly ? undefined : handleCompact}
            onExport={handleExport}
            onToggleSearch={toggleSearch}
            searchActive={searchOpen}
            onShowHelp={showHelp}
            isFocused={chatPanel?.isFocused}
            onToggleFocus={chatPanel?.canFocus ? chatPanel.toggleFocus : undefined}
            chatTextSize={chatTextSize}
            onCycleTextSize={cycleTextSize}
            pickerTriggerRef={pickerTriggerRef}
          />

          <ChatThreadBar
            threads={flowThreads}
            activeThreadId={activeThreadId}
            onSelect={switchThread}
            onCreate={handleCreateThread}
            onClose={confirmCloseThread}
            onRename={handleRenameThread}
            modelLabels={threadModelLabels}
          />

          {activeThread && noticeView && (
            <ModelUnavailableNotice
              notice={noticeView}
              providerName={providerName}
              onChooseModel={() => useChatStore.getState().openModelPicker()}
              onDismiss={dismissModelNotice}
              onManage={openAzureAccounts}
            />
          )}

          {activeThread && (doc || contextBlockId) && (
            <ChatContextBar
              contextBlockId={contextBlockId}
              blockName={selectedBlock?.name}
              blockType={selectedBlock?.rawType}
              onClearBlock={clearBlockScope}
              selectedBlockId={selectedBlockId}
              onFocusBlock={setThreadContextBlock}
              files={sourceFiles}
              selectedFiles={selectedSourceFiles}
              onFilesChange={setThreadSourceFiles}
            />
          )}

          {searchOpen && (
            <ChatSearchBar
              query={msgSearch}
              onChange={setMsgSearch}
              current={matchIndices.length > 0 ? safeCursor + 1 : 0}
              total={matchIndices.length}
              onPrev={prevMatch}
              onNext={nextMatch}
              onClose={closeSearch}
            />
          )}
        </div>

        {historyState === 'error' && (
          <div
            role="alert"
            className="flex items-center gap-2 chat-gutter py-1.5 border-b border-border-subtle bg-semantic-warning/5 text-2xs text-text-secondary"
          >
            <AlertTriangle size={12} className="text-semantic-warning shrink-0" />
            <span className="flex-1 min-w-0 truncate">{t('history.loadFailed')}</span>
            <button
              onClick={retryHistoryLoad}
              className="shrink-0 font-medium text-brand-400 hover:text-brand-300 transition-colors"
            >
              {t('history.retry')}
            </button>
          </div>
        )}

        {/* Scroll region — exactly one of welcome / messages / empty-state fills it
          and owns its own scrolling. Its boundary resets per thread. */}
        <ChatErrorBoundary key={activeThreadId ?? 'no-thread'}>
          <div className="flex-1 min-h-0 flex flex-col">
            {doc && activeThread && historyLoading ? (
              <div className="flex-1 flex items-center justify-center gap-2 text-xs text-text-tertiary" role="status">
                <Spinner size={14} />
                {t('history.loading')}
              </div>
            ) : doc && activeThread ? (
              showWelcome ? (
                <div className="flex-1 min-h-0 overflow-y-auto custom-scrollbar">
                  <div>
                    <div className="chat-gutter pt-4 pb-2">
                      {selection.label && (
                        <p className="text-xs font-medium text-text-secondary leading-relaxed [overflow-wrap:anywhere]">
                          {threadPurpose
                            ? t('welcome.modelPurpose', {model: selection.label, purpose: threadPurpose})
                            : t('welcome.model', {model: selection.label})}
                        </p>
                      )}
                      <p className="text-xs text-text-tertiary leading-relaxed">
                        {t(`welcome.${selection.provider}`, {defaultValue: t('welcome.fallback')})}
                      </p>
                    </div>
                    <SuggestedPrompts
                      prompts={suggestedPrompts}
                      onSelect={onSelectPrompt}
                      disabledReason={sendBlockedReason}
                    />
                    <PromptTemplates
                      onSelect={onSelectPrompt}
                      hasBlock={!!contextBlockId}
                      flowName={doc?.name}
                      blockName={selectedBlock?.name}
                      disabledReason={sendBlockedReason}
                    />
                  </div>
                </div>
              ) : (
                // key = thread id: switching threads remounts the virtualized
                // list so it opens at the new thread's bottom (initialTopMostItemIndex).
                <ChatMessageList
                  key={activeThread.id}
                  messages={activeThreadMessages}
                  renderMessage={(i, m) => (
                    <MessageBubble
                      message={m}
                      modelLabel={listedModelName(m)}
                      isLastAssistant={m.id === lastAssistantId}
                      onRegenerate={m.id === lastAssistantId ? handleResend : undefined}
                      // Retry resends the LAST exchange, so only the last
                      // reply may offer it: on an older error bubble it
                      // deleted the latest answer and resent the wrong question.
                      onRetry={m.finishReason === 'error' && m.id === lastAssistantId ? handleResend : undefined}
                      resendDisabledReason={sendBlockedReason}
                      highlight={searchQuery || undefined}
                      isActiveMatch={i === activeMatchIndex}
                    />
                  )}
                  footer={streamFooter}
                  isStreaming={isCurrentThreadStreaming}
                  scrollTo={scrollTo}
                />
              )
            ) : (
              <EmptyChatState hasDoc={!!doc} hasThread={!!activeThread} onCreateThread={handleCreateThread} />
            )}
          </div>
        </ChatErrorBoundary>

        {/* Pinned bottom — the composer, and nothing else. */}
        {doc && activeThread && (
          <div className="flex-shrink-0">
            <ChatInput
              onSend={handleSend}
              onPreview={handlePreviewContext}
              onCancel={handleCancelStream}
              onClearThread={readOnly || isCurrentThreadStreaming ? undefined : confirmClear}
              onShowHelp={showHelp}
              onQueue={handleQueue}
              queued={queuedForActiveThread ?? null}
              onCancelQueue={cancelQueued}
              sourceFiles={sourceFiles}
              promptTokens={activeThread.tokensIn ?? 0}
              completionTokens={activeThread.tokensOut ?? 0}
              contextLimit={threadModelDetail?.contextLimit || undefined}
              inputCostPerM={threadModelDetail?.inputCostPerM}
              outputCostPerM={threadModelDetail?.outputCostPerM}
              guidance={guidance}
            />
          </div>
        )}

        {helpOpen && (
          <Suspense fallback={null}>
            <ChatHelpPopover onClose={() => setHelpOpen(false)} />
          </Suspense>
        )}

        {contextPreview && pendingMessage && (
          <Suspense fallback={null}>
            <ContextPreviewModal
              preview={contextPreview}
              onClose={clearContextPreview}
              onConfirm={confirmContextPreview}
            />
          </Suspense>
        )}
      </div>
    </ChatErrorBoundary>
  )

  return (
    <>
      {statusRegion}
      {main}
    </>
  )
}
