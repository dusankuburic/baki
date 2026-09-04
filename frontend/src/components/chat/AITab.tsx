import {useTranslation} from 'react-i18next'
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
  PromptTemplates,
  ChatThreadBar,
  FixProposalCard,
} from '.'
import {useState, useEffect, useMemo, useCallback, lazy, Suspense} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {chatApi} from '@/api'
import LiveToolTrail from './LiveToolTrail'
import StreamingBubble from './StreamingBubble'
import type {ProviderID} from '@/types'
import ChatHeader from './ChatHeader'
import ChatContextBar from './ChatContextBar'
import EmptyChatState from './EmptyChatState'
import ChatErrorBoundary from './ChatErrorBoundary'
import ChatSearchBar from './ChatSearchBar'
import {useChatPanel} from './ResizableChatPanel'

// Rarely-rendered overlays load on first open: the context-preview modal only
// appears when the user confirms a context-heavy send, and the help popover
// only on explicit help clicks. Keeping them out of the AITab chunk shaves
// its initial fetch.
const ContextPreviewModal = lazy(() => import('./ContextPreviewModal'))
const ChatHelpPopover = lazy(() => import('./ChatHelpPopover'))

// Welcome copy is keyed by provider id under chat:welcome.*, with a fallback
// for providers that have no dedicated line.

export default function AITab() {
  const {t} = useTranslation('chat')
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
  const chatPanel = useChatPanel()

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
  // to whichever message happened to sit last in the search results.
  const lastAssistantId = useMemo(() => {
    for (let i = activeThreadMessages.length - 1; i >= 0; i--) {
      if (activeThreadMessages[i].role === 'assistant') return activeThreadMessages[i].id
    }
    return null
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

  if (!configured) {
    return <ApiKeyMissingState />
  }

  const selectedSourceFiles = activeThread?.selectedSourceFiles ?? []
  const configuredProviders = providers.filter(p => p.configured || p.id === 'demo')
  const showCost = aiSettings.showCostEstimates && currentModelDetail && currentModelDetail.inputCostPerM > 0
  const messages = activeThreadMessages
  const showWelcome = messages.length === 0 && !isCurrentThreadStreaming

  return (
    <ChatErrorBoundary key={activeThreadId ?? 'no-thread'}>
      {/* chat-root establishes the container that every density rule below
          keys off. The panel is 280-560px wide by drag, independent of the
          viewport, so viewport breakpoints cannot describe it. */}
      <div className="chat-root flex flex-col h-full min-h-0">
        <div className="flex-shrink-0">
          <ChatHeader
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
            isStreaming={isCurrentThreadStreaming}
            messageCount={messages.length}
            useTools={activeThread?.useTools ?? false}
            onToggleTools={provider === 'demo' ? undefined : toggleTools}
            onNewChat={handleCreateThread}
            onClearContext={handleClearContext}
            onCompact={handleCompact}
            onExport={handleExport}
            onToggleSearch={toggleSearch}
            searchActive={searchOpen}
            onShowHelp={showHelp}
            isPoppedOut={chatPanel?.isPoppedOut}
            onTogglePopOut={chatPanel?.togglePopOut}
          />

          <ChatThreadBar
            threads={flowThreads}
            activeThreadId={activeThreadId}
            onSelect={switchThread}
            onCreate={handleCreateThread}
            onClose={handleCloseThread}
            onRename={handleRenameThread}
          />

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

        {/* Scroll region — exactly one of welcome / messages / empty-state fills it
          and owns its own scrolling. */}
        <div className="flex-1 min-h-0 flex flex-col">
          {doc && activeThread ? (
            showWelcome ? (
              <div className="flex-1 min-h-0 overflow-y-auto custom-scrollbar">
                <div className="px-4 pt-4 pb-2">
                  <p className="text-xs text-text-tertiary leading-relaxed">
                    {t(`welcome.${provider}`, {defaultValue: t('welcome.fallback')})}
                  </p>
                </div>
                <SuggestedPrompts prompts={suggestedPrompts} onSelect={onSelectPrompt} />
                <PromptTemplates
                  onSelect={onSelectPrompt}
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
                messages={activeThreadMessages}
                renderMessage={(i, m) => (
                  <MessageBubble
                    message={m}
                    isLastAssistant={m.id === lastAssistantId}
                    onRegenerate={m.id === lastAssistantId ? handleResend : undefined}
                    onRetry={m.finishReason === 'error' ? handleResend : undefined}
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

        {/* Pinned bottom — the composer, and nothing else. */}
        {doc && activeThread && (
          <div className="flex-shrink-0">
            <ChatInput
              onSend={handleSend}
              onPreview={handlePreviewContext}
              onCancel={handleCancelStream}
              onClearThread={handleClearThread}
              onShowHelp={showHelp}
              onQueue={handleQueue}
              queued={queuedForActiveThread ?? null}
              onCancelQueue={cancelQueued}
              sourceFiles={sourceFiles}
              promptTokens={activeThread.tokensIn ?? 0}
              completionTokens={activeThread.tokensOut ?? 0}
              inputCostPerM={showCost ? currentModelDetail?.inputCostPerM : undefined}
              outputCostPerM={showCost ? currentModelDetail?.outputCostPerM : undefined}
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
