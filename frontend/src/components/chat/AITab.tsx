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

const ContextPreviewModal = lazy(() => import('./ContextPreviewModal'))
const ChatHelpPopover = lazy(() => import('./ChatHelpPopover'))

export default function AITab() {
  const {t} = useTranslation('chat')
  const hasSelectedBlock = useFlowStore(s => s.selectedBlockId != null)
  const chatTextSize = useSettingsStore(s => s.settings.appearance.chatTextSize)
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
        if (!readOnly) confirmClear()
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

  const clearBlockScope = useCallback(() => setThreadContextBlock(null), [setThreadContextBlock])
  const onSelectPrompt = useCallback((text: string) => handleSend(text, [], false), [handleSend])
  const toggleTools = useCallback(
    () => setThreadUseTools(!(activeThread?.useTools ?? false)),
    [setThreadUseTools, activeThread],
  )

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
  const statusRegion = <StatusRegion text={noticeText} />

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
              onClearThread={readOnly || isCurrentThreadStreaming ? undefined : confirmClear}
              onShowHelp={showHelp}
              onQueue={handleQueue}
              queued={queuedForActiveThread ?? null}
              onCancelQueue={cancelQueued}
              sourceFiles={sourceFiles}
              promptTokens={activeThread.tokensIn ?? 0}
              completionTokens={activeThread.tokensOut ?? 0}
              contextLimit={threadModelDetail?.contextLimit || undefined}
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
