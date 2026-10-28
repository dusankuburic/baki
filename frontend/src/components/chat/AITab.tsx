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
  TurnAnnouncer,
  ProvidersPendingState,
  ProvidersErrorState,
  ProvidersNotConfiguredState,
} from './aitab/StatusRegion'
import {SelectionContextBar} from './aitab/SelectionContextBar'
import {SelectionPromptTemplates} from './aitab/SelectionPromptTemplates'
import {answerAfter, answerAnnouncement} from './aitab/announce'
import {threadUsesTools} from './hooks/useChatRequestBuilder'
import {isAgentModel} from '@/lib/modelIds'

interface TurnWatch {
  threadId: string | null
  /** The stream whose answer is awaited (or was announced). */
  messageId?: string
  /** The thread's last message when that stream started: its answer is the first assistant message after it. */
  lastIdAtStart?: string
  answered: boolean
}

const ContextPreviewModal = lazy(() => import('./ContextPreviewModal'))
const ChatHelpPopover = lazy(() => import('./ChatHelpPopover'))

export default function AITab() {
  const {t} = useTranslation('chat')
  const hasSelectedBlock = useFlowStore(s => s.selectedBlockId != null)
  const chatTextSize = useSettingsStore(s => s.settings.appearance.chatTextSize)
  const toolsOffByDefault = useSettingsStore(s => s.settings.ai.toolsOffByDefault)
  const updateAppearance = useSettingsStore(s => s.updateAppearance)
  const chatPanel = useChatPanel()

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

  const {purposeOf, providerName, listedModels, pickerProviders, listedModelName, exportModelName} = useModelNames({
    providers,
  })

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
    queuePaused,
    resumeQueue,
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

  const chatRootRef = useRef<HTMLDivElement>(null)
  const {confirm} = useConfirm()
  const toast = useToast()
  const confirmClear = useCallback(() => {
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

  const confirmCloseThread = useCallback(
    (threadId: string) => {
      const st = useChatStore.getState()
      const msgs = st.conversations.get(threadId)
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
        confirmClear()
      },
      'ai.cancel.stream': e => {
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

  const lastAssistantId = useMemo(() => {
    const last = activeThreadMessages[activeThreadMessages.length - 1]
    return last?.role === 'assistant' ? last.id : null
  }, [activeThreadMessages])

  // R-frontend-5: the message list is not a live region, so the turn announcer reads each committed answer once. A
  // turn is watched from its stream's start; the first assistant message after the one that was last then is its
  // answer (or its error). The answer is looked for before a new stream is taken up, so a queued follow-up that
  // starts in the same update does not hide it; a new turn clears the line. History, scrolling, re-renders and
  // thread switches announce nothing.
  const streamingMessageId = useChatStore(s => (s.activeThreadId ? s.streams[s.activeThreadId]?.messageId : undefined))
  const lastMessageId = activeThreadMessages[activeThreadMessages.length - 1]?.id
  const [turnWatch, setTurnWatch] = useState<TurnWatch>({threadId: activeThreadId, answered: true})
  const [turnLine, setTurnLine] = useState({text: '', seq: 0})
  if (turnWatch.threadId !== activeThreadId) {
    // Only a reply already streaming on the thread switched to is awaited; its history is never announced.
    setTurnWatch({
      threadId: activeThreadId,
      messageId: streamingMessageId,
      lastIdAtStart: lastMessageId,
      answered: !streamingMessageId,
    })
    if (turnLine.text) setTurnLine({text: '', seq: turnLine.seq})
  } else {
    let watch = turnWatch
    let line: string | undefined
    const answer = watch.answered ? undefined : answerAfter(activeThreadMessages, watch.lastIdAtStart)
    if (answer) {
      watch = {...watch, answered: true, lastIdAtStart: answer.id}
      line = t('a11y.answerAnnouncement', {text: answerAnnouncement(answer.content)})
    }
    if (streamingMessageId && streamingMessageId !== watch.messageId) {
      watch = {threadId: activeThreadId, messageId: streamingMessageId, lastIdAtStart: lastMessageId, answered: false}
      line ??= ''
    }
    if (watch !== turnWatch) setTurnWatch(watch)
    if (line !== undefined && (line || turnLine.text)) setTurnLine({text: line, seq: turnLine.seq + 1})
  }

  const clearBlockScope = useCallback(() => setThreadContextBlock(null), [setThreadContextBlock])
  const usesTools = threadUsesTools(activeThread, {toolsOffByDefault})
  const isAgent = selection.provider === 'azure-foundry' && isAgentModel(selection.model, threadModelDetail)
  // A prompt that needs lookups (search_flow, get_subflow_blocks…) turns the thread's tools on, as stageFindingFix
  // does (R-frontend-6): in the store before engine.send reads it, put back if the send does not go ahead, and told
  // to the user when it does (the choice stays with the thread).
  const onSelectPrompt = useCallback(
    (text: string, needsTools: boolean) => {
      const thread = activeThread ? useChatStore.getState().threads.find(th => th.id === activeThread.id) : undefined
      const turnOn = needsTools && !!thread && !usesTools
      const before = thread?.useTools
      if (turnOn) useChatStore.getState().updateThread(thread.id, {useTools: true})
      const sent = handleSend(text, [], false)
      if (!turnOn) return
      if (!sent) {
        useChatStore.getState().updateThread(thread.id, {useTools: before})
        return
      }
      toast.info(t('composer.toolsTurnedOn'), {description: t('composer.toolsTurnedOnBody')})
    },
    [activeThread, handleSend, usesTools, toast, t],
  )
  const onSelectSuggestedPrompt = useCallback(
    // Suggested prompts are the server's strings and cannot be told apart: every one is a question about the flow.
    (text: string) => onSelectPrompt(text, true),
    [onSelectPrompt],
  )
  const toggleTools = useCallback(() => setThreadUseTools(!usesTools), [setThreadUseTools, usesTools])
  const enableTools = useCallback(() => setThreadUseTools(true), [setThreadUseTools])

  const renderMessage = useCallback(
    (i: number, m: ChatMessage) => (
      <MessageBubble
        message={m}
        modelLabel={listedModelName(m)}
        isLastAssistant={m.id === lastAssistantId}
        onRegenerate={m.id === lastAssistantId ? handleResend : undefined}
        onRetry={m.finishReason === 'error' && m.id === lastAssistantId ? handleResend : undefined}
        resendDisabledReason={sendBlockedReason}
        highlight={searchQuery || undefined}
        isActiveMatch={i === activeMatchIndex}
        onCompact={readOnly ? undefined : handleCompact}
      />
    ),
    [listedModelName, lastAssistantId, handleResend, sendBlockedReason, searchQuery, activeMatchIndex, readOnly, handleCompact],
  )

  const streamFooter = useStreamFooter({
    showThinking,
    isCurrentThreadStreaming,
    approvals,
    activeThreadId,
    respondFixProposal,
  })

  const noticeView: ModelNoticeView | null =
    notice ?? (legacyDefaultNotice ? {kind: 'legacyDefault', ...legacyDefaultNotice} : null)
  const noticeText =
    configured && activeThread && noticeView ? modelNoticeSentences(t, noticeView, providerName).join(' ') : ''
  const statusRegion = (
    <>
      <StatusRegion text={noticeText} />
      <TurnAnnouncer line={turnLine} />
    </>
  )

  if (configured === null) {
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

  const dismissModelNotice = () => {
    if (notice) dismissNotice()
    else dismissLegacyDefaultNotice()
    pickerTriggerRef.current?.focus()
  }

  const main = (
    <ChatErrorBoundary>
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
            useTools={usesTools}
            onToggleTools={toggleTools}
            onNewChat={handleCreateThread}
            onClearContext={handleClearContext}
            // Clear works for a viewer (a conversation is its owner's to delete), Compact does not: it stores the
            // kept turns with /api/chat/save, which still needs edit access to the flow.
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
              lostBlock={activeThread.contextBlockLost}
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
                      onSelect={onSelectSuggestedPrompt}
                      disabledReason={sendBlockedReason}
                    />
                    <SelectionPromptTemplates
                      onSelect={onSelectPrompt}
                      contextBlockId={contextBlockId}
                      flowName={doc?.name}
                      disabledReason={sendBlockedReason}
                    />
                  </div>
                </div>
              ) : (
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

        {doc && activeThread && (
          <div className="shrink-0">
            <ChatInput
              onSend={handleSend}
              onPreview={handlePreviewContext}
              onCancel={handleCancelStream}
              onClearThread={isCurrentThreadStreaming ? undefined : confirmClear}
              onShowHelp={showHelp}
              onQueue={handleQueue}
              queued={queuedForActiveThread ?? null}
              onCancelQueue={cancelQueued}
              queuePaused={queuePaused ?? null}
              onResumeQueue={resumeQueue}
              sourceFiles={sourceFiles}
              promptTokens={activeThread.tokensIn ?? 0}
              completionTokens={activeThread.tokensOut ?? 0}
              contextLimit={threadModelDetail?.contextLimit || undefined}
              guidance={guidance}
              onEnableTools={usesTools || isAgent ? undefined : enableTools}
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
