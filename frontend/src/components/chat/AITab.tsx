import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useProviderSetup} from './hooks/useProviderSetup'
import {useAIChat} from './hooks/useAIChat'
import {logger} from '@/lib/logger'
import {
  MessageBubble, ChatMessageList, SuggestedPrompts, ChatInput,
  ApiKeyMissingState, ContextChip, ConnectionPanel, TokenCounter,
  ContextPreviewModal, PromptTemplates, SourceFilePicker,
  ChatToolbar, ChatThreadBar,
} from '.'
import {useState, useEffect, useMemo, useCallback} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {chatApi} from '@/api'
import StreamingProgress from './StreamingProgress'
import StreamingBubble from './StreamingBubble'
import type {ProviderID} from '@/types'
import ConnectionStatus from './ConnectionStatus'
import EmptyChatState from './EmptyChatState'
import ChatErrorBoundary from './ChatErrorBoundary'
import ChatSearchBar from './ChatSearchBar'

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
  const _analysisReport = useAnalysisStore(s => _document ? s.reports.get(_document.id) : undefined)
  const selectedBlock = useMemo(() => {
    if (!_document || !_selectedBlockId) return null
    const stack = [..._document.subflows.flatMap(sf => sf.blocks)]
    while (stack.length) {
      const b = stack.pop()!
      if (b.id === _selectedBlockId) return b
      if (b.children?.length) stack.push(...b.children)
    }
    return null
  }, [_document, _selectedBlockId])
  const aiSettings = useSettingsStore(s => s.settings.ai)
  const provider = useChatStore(s => s.selectedProvider)

  const {
    configured, providers, selectedModel, setSelectedModel,
    demoRemaining, handleSetProvider, currentModels, currentModelDetail,
  } = useProviderSetup()

  const {
    doc, selectedBlockId, activeThreadId, activeThread, activeThreadMessages,
    flowThreads, isCurrentThreadStreaming, showThinking,
    streamingTokens,
    sourceFiles, contextPreview, pendingMessage, toolStatus,
    switchThread,
    handleSend, handlePreviewContext, handleResend, handleExport,
    handleCreateThread, handleCloseThread, handleRenameThread,
    handleClearContext, handleCompact,
    setThreadContextBlock, setThreadSourceFiles, setThreadUseTools,
    handleCancelStream,
    clearContextPreview, confirmContextPreview,
  } = useAIChat({selectedModel})

  const [suggestedPrompts, setSuggestedPrompts] = useState<string[]>([])
  const [msgSearch, setMsgSearch] = useState('')
  const [searchOpen, setSearchOpen] = useState(false)
  const toggleSearch = useCallback(() => setSearchOpen(v => !v), [])
  const closeSearch = useCallback(() => { setSearchOpen(false); setMsgSearch('') }, [])

  const contextBlockId = activeThread?.contextBlockId ?? null

  const hasFindings = useMemo(() => {
    if (!contextBlockId || !_analysisReport) return false
    return _analysisReport.findings.some(f => f.blockId === contextBlockId)
  }, [contextBlockId, _analysisReport])

  useEffect(() => {
    let active = true
    chatApi.getSuggestedPrompts(!!selectedBlockId, hasFindings).then((ps: string[] | null) => {
      if (active) setSuggestedPrompts(ps || [])
    }).catch((err) => { logger.warn('Failed to load suggested prompts', err) })
    return () => { active = false }
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
  const lastAssistantIdx = displayedMessages.reduce((acc, m, i) => m.role === 'assistant' ? i : acc, -1)
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
          <ConnectionStatus
            isStreaming={isCurrentThreadStreaming}
            provider={currentModelDetail?.displayName}
          />
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
          onToggleTools={() => setThreadUseTools(!(activeThread?.useTools ?? false))}
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
              <SuggestedPrompts prompts={suggestedPrompts} onSelect={(text) => handleSend(text, [], false)} />
              <PromptTemplates
                onSelect={(text) => handleSend(text, [], false)}
                hasBlock={!!contextBlockId}
                flowName={doc?.name}
                blockName={selectedBlock?.name}
              />
            </div>
          ) : (
            <ChatMessageList isStreaming={isCurrentThreadStreaming}>
              {displayedMessages.map((m, i) => (
                <MessageBubble
                  key={m.id}
                  message={m}
                  isLastAssistant={i === lastAssistantIdx}
                  onRegenerate={i === lastAssistantIdx ? handleResend : undefined}
                  onRetry={m.finishReason === 'error' ? handleResend : undefined}
                />
              ))}
              {showThinking && (
                <MessageBubble
                  message={{id: 'thinking', role: 'assistant', content: '', timestamp: new Date().toISOString()}}
                  isThinking
                />
              )}
              {isCurrentThreadStreaming && <StreamingBubble />}
            </ChatMessageList>
          )
        ) : (
          <EmptyChatState hasDoc={!!doc} hasThread={!!activeThread} onCreateThread={handleCreateThread} />
        )}
      </div>

      {/* Pinned bottom — tokens/progress + input. */}
      {doc && activeThread && (
        <div className="flex-shrink-0">
          {isCurrentThreadStreaming && toolStatus && (
            <div className="flex items-center gap-2 px-3 py-1.5 text-2xs text-brand-400">
              <span className="inline-block w-1.5 h-1.5 rounded-full bg-brand-400 animate-pulse" />
              <span className="truncate">{toolStatus}…</span>
            </div>
          )}
          {isCurrentThreadStreaming && streamingTokens > 0 ? (
            <StreamingProgress
              tokens={streamingTokens}
              isStreaming={isCurrentThreadStreaming}
              estimatedTokens={undefined}
            />
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
          />
        </div>
      )}

      {contextPreview && pendingMessage && (
        <ContextPreviewModal
          preview={contextPreview}
          onClose={clearContextPreview}
          onConfirm={confirmContextPreview}
        />
      )}
    </div>
    </ChatErrorBoundary>
  )
}
