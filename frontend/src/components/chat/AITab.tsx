import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useProviderSetup} from './hooks/useProviderSetup'
import {useAIChat} from './hooks/useAIChat'
import {
  MessageBubble, ChatMessageList, SuggestedPrompts, ChatInput,
  ApiKeyMissingState, ContextChip, ConnectionPanel, TokenCounter,
  ContextPreviewModal, PromptTemplates, SourceFilePicker,
  ChatToolbar, ChatThreadBar,
} from '.'
import {useState, useEffect, useMemo} from 'react'
import {chatApi} from '@/api'

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

  const {
    configured, providers, selectedModel, setSelectedModel,
    demoRemaining, handleSetProvider, currentModels, currentModelDetail,
  } = useProviderSetup()

  const {
    doc, selectedBlockId, activeThreadId, activeThread, activeThreadMessages,
    flowThreads, isCurrentThreadStreaming, showThinking,
    streamingText, streamingMessageId, streamingTokens,
    sourceFiles, contextPreview, pendingMessage,
    switchThread,
    handleSend, handlePreviewContext, handleResend, handleExport,
    handleCreateThread, handleCloseThread, handleRenameThread,
    handleClearContext, handleCompact,
    setThreadContextBlock, setThreadSourceFiles,
    handleCancelStream,
    clearContextPreview, confirmContextPreview,
  } = useAIChat({selectedModel})

  const [suggestedPrompts, setSuggestedPrompts] = useState<string[]>([])

  const hasFindings = useMemo(() => {
    if (!activeThread?.contextBlockId || !_analysisReport) return false
    return _analysisReport.findings.some(f => f.blockId === activeThread.contextBlockId)
  }, [activeThread?.contextBlockId, _analysisReport])

  useEffect(() => {
    chatApi.getSuggestedPrompts(!!selectedBlockId, hasFindings).then((ps: any) => {
      setSuggestedPrompts(ps || [])
    }).catch(() => {})
  }, [selectedBlockId, hasFindings])

  if (!configured) {
    return <ApiKeyMissingState />
  }

  const contextBlockId = activeThread?.contextBlockId ?? null
  const selectedSourceFiles = activeThread?.selectedSourceFiles ?? []
  const totalTokensIn = activeThread?.tokensIn ?? 0
  const totalTokensOut = activeThread?.tokensOut ?? 0

  const configuredProviders = providers.filter(p => p.configured || p.id === 'demo')
  const showCost = aiSettings.showCostEstimates && currentModelDetail && currentModelDetail.inputCostPerM > 0
  const messages = activeThreadMessages
  const lastAssistantIdx = messages.reduce((acc, m, i) => m.role === 'assistant' ? i : acc, -1)
  const showWelcome = messages.length === 0 && !isCurrentThreadStreaming

  return (
    <div className="flex flex-col h-full">
      <ConnectionPanel
        providers={configuredProviders.map(p => ({
          id: p.id as any,
          name: p.name,
          configured: p.configured,
          authType: p.authType,
        }))}
        selectedProvider={activeThread ? (activeThread as any).provider ?? undefined : undefined}
        onSelectProvider={handleSetProvider}
        models={currentModels}
        selectedModel={selectedModel}
        onSelectModel={setSelectedModel}
        demoRemaining={demoRemaining}
        onExport={handleExport}
        hasMessages={messages.length > 0}
      />

      <ChatThreadBar
        threads={flowThreads}
        activeThreadId={activeThreadId}
        onSelect={switchThread}
        onCreate={handleCreateThread}
        onClose={handleCloseThread}
        onRename={handleRenameThread}
      />

      {(contextBlockId || doc) && activeThread && (
        <div className="mt-1">
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
        <div className="mt-1">
          <SourceFilePicker
            files={sourceFiles}
            selected={selectedSourceFiles}
            onSelectionChange={setThreadSourceFiles}
          />
        </div>
      )}

      <ChatToolbar
        messageCount={messages.length}
        onNewChat={handleCreateThread}
        onClearContext={handleClearContext}
        onCompact={handleCompact}
      />

      {showWelcome && (
        <>
          <SuggestedPrompts prompts={suggestedPrompts} onSelect={handleSend} />
          <PromptTemplates
            onSelect={handleSend}
            hasBlock={!!contextBlockId}
            flowName={doc?.name}
            blockName={selectedBlock?.name}
          />
        </>
      )}

      <ChatMessageList isStreaming={isCurrentThreadStreaming}>
        {messages.map((m, i) => (
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
        {isCurrentThreadStreaming && streamingText && (
          <MessageBubble
            message={{
              id: streamingMessageId || 'streaming',
              role: 'assistant',
              content: streamingText,
              timestamp: new Date().toISOString(),
            }}
            isStreaming
          />
        )}
      </ChatMessageList>

      {isCurrentThreadStreaming && streamingTokens > 0 ? (
        <div className="px-3 py-1">
          <span className="text-2xs text-text-tertiary animate-pulse-soft">
            ~{streamingTokens > 1000 ? `${(streamingTokens / 1000).toFixed(1)}k` : streamingTokens} tokens generating...
          </span>
        </div>
      ) : (
        <TokenCounter
          promptTokens={totalTokensIn}
          completionTokens={totalTokensOut}
          inputCostPerM={showCost ? currentModelDetail!.inputCostPerM : undefined}
          outputCostPerM={showCost ? currentModelDetail!.outputCostPerM : undefined}
        />
      )}

      <ChatInput
        onSend={handleSend}
        onPreview={handlePreviewContext}
        onCancel={handleCancelStream}
        disabled={!doc || !activeThread}
      />

      {contextPreview && pendingMessage && (
        <ContextPreviewModal
          preview={contextPreview}
          onClose={clearContextPreview}
          onConfirm={confirmContextPreview}
        />
      )}
    </div>
  )
}
