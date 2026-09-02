import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useProviderSetup} from './hooks/useProviderSetup'
import {useAIChat} from './hooks/useAIChat'
import {logger} from '@/lib/logger'
import {buildBlockLookup} from '@/lib/tree'
import {
  MessageBubble,
  ChatMessageList,
  SuggestedPrompts,
  ChatInput,
  ApiKeyMissingState,
  ContextChip,
  ConnectionPanel,
  TokenCounter,
  PromptTemplates,
  SourceFilePicker,
  ChatToolbar,
  ChatThreadBar,
  FixProposalCard,
} from '.'
import {useState, useEffect, useMemo, useCallback, lazy, Suspense} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {chatApi} from '@/api'
import StreamingProgress from './StreamingProgress'
import LiveToolTrail from './LiveToolTrail'
import StreamingBubble from './StreamingBubble'
import type {ProviderID} from '@/types'
import ConnectionStatus from './ConnectionStatus'
import EmptyChatState from './EmptyChatState'
import ChatErrorBoundary from './ChatErrorBoundary'
import ChatSearchBar from './ChatSearchBar'

// Rarely-rendered overlays load on first open: the context-preview modal only
// appears when the user confirms a context-heavy send, and the help popover
// only on explicit help clicks. Keeping them out of the AITab chunk shaves
// its initial fetch.
const ContextPreviewModal = lazy(() => import('./ContextPreviewModal'))
const ChatHelpPopover = lazy(() => import('./ChatHelpPopover'))

const WELCOME_MESSAGES: Record<string, string> = {
  copilot: 'GitHub Copilot is ready — ask about your PAD flow, request code, or analyze findings.',
  claude: 'Claude is ready to help you analyze and debug your PAD flow.',
  openai: 'GPT is ready — ask questions about your flow or request analysis.',
  gemini: 'Gemini is ready to help you explore your PAD flow.',
  demo: 'Demo mode — try out AI analysis with limited daily requests.',
}
const DEFAULT_WELCOME = 'AI assistant is ready — ask about your flow, request analysis, or explore findings.'

