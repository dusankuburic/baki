import clsx from 'clsx'
import {Square, Eye, ArrowUp, Maximize2, FileText, X} from 'lucide-react'
import {useRef, useEffect, useCallback, useState, useMemo} from 'react'
import {useChatStore, MAX_CONCURRENT_STREAMS} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import FileAutocomplete from './FileAutocomplete'
import SlashCommandAutocomplete, {type SlashCommand} from './SlashCommandAutocomplete'
import ExpandedChatInput from './ExpandedChatInput'

interface Props {
  onSend: (text: string, files: string[], excludeContext?: boolean) => void
  onPreview?: (text: string, files: string[], excludeContext?: boolean) => void
  onCancel?: () => void
  onFilesChange?: (files: string[]) => void
  onClearThread?: () => void
  onShowHelp?: () => void
  disabled?: boolean
  placeholder?: string
  // Queue-while-streaming (U1.6): Enter during a stream stages the message
  // instead of dead-ending; it auto-sends when the reply finishes.
  onQueue?: (text: string, files: string[], excludeContext?: boolean) => void
  queued?: {text: string; files: string[]; excludeContext?: boolean} | null
  onCancelQueue?: () => void
}

export default function ChatInput({
  onSend,
  onPreview,
  onCancel,
  onFilesChange,
  onClearThread,
  onShowHelp,
  disabled,
  placeholder,
  onQueue,
  queued,
  onCancelQueue,
}: Props) {
  const [value, setValue] = useState('')
  const [autocompleteQuery, setAutocompleteQuery] = useState<string | null>(null)
  const [slashQuery, setSlashQuery] = useState<string | null>(null)
  const [isExpanded, setIsExpanded] = useState(false)
  const [historyIndex, setHistoryIndex] = useState(-1)
  const [excludeContext, setExcludeContext] = useState(false)
  const [taggedFiles, setTaggedFiles] = useState<string[]>([])

  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const valueRef = useRef(value)
  valueRef.current = value

  const activeThreadId = useChatStore(s => s.activeThreadId)
  const setDraft = useChatStore(s => s.setDraft)
  const stagedPrompt = useChatStore(s => s.stagedPrompt)
  const setStagedPrompt = useChatStore(s => s.setStagedPrompt)

  // Draft persistence: seed the composer from the thread's saved draft when the
  // active thread changes, and flush the current text back to the thread it
  // belonged to on switch/unmount. Reading via getState() avoids subscribing to
  // the whole drafts map (which would re-seed mid-typing). Cleared on send.
  useEffect(() => {
    if (!activeThreadId) return
    setValue(useChatStore.getState().drafts[activeThreadId] ?? '')
    const threadId = activeThreadId
    return () => {
      setDraft(threadId, valueRef.current)
    }
  }, [activeThreadId, setDraft])

  // Staged prompts ("Explain/Fix with AI") land in the composer for review.
  // Declared after the draft-seed effect so that when both fire on the same
  // thread switch (staging into a new thread), this wins and the staged text
  // is what the user sees. Cleared once consumed.
  useEffect(() => {
    if (!stagedPrompt || stagedPrompt.threadId !== activeThreadId) return
    setValue(stagedPrompt.text)
    setStagedPrompt(null)
    requestAnimationFrame(() => {
      const el = textareaRef.current
      if (el) {
        el.focus()
        el.setSelectionRange(el.value.length, el.value.length)
      }
    })
  }, [stagedPrompt, activeThreadId, setStagedPrompt])
  // Per-thread streaming: the Send/Stop toggle reflects ONLY the active thread
  // so the user can keep composing in other idle threads while one generates.
  const streaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  // Global cap (mirrors backend): once MAX_CONCURRENT_STREAMS threads are
  // generating, disable Send across the board with a tooltip.
  const atCap = useChatStore(s => Object.keys(s.streams).length >= MAX_CONCURRENT_STREAMS)
  const provider = useChatStore(s => s.selectedProvider)
  const aiSettings = useSettingsStore(s => s.settings.ai)

  // Track the active thread's user-message count reactively. getState() in the
  // history memo below is a non-reactive snapshot, so this count is what
  // triggers recomputation when messages arrive (otherwise ArrowUp history
  // would miss the just-sent message until the active thread changed).
  const userMsgCount = useChatStore(s => {
    if (!activeThreadId) return 0
    const msgs = s.conversations.get(activeThreadId)
    if (!msgs) return 0
    let n = 0
    for (const m of msgs) if (m.role === 'user') n++
    return n
  })

  const history = useMemo(() => {
    if (!activeThreadId) return []
    const msgs = useChatStore.getState().getMessages(activeThreadId)
    return msgs
      .filter(m => m.role === 'user')
      .map(m => m.content)
      .reverse()
  }, [activeThreadId, userMsgCount])

  // U1.6: streaming no longer disables the TEXTAREA — typing, paste, and
  // ArrowUp history recall stay live while a reply generates; Enter routes
  // to the queue instead. `isDisabled` now only gates send-style actions.
  const isDisabled = disabled
  const canQueue = streaming && !disabled && !!onQueue
  const capTooltip =
    atCap && !streaming
      ? `${MAX_CONCURRENT_STREAMS} chats are generating — wait for one to finish or stop it`
      : undefined

  useEffect(() => {
    onFilesChange?.(taggedFiles)
  }, [taggedFiles, onFilesChange])

  const adjustHeight = useCallback(() => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = Math.min(el.scrollHeight, 200) + 'px'
  }, [])

  useEffect(() => {
    adjustHeight()
  }, [value, adjustHeight])

  const handleSend = useCallback(() => {
    const trimmed = value.trim()
    if (!trimmed && taggedFiles.length === 0) return
    if (isDisabled) return
    if (canQueue) {
      onQueue?.(trimmed, taggedFiles, excludeContext)
      // Reset the FULL composer like the send branch (F1.7): attachments
      // left behind were double-represented (chip + queued payload) and
      // silently re-attached to the next message.
      setValue('')
      if (activeThreadId) setDraft(activeThreadId, '')
      setHistoryIndex(-1)
      setTaggedFiles([])
      setAutocompleteQuery(null)
      setSlashQuery(null)
      return
    }
    onSend(trimmed, taggedFiles, excludeContext)
    setValue('')
    if (activeThreadId) setDraft(activeThreadId, '')
    setHistoryIndex(-1)
    setTaggedFiles([])
    setAutocompleteQuery(null)
    setSlashQuery(null)
    requestAnimationFrame(() => {
      if (textareaRef.current) {
        textareaRef.current.style.height = 'auto'
      }
    })
  }, [value, taggedFiles, isDisabled, canQueue, onQueue, onSend, excludeContext, activeThreadId, setDraft])

  const handlePreview = useCallback(() => {
    const trimmed = value.trim()
    if ((!trimmed && taggedFiles.length === 0) || isDisabled || !onPreview) return
    onPreview(trimmed, taggedFiles, excludeContext)
  }, [value, taggedFiles, isDisabled, onPreview, excludeContext])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      // Autocomplete navigation is handled by the sub-components via window event listeners
      if (autocompleteQuery !== null || slashQuery !== null) {
        return
      }

      if (e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey) {
        e.preventDefault()
        handleSend()
      } else if (
        e.key === 'ArrowUp' &&
        (value === '' || (textareaRef.current?.selectionStart === 0 && textareaRef.current?.selectionEnd === 0))
      ) {
        // History Up
        if (historyIndex < history.length - 1) {
          const next = historyIndex + 1
          setHistoryIndex(next)
          setValue(history[next])
          e.preventDefault()
        }
      } else if (e.key === 'ArrowDown' && historyIndex >= 0) {
        // History Down
        const next = historyIndex - 1
        setHistoryIndex(next)
        setValue(next >= 0 ? history[next] : '')
        e.preventDefault()
      }
    },
    [handleSend, autocompleteQuery, slashQuery, value, history, historyIndex],
  )

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const val = e.target.value
    setValue(val)
    setHistoryIndex(-1)

    const pos = e.target.selectionStart
    const textBefore = val.slice(0, pos)

    // Check for @ file mentions
    const lastAt = textBefore.lastIndexOf('@')
    const lastSpace = textBefore.lastIndexOf(' ')

    if (lastAt !== -1 && lastAt > lastSpace) {
      setAutocompleteQuery(textBefore.slice(lastAt + 1))
      setSlashQuery(null)
    }
    // Check for / slash commands
    else {
      const lastSlash = textBefore.lastIndexOf('/')
      const lastSpace = textBefore.lastIndexOf(' ')
      if (lastSlash !== -1 && (lastSlash === 0 || textBefore[lastSlash - 1] === ' ') && lastSlash > lastSpace) {
        setSlashQuery(textBefore.slice(lastSlash + 1))
        setAutocompleteQuery(null)
      } else {
        setAutocompleteQuery(null)
        setSlashQuery(null)
      }
    }
  }

  const handleSelectFile = (filename: string) => {
    const pos = textareaRef.current?.selectionStart ?? value.length
    const textBefore = value.slice(0, pos)
    const atIdx = textBefore.lastIndexOf('@')

    // Remove the @query from the input text
    const newValue = value.slice(0, atIdx) + value.slice(pos)
    setValue(newValue)

    // Add to attached files
    if (!taggedFiles.includes(filename)) {
      setTaggedFiles([...taggedFiles, filename])
    }

    setAutocompleteQuery(null)
    textareaRef.current?.focus()
  }

  const handleRemoveFile = (filename: string) => {
    setTaggedFiles(taggedFiles.filter(f => f !== filename))
  }

  const handleSelectCommand = (cmd: SlashCommand) => {
    const pos = textareaRef.current?.selectionStart ?? value.length
    const textBefore = value.slice(0, pos)
    const lastSlash = textBefore.lastIndexOf('/')

    // Action commands run a local handler and must never be sent to the model.
    // Strip the "/cmd" token from the composer and dispatch.
    if (cmd.kind === 'action') {
      setValue(value.slice(0, lastSlash) + value.slice(pos))
      setSlashQuery(null)
      if (cmd.action === 'clear') onClearThread?.()
      else if (cmd.action === 'help') onShowHelp?.()
      textareaRef.current?.focus()
      return
    }

    const newValue = value.slice(0, lastSlash) + cmd.id + ' ' + value.slice(pos)
    setValue(newValue)
    setSlashQuery(null)
    textareaRef.current?.focus()
  }

  const hasContent = value.trim().length > 0 || taggedFiles.length > 0
  const tokenEstimate = Math.ceil(value.length / 4)
  const providerCfg = aiSettings.providers[provider as keyof typeof aiSettings.providers]
  const maxTokens = (providerCfg as {maxTokens?: number} | undefined)?.maxTokens ?? 4096
  const tokenPct = tokenEstimate / maxTokens
  const tokenClass =
    tokenPct >= 0.95 ? 'text-semantic-error' : tokenPct >= 0.8 ? 'text-semantic-warning' : 'text-text-tertiary'

  return (
    <div className="px-3 pb-3 pt-1">
      <div
        className={clsx(
          'relative flex flex-col bg-surface-2 border rounded-xl transition-all duration-200 shadow-sm',
          hasContent && !isDisabled
            ? 'border-brand-500/40 ring-1 ring-brand-500/10 shadow-lg shadow-brand-500/5'
            : 'border-border-default hover:border-border-strong',
        )}
      >
        {/* Top Attached Files Area */}
        {taggedFiles.length > 0 && (
          <div className="flex flex-wrap gap-2 px-3 pt-2">
            {taggedFiles.map(file => {
              const parts = file.split(/[/\\]/)
              const name = parts[parts.length - 1]
              return (
                <div
                  key={file}
                  className="flex items-center gap-1.5 px-2 py-1 bg-surface-3 border border-border-default rounded-md text-xs text-text-secondary animate-fade-in shadow-sm"
                >
                  <FileText size={12} className="text-brand-400" />
                  <span className="truncate max-w-[150px] font-medium" title={file}>
                    {name}
                  </span>
                  <button
                    onClick={() => handleRemoveFile(file)}
                    className="p-0.5 -mr-1 hover:bg-surface-4 rounded hover:text-text-primary transition-colors"
                  >
                    <X size={12} />
                  </button>
                </div>
              )
            })}
          </div>
        )}

        {queued && (
          <div className="flex items-center gap-2 px-3 py-1.5 bg-brand-500/5 border-t border-border-subtle/50 text-2xs text-text-secondary">
            <span className="w-1.5 h-1.5 rounded-full bg-brand-400 animate-pulse-soft shrink-0" />
            <span className="truncate">
              Queued — sends when the reply finishes: <span className="text-text-primary">{queued.text}</span>
            </span>
            {onCancelQueue && (
              <button
                onClick={onCancelQueue}
                className="ml-auto shrink-0 text-text-tertiary hover:text-text-primary transition-colors"
                aria-label="Cancel queued message"
              >
                <X size={12} />
              </button>
            )}
          </div>
        )}

        <div className="relative flex items-end gap-2 px-3 py-2">
          <textarea
            ref={textareaRef}
            className="flex-1 bg-transparent border-none outline-none text-sm leading-relaxed text-text-primary placeholder:text-text-tertiary resize-none py-0 min-h-[20px] max-h-[200px] z-10 scrollbar-none font-sans"
            placeholder={
              canQueue
                ? 'Reply streaming — type and press Enter to queue your next message…'
                : placeholder || 'Ask anything... (@ to tag files, / for commands)'
            }
            value={value}
            onChange={handleChange}
            onKeyDown={handleKeyDown}
            disabled={isDisabled}
            rows={1}
          />

          <div className="flex items-center gap-1.5 pb-0.5 z-10">
            {onPreview && hasContent && !isDisabled && (
              <button
                onClick={handlePreview}
                className="p-1.5 rounded-lg text-text-tertiary hover:text-text-primary hover:bg-surface-3 transition-all"
                title="Preview context"
                aria-label="Preview context"
              >
                <Eye size={16} />
              </button>
            )}

            <button
              onClick={() => setIsExpanded(true)}
              className="p-1.5 rounded-lg text-text-tertiary hover:text-text-primary hover:bg-surface-3 transition-all"
              title="Expand editor"
              aria-label="Expand editor"
            >
              <Maximize2 size={16} />
            </button>

            {streaming ? (
              <button
                onClick={onCancel}
                className="p-1.5 rounded-lg bg-semantic-error/10 text-semantic-error hover:bg-semantic-error/20 transition-all border border-semantic-error/20"
                title="Stop generation"
                aria-label="Stop generation"
              >
                <Square size={16} fill="currentColor" />
              </button>
            ) : (
              <button
                onClick={handleSend}
                disabled={!hasContent || isDisabled || atCap}
                title={capTooltip}
                aria-label="Send message"
                className={clsx(
                  'p-1.5 rounded-lg transition-all',
                  hasContent && !atCap
                    ? 'bg-brand-500 text-brand-foreground shadow-lg shadow-brand-500/20 hover:bg-brand-600'
                    : 'text-text-tertiary hover:bg-surface-3',
                )}
              >
                <ArrowUp size={18} strokeWidth={2.5} />
              </button>
            )}
          </div>
        </div>

        {/* Footer info (optional context toggle) */}
        {hasContent && !isDisabled && (
          <div className="flex items-center justify-between px-3 py-1.5 bg-surface-3/50 rounded-b-xl border-t border-border-subtle/50">
            <div className="flex items-center gap-2">
              <label className="flex items-center gap-1.5 cursor-pointer group">
                <input
                  type="checkbox"
                  className="w-3.5 h-3.5 rounded border-border-default bg-surface-2 text-brand-500 focus:ring-brand-500 focus:ring-offset-surface-2"
                  checked={excludeContext}
                  onChange={e => setExcludeContext(e.target.checked)}
                />
                <span className="text-[10px] font-medium text-text-tertiary group-hover:text-text-secondary transition-colors">
                  Exclude context
                </span>
              </label>
            </div>
            <span className={clsx('text-[10px] tabular-nums', tokenClass)}>
              ~{tokenEstimate.toLocaleString()} tokens
            </span>
          </div>
        )}

        {autocompleteQuery !== null && (
          <FileAutocomplete
            query={autocompleteQuery}
            onSelect={handleSelectFile}
            onClose={() => setAutocompleteQuery(null)}
          />
        )}

        {slashQuery !== null && (
          <SlashCommandAutocomplete
            query={slashQuery}
            onSelect={handleSelectCommand}
            onClose={() => setSlashQuery(null)}
          />
        )}
      </div>

      {isExpanded && (
        <ExpandedChatInput
          value={value}
          onChange={setValue}
          onSend={handleSend}
          onPreview={handlePreview || (() => {})}
          onClose={() => setIsExpanded(false)}
          excludeContext={excludeContext}
          onExcludeContextChange={setExcludeContext}
        />
      )}
    </div>
  )
}
