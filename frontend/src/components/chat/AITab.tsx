import {useTranslation} from 'react-i18next'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useProviderSetup} from './hooks/useProviderSetup'
import {useAIChat} from './hooks/useAIChat'
import {MessageBubble, ChatMessageList, SuggestedPrompts, ChatInput, ChatThreadBar} from '.'
import {useState, useMemo, useCallback, useRef, lazy, Suspense} from 'react'
import {AlertTriangle} from 'lucide-react'
import {useKeyboard} from '@/hooks/useKeyboard'
import {useConfirm, useToast, Spinner} from '@/components/shared'
import {isImeKeydown} from '@/lib/imeKey'
import {useChatStore} from '@/stores/chatStore'
import type {ChatMessage, ChatTextSize} from '@/types'
import ChatHeader from './ChatHeader'
import EmptyChatState from './EmptyChatState'
import ChatErrorBoundary from './ChatErrorBoundary'
import ChatSearchBar from './ChatSearchBar'
import ModelUnavailableNotice, {modelNoticeSentences, type ModelNoticeView} from './ModelUnavailableNotice'
import {useChatPanel} from './ChatPanelShell'
import {openAzureAccounts, useModelNames} from './aitab/useModelNames'
import {useThreadModelLabels} from './aitab/useThreadModelLabels'
import {useSuggestedPrompts} from './aitab/useSuggestedPrompts'
import {useMessageSearch} from './aitab/useMessageSearch'
import {useStreamFooter} from './aitab/useStreamFooter'
import {
  StatusRegion,
  ProvidersPendingState,
  ProvidersErrorState,
  ProvidersNotConfiguredState,
} from './aitab/StatusRegion'
import {SelectionContextBar} from './aitab/SelectionContextBar'
import {SelectionPromptTemplates} from './aitab/SelectionPromptTemplates'

// Rarely-rendered overlays load on first open: the context-preview modal only
// appears when the user confirms a context-heavy send, and the help popover
// only on explicit help clicks. Keeping them out of the AITab chunk shaves
// its initial fetch.
const ContextPreviewModal = lazy(() => import('./ContextPreviewModal'))
const ChatHelpPopover = lazy(() => import('./ChatHelpPopover'))

// Welcome copy is keyed by provider id under chat:welcome.*, with a fallback
// for providers that have no dedicated line; above it, the thread's model and
// what it is for.

export default function AITab() {
  const {t} = useTranslation('chat')
  // The canvas selection is NOT subscribed here: the controls that show the
  // selected block (SelectionContextBar, SelectionPromptTemplates below)
  // follow it on their own, so block navigation in the editor leaves this
  // panel — and the memoised message list — alone. The suggested prompts only
  // care whether a block is selected at all.
  const hasSelectedBlock = useFlowStore(s => s.selectedBlockId != null)
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

  // Model names: the header picker's list, a message's display name, the
  // purpose labels and the export naming.
  const {purposeOf, providerName, listedModels, pickerProviders, listedModelName, exportModelName} = useModelNames({
    providers,
  })

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
  }, [listingLoaded, selection.provider, selection.model, azure, t])
  const sendBlockedReason = guidance?.text

  // The model picker's trigger: focus goes back to it when a notice's
  // Dismiss takes the row (and the focused button) away.
  const pickerTriggerRef = useRef<HTMLButtonElement | null>(null)

  const {
    doc,
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
    approvals,
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

  const threadModelLabels = useThreadModelLabels({
    flowThreads,
    activeThreadId,
    selection,
    selectionSettings,
    providers,
    listingLoaded,
    purposeOf,
    listedModels,
  })

  const [helpOpen, setHelpOpen] = useState(false)
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
        // fix awaiting approval: the newest pending card (the one in view, as
        // its own Escape would take) is declined and the reply itself goes on
        // (the card no longer takes focus while the user types, so its own
        // Escape handler is not in the path). A bare Escape stops the reply;
        // with nothing streaming it leaves focus mode. Escapes inside the
        // portaled dialogs (Compose editor, preview, confirm) never reach
        // here: useKeyboard requires the focus to be inside the chat root.
        if (e.defaultPrevented || isImeKeydown(e)) return
        if (isCurrentThreadStreaming) {
          e.preventDefault()
          const pendingCard = [...approvals].reverse().find(c => c.status === 'pending')
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

  const analysisReport = useAnalysisStore(s => (doc ? s.reports.get(doc.id) : undefined))
  const hasFindings = useMemo(() => {
    if (!contextBlockId || !analysisReport) return false
    return analysisReport.findings.some(f => f.blockId === contextBlockId)
  }, [contextBlockId, analysisReport])

  const suggestedPrompts = useSuggestedPrompts(hasSelectedBlock, hasFindings)

  const {
    msgSearch,
    setMsgSearch,
    searchOpen,
    toggleSearch,
    closeSearch,
    searchQuery,
    matchIndices,
    nextMatch,
    prevMatch,
    safeCursor,
    activeMatchIndex,
    scrollTo,
  } = useMessageSearch(activeThreadMessages)

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

  // Stable across renders that change none of its inputs, so the memoised
  // list (ChatMessageList) skips them — a fresh arrow here re-rendered every
  // visible bubble on each canvas selection and thinking-state flip.
  const renderMessage = useCallback(
    (i: number, m: ChatMessage) => (
      <MessageBubble
        message={m}
        modelLabel={listedModelName(m)}
        isLastAssistant={m.id === lastAssistantId}
        onRegenerate={m.id === lastAssistantId ? handleResend : undefined}
        // Retry resends the LAST exchange, so only the last reply may offer
        // it: on an older error bubble it deleted the latest answer and
        // resent the wrong question.
        onRetry={m.finishReason === 'error' && m.id === lastAssistantId ? handleResend : undefined}
        resendDisabledReason={sendBlockedReason}
        highlight={searchQuery || undefined}
        isActiveMatch={i === activeMatchIndex}
      />
    ),
    [listedModelName, lastAssistantId, handleResend, sendBlockedReason, searchQuery, activeMatchIndex],
  )

  const streamFooter = useStreamFooter({
    showThinking,
    isCurrentThreadStreaming,
    approvals,
    activeThreadId,
    respondFixProposal,
  })

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
  const statusRegion = <StatusRegion text={noticeText} />

  if (configured === null) {
    // Still checking the providers — render nothing rather than flash the
    // "add an API key" screen; a failed check gets a way to retry.
    if (!providersError) {
      return (
        <>
          {statusRegion}
          <ProvidersPendingState />
        </>
      )
    }
    return (
      <>
        {statusRegion}
        <ProvidersErrorState onRetry={retryProviders} />
      </>
    )
  }
  if (!configured) {
    return (
      <>
        {statusRegion}
        <ProvidersNotConfiguredState />
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
        className="chat-root flex flex-col h-full min-h-0 outline-hidden"
      >
        <div className="shrink-0">
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
            <SelectionContextBar
              contextBlockId={contextBlockId}
              onClearBlock={clearBlockScope}
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
                        <p className="text-xs font-medium text-text-secondary leading-relaxed wrap-anywhere">
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
                    <SelectionPromptTemplates
                      onSelect={onSelectPrompt}
                      hasBlock={!!contextBlockId}
                      flowName={doc?.name}
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
                  renderMessage={renderMessage}
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
          <div className="shrink-0">
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