export default function AITab() {
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
  const aiSettings = useSettingsStore(s => s.settings.ai)
  const provider = useChatStore(s => s.selectedProvider)

  const {
    configured,
    providers,
    selectedModel,
    setSelectedModel,
    demoRemaining,
    handleSetProvider,
    currentModels,
    currentModelDetail,
  } = useProviderSetup()

  const {
    doc,
    selectedBlockId,
    activeThreadId,
    activeThread,
    activeThreadMessages,
    flowThreads,
    isCurrentThreadStreaming,
    showThinking,
    sourceFiles,
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
  } = useAIChat({selectedModel})

  const [suggestedPrompts, setSuggestedPrompts] = useState<string[]>([])
  const [msgSearch, setMsgSearch] = useState('')
  const [searchOpen, setSearchOpen] = useState(false)
  const [helpOpen, setHelpOpen] = useState(false)
  const toggleSearch = useCallback(() => setSearchOpen(v => !v), [])
  const closeSearch = useCallback(() => {
    setSearchOpen(false)
    setMsgSearch('')
  }, [])

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

  const displayedMessages = useMemo(() => {
    if (!searchOpen || !msgSearch.trim()) return activeThreadMessages
    const q = msgSearch.toLowerCase()
    return activeThreadMessages.filter(m => m.content.toLowerCase().includes(q))
  }, [activeThreadMessages, searchOpen, msgSearch])

  if (!configured) {
    return <ApiKeyMissingState />
  }

  const selectedSourceFiles = activeThread?.selectedSourceFiles ?? []
  const totalTokensIn = activeThread?.tokensIn ?? 0
  const totalTokensOut = activeThread?.tokensOut ?? 0

  const configuredProviders = providers.filter(p => p.configured || p.id === 'demo')
  const showCost = aiSettings.showCostEstimates && currentModelDetail && currentModelDetail.inputCostPerM > 0
  const messages = activeThreadMessages
  const lastAssistantIdx = displayedMessages.reduce((acc, m, i) => (m.role === 'assistant' ? i : acc), -1)
  const showWelcome = messages.length === 0 && !isCurrentThreadStreaming

  return (
    <ChatErrorBoundary key={activeThreadId ?? 'no-thread'}>
      <div className="flex flex-col h-full min-h-0">
        {/* Pinned header — provider/model selector, connection status, thread tabs.
          flex-shrink-0 keeps it anchored while only the message list scrolls. */}
        <div className="flex-shrink-0 border-b border-border-subtle">
          <ConnectionPanel
            providers={configuredProviders.map(p => ({
              id: p.id as ProviderID,
              name: p.name,
              configured: p.configured,
              authType: p.authType,
            }))}
            selectedProvider={provider}
            onSelectProvider={handleSetProvider}
            models={currentModels}
            selectedModel={selectedModel}
            onSelectModel={setSelectedModel}
            demoRemaining={demoRemaining}
            onExport={handleExport}
            hasMessages={messages.length > 0}
          />

          <div className="px-3 py-1.5">
            <ConnectionStatus isStreaming={isCurrentThreadStreaming} provider={currentModelDetail?.displayName} />
          </div>

          <ChatThreadBar
            threads={flowThreads}
            activeThreadId={activeThreadId}
            onSelect={switchThread}
            onCreate={handleCreateThread}
            onClose={handleCloseThread}
            onRename={handleRenameThread}
          />
        </div>

        {/* Pinned sub-controls — context scope, source files, toolbar. */}
        {(contextBlockId || doc) && activeThread && (
          <div className="flex-shrink-0 mt-1">
            {contextBlockId && selectedBlock ? (
              <ContextChip
                blockId={contextBlockId}
                blockName={selectedBlock.name}
                blockType={selectedBlock.rawType}
                onClear={() => setThreadContextBlock(null)}
              />
            ) : doc && selectedBlockId ? (
              <div className="px-3 py-1.5">
                <span className="text-2xs text-text-tertiary">
                  Scope: entire flow
                  <button
                    className="ml-2 text-brand-400 hover:text-brand-300 transition-colors"
                    onClick={() => setThreadContextBlock(selectedBlockId)}
                  >
                    Focus on selected block
                  </button>
                </span>
              </div>
            ) : null}
          </div>
        )}

        {sourceFiles.length > 0 && activeThread && (
          <div className="flex-shrink-0 mt-1">
            <SourceFilePicker
              files={sourceFiles}
              selected={selectedSourceFiles}
              onSelectionChange={setThreadSourceFiles}
            />
          </div>
        )}

        <div className="flex-shrink-0">
          <ChatToolbar
            messageCount={messages.length}
            onNewChat={handleCreateThread}
            onClearContext={handleClearContext}
            onCompact={handleCompact}
            useTools={activeThread?.useTools ?? false}
            providerId={provider}
            onToggleTools={provider === 'demo' ? undefined : () => setThreadUseTools(!(activeThread?.useTools ?? false))}
            onToggleSearch={toggleSearch}
            searchActive={searchOpen}
          />
          {searchOpen && (
            <ChatSearchBar
              query={msgSearch}
              onChange={setMsgSearch}
              matchCount={displayedMessages.length}
              onClose={closeSearch}
            />
          )}
        </div>

        {/* Scroll region — exactly one of welcome / messages / empty-state fills it
          and owns its own scrolling. */}
        <div className="flex-1 min-h-0 flex flex-col">
          {doc && activeThread ? (
            showWelcome ? (
              <div className="flex-1 min-h-0 overflow-y-auto custom-scrollbar">
                <div className="px-4 pt-4 pb-2">
                  <p className="text-xs text-text-tertiary leading-relaxed">
                    {WELCOME_MESSAGES[useChatStore.getState().selectedProvider] ?? DEFAULT_WELCOME}
                  </p>
                </div>
                <SuggestedPrompts prompts={suggestedPrompts} onSelect={text => handleSend(text, [], false)} />
                <PromptTemplates
                  onSelect={text => handleSend(text, [], false)}
                  hasBlock={!!contextBlockId}
                  flowName={doc?.name}
                  blockName={selectedBlock?.name}
                />
              </div>
            ) : (
              // key = thread id: switching threads remounts the virtualized
              // list so it opens at the new thread's bottom (initialTopMostItemIndex).
              <ChatMessageList
                key={activeThread.id}
                messages={displayedMessages}
                renderMessage={(i, m) => (
                  <MessageBubble
                    message={m}
                    isLastAssistant={i === lastAssistantIdx}
                    onRegenerate={i === lastAssistantIdx ? handleResend : undefined}
                    onRetry={m.finishReason === 'error' ? handleResend : undefined}
                  />
                )}
                footer={
                  <>
                    {showThinking && (
                      <MessageBubble
                        message={{id: 'thinking', role: 'assistant', content: '', timestamp: new Date().toISOString()}}
                        isThinking
                      />
                    )}
                    {isCurrentThreadStreaming && <StreamingBubble />}
                  </>
                }
                isStreaming={isCurrentThreadStreaming}
              />
            )
          ) : (
            <EmptyChatState hasDoc={!!doc} hasThread={!!activeThread} onCreateThread={handleCreateThread} />
          )}
        </div>

        {/* Pinned bottom — tokens/progress + input. */}
        {doc && activeThread && (
          <div className="flex-shrink-0">
            {isCurrentThreadStreaming && <LiveToolTrail />}
            {isCurrentThreadStreaming &&
              fixProposals.map(card => (
                <FixProposalCard
                  key={card.proposalId}
                  proposal={card}
                  onRespond={(approved, proposalId, excludedItemIndices) =>
                    respondFixProposal(activeThreadId!, approved, proposalId, excludedItemIndices)
                  }
                />
              ))}
            {isCurrentThreadStreaming ? (
              <StreamingProgress />
            ) : (
              <TokenCounter
                promptTokens={totalTokensIn}
                completionTokens={totalTokensOut}
                inputCostPerM={showCost ? currentModelDetail?.inputCostPerM : undefined}
                outputCostPerM={showCost ? currentModelDetail?.outputCostPerM : undefined}
              />
            )}

            <ChatInput
              onSend={(text, files, excludeContext) => handleSend(text, files, excludeContext)}
              onPreview={(text, files, excludeContext) => handlePreviewContext(text, files, excludeContext)}
              onCancel={handleCancelStream}
              onFilesChange={setThreadSourceFiles}
              onClearThread={handleClearThread}
              onShowHelp={() => setHelpOpen(true)}
              onQueue={(text, files, excludeContext) => handleQueue(text, files, excludeContext)}
              queued={queuedForActiveThread ?? null}
              onCancelQueue={cancelQueued}
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
}
