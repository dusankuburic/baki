import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Square, Eye, ArrowUp, Maximize2, FileText, X} from 'lucide-react'
import {useRef, useEffect, useCallback, useState, useMemo} from 'react'
import type {SourceFileInfo} from '@/types'
import {useChatStore, MAX_CONCURRENT_STREAMS} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import FileAutocomplete from './FileAutocomplete'
import SlashCommandAutocomplete, {type SlashCommand} from './SlashCommandAutocomplete'
import ExpandedChatInput from './ExpandedChatInput'
import ChatUsageMeter from './ChatUsageMeter'

const EMPTY_SOURCE_FILES: SourceFileInfo[] = []

interface Props {
  onSend: (text: string, files: string[], excludeContext?: boolean) => void
  onPreview?: (text: string, files: string[], excludeContext?: boolean) => void
  onCancel?: () => void
  onClearThread?: () => void
  onShowHelp?: () => void
  disabled?: boolean
  placeholder?: string
  // Queue-while-streaming (U1.6): Enter during a stream stages the message
  // instead of dead-ending; it auto-sends when the reply finishes.
  onQueue?: (text: string, files: string[], excludeContext?: boolean) => void
  queued?: {text: string; files: string[]; excludeContext?: boolean} | null
  onCancelQueue?: () => void
  // The flow's source files, for @-mention completion. Owned by AITab so the
  // menu does not refetch the same list every time it opens.
  sourceFiles?: SourceFileInfo[]
  // Thread usage totals, rendered in the composer's always-present footer.
  promptTokens?: number
  completionTokens?: number
  inputCostPerM?: number
  outputCostPerM?: number
}

export default function ChatInput({
  onSend,
  onPreview,
  onCancel,
  onClearThread,
  onShowHelp,
  disabled,
  placeholder,
  onQueue,
  queued,
  onCancelQueue,
  sourceFiles = EMPTY_SOURCE_FILES,
  promptTokens = 0,
  completionTokens = 0,
  inputCostPerM,
  outputCostPerM,
}: Props) {
  const [value, setValue] = useState('')
  const {t} = useTranslation('chat')
  const [autocompleteQuery, setAutocompleteQuery] = useState<string | null>(null)
  const [slashQuery, setSlashQuery] = useState<string | null>(null)
  // A menu only OWNS the keyboard while it has something to pick. Gating on
  // `query !== null` alone dead-ended Enter whenever the query matched nothing
  // ("/xyz" + Enter did nothing at all): the popup had already rendered null,
  // so no one handled the key and the composer had bowed out.
  const [fileMatches, setFileMatches] = useState(0)
  const [slashMatches, setSlashMatches] = useState(0)
  const [isExpanded, setIsExpanded] = useState(false)
  const isExpandedRef = useRef(false)
  const [historyIndex, setHistoryIndex] = useState(-1)
  const [excludeContext, setExcludeContext] = useState(false)
  const [taggedFiles, setTaggedFiles] = useState<string[]>([])

  // Two textareas exist while the expanded editor is open (the inline one
  // stays mounted behind the modal), so selection-dependent helpers must act
  // on whichever one the user is actually typing in.
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const expandedRef = useRef<HTMLTextAreaElement>(null)
  const activeTextarea = useCallback(
    () => (isExpandedRef.current ? expandedRef.current : textareaRef.current),
    [],
  )
  const valueRef = useRef(value)
  // Synced in a commit-phase effect rather than assigned during render:
  // mutating a ref while rendering is not safe under concurrent rendering
  // (a render can be discarded, leaving the ref describing a tree that was
  // never committed). The draft-flush cleanup only reads this after commit.
  useEffect(() => {
    valueRef.current = value
  })
  useEffect(() => {
    isExpandedRef.current = isExpanded
  }, [isExpanded])

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
    // Seeds the composer from the persisted draft store when the thread changes.
    // eslint-disable-next-line react-hooks/set-state-in-effect
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
    // Consumes a one-shot staged prompt handed over by another component.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setValue(stagedPrompt.text)
    setStagedPrompt(null)
    requestAnimationFrame(() => {
      const el = activeTextarea()
      if (el) {
        el.focus()
        el.setSelectionRange(el.value.length, el.value.length)
      }
    })
  }, [stagedPrompt, activeThreadId, setStagedPrompt, activeTextarea])
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
    // userMsgCount is a deliberate reactivity TRIGGER, not an input: the body
    // reads history via getState() (a non-reactive snapshot), so without this
    // dep ArrowUp recall would miss the just-sent message.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeThreadId, userMsgCount])

  // U1.6: streaming no longer disables the TEXTAREA — typing, paste, and
  // ArrowUp history recall stay live while a reply generates; Enter routes
  // to the queue instead. `isDisabled` now only gates send-style actions.
  const isDisabled = disabled
  const canQueue = streaming && !disabled && !!onQueue
  const capTooltip = atCap && !streaming ? t('composer.atCap', {count: MAX_CONCURRENT_STREAMS}) : undefined

  // NOTE: `taggedFiles` is deliberately NOT mirrored into the thread's
  // selectedSourceFiles. It is a PER-MESSAGE @-mention override that already
  // travels to the backend as onSend/onQueue/onPreview's `files` argument
  // (buildRequest's `overrideFiles`). The thread's persistent selection is owned
  // by SourceFilePicker. An effect used to push taggedFiles outward, but since
  // it starts [] and is cleared after every send, it wrote an EMPTY selection on
  // mount, after each message, and on every thread switch — silently dropping
  // the user's source-file context from the second message onward.

  // Closing a menu also clears its match count, so a stale non-zero count can
  // never keep the composer from handling Enter.
  const closeFileMenu = useCallback(() => {
    setAutocompleteQuery(null)
    setFileMatches(0)
  }, [])
  const closeSlashMenu = useCallback(() => {
    setSlashQuery(null)
    setSlashMatches(0)
  }, [])

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
      setFileMatches(0)
      setSlashMatches(0)
      return
    }
    onSend(trimmed, taggedFiles, excludeContext)
    setValue('')
    if (activeThreadId) setDraft(activeThreadId, '')
    setHistoryIndex(-1)
    setTaggedFiles([])
    setAutocompleteQuery(null)
    setSlashQuery(null)
    setFileMatches(0)
    setSlashMatches(0)
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
      // Autocomplete navigation is handled by the sub-components via window
      // event listeners — but only while they actually have matches to show.
      if ((autocompleteQuery !== null && fileMatches > 0) || (slashQuery !== null && slashMatches > 0)) {
        return
      }

      if (e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey) {
        e.preventDefault()
        handleSend()
      } else if (
        e.key === 'ArrowUp' &&
        (value === '' || (activeTextarea()?.selectionStart === 0 && activeTextarea()?.selectionEnd === 0))
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
    [handleSend, autocompleteQuery, slashQuery, fileMatches, slashMatches, value, history, historyIndex, activeTextarea],
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
      setSlashMatches(0)
    }
    // Check for / slash commands
    else {
      const lastSlash = textBefore.lastIndexOf('/')
      const lastSpace = textBefore.lastIndexOf(' ')
      if (lastSlash !== -1 && (lastSlash === 0 || textBefore[lastSlash - 1] === ' ') && lastSlash > lastSpace) {
        setSlashQuery(textBefore.slice(lastSlash + 1))
        setAutocompleteQuery(null)
        setFileMatches(0)
      } else {
        setAutocompleteQuery(null)
        setSlashQuery(null)
        setFileMatches(0)
        setSlashMatches(0)
      }
    }
  }

  const handleSelectFile = (filename: string) => {
    const pos = activeTextarea()?.selectionStart ?? value.length
    const textBefore = value.slice(0, pos)
    const atIdx = textBefore.lastIndexOf('@')

    // Remove the @query from the input text
    const newValue = value.slice(0, atIdx) + value.slice(pos)
    setValue(newValue)

    // Add to attached files
    if (!taggedFiles.includes(filename)) {
      setTaggedFiles([...taggedFiles, filename])
    }

    closeFileMenu()
    activeTextarea()?.focus()
  }

  const handleRemoveFile = (filename: string) => {
    setTaggedFiles(taggedFiles.filter(f => f !== filename))
  }

  const handleSelectCommand = (cmd: SlashCommand) => {
    const pos = activeTextarea()?.selectionStart ?? value.length
    const textBefore = value.slice(0, pos)
    const lastSlash = textBefore.lastIndexOf('/')

    // Action commands run a local handler and must never be sent to the model.
    // Strip the "/cmd" token from the composer and dispatch.
    if (cmd.kind === 'action') {
      setValue(value.slice(0, lastSlash) + value.slice(pos))
      closeSlashMenu()
      if (cmd.action === 'clear') onClearThread?.()
      else if (cmd.action === 'help') onShowHelp?.()
      activeTextarea()?.focus()
      return
    }

    const newValue = value.slice(0, lastSlash) + cmd.id + ' ' + value.slice(pos)
    setValue(newValue)
    closeSlashMenu()
    activeTextarea()?.focus()
  }

  // One definition, rendered either inline or inside the expanded editor.
  // The expanded composer used to silently lose @-mentions and /commands —
  // exactly the editor a user opens to write a detailed, file-referencing
  // prompt.
  const menus = (
    <>
      {autocompleteQuery !== null && (
        <FileAutocomplete
          query={autocompleteQuery}
          files={sourceFiles}
          onSelect={handleSelectFile}
          onClose={closeFileMenu}
          onMatchCount={setFileMatches}
        />
      )}
      {slashQuery !== null && (
        <SlashCommandAutocomplete
          query={slashQuery}
          onSelect={handleSelectCommand}
          onClose={closeSlashMenu}
          onMatchCount={setSlashMatches}
        />
      )}
    </>
  )

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
              {t('composer.queuedPrefix')} <span className="text-text-primary">{queued.text}</span>
            </span>
            {onCancelQueue && (
              <button
                onClick={onCancelQueue}
                className="ml-auto shrink-0 text-text-tertiary hover:text-text-primary transition-colors"
                aria-label={t('composer.cancelQueued')}
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
            aria-label={t('composer.inputAria')}
            placeholder={canQueue ? t('composer.queuePlaceholder') : placeholder || t('composer.placeholder')}
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
                title={t('composer.previewContext')}
                aria-label={t('composer.previewContext')}
              >
                <Eye size={16} />
              </button>
            )}

            <button
              onClick={() => setIsExpanded(true)}
              className="p-1.5 rounded-lg text-text-tertiary hover:text-text-primary hover:bg-surface-3 transition-all"
              title={t('composer.expandEditor')}
              aria-label={t('composer.expandEditor')}
            >
              <Maximize2 size={16} />
            </button>

            {streaming ? (
              <button
                onClick={onCancel}
                className="p-1.5 rounded-lg bg-semantic-error/10 text-semantic-error hover:bg-semantic-error/20 transition-all border border-semantic-error/20"
                title={t('composer.stopGeneration')}
                aria-label={t('composer.stopGeneration')}
              >
                <Square size={16} fill="currentColor" />
              </button>
            ) : (
              <button
                onClick={handleSend}
                disabled={!hasContent || isDisabled || atCap}
                title={capTooltip}
                aria-label={t('composer.sendMessage')}
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

        {/* Footer: ALWAYS rendered. Gating it on `hasContent` made the whole
            conversation jump on the first keystroke and again on send. */}
        <div className="flex items-center justify-between gap-2 px-3 py-1.5 bg-surface-3/50 rounded-b-xl border-t border-border-subtle/50">
          <label className="flex items-center gap-1.5 cursor-pointer group shrink-0">
            <input
              type="checkbox"
              className="w-3.5 h-3.5 rounded border-border-default bg-surface-2 text-brand-500 focus:ring-brand-500 focus:ring-offset-surface-2"
              checked={excludeContext}
              onChange={e => setExcludeContext(e.target.checked)}
            />
            <span className="cq-exclude-label text-[10px] font-medium text-text-tertiary group-hover:text-text-secondary transition-colors">
              {t('composer.excludeContext')}
            </span>
          </label>
          <div className="flex items-center gap-2 min-w-0">
            {hasContent && (
              <span className={clsx('text-[10px] tabular-nums shrink-0', tokenClass)}>
                {t('composer.tokenEstimate', {count: tokenEstimate})}
              </span>
            )}
            <ChatUsageMeter
              promptTokens={promptTokens}
              completionTokens={completionTokens}
              inputCostPerM={inputCostPerM}
              outputCostPerM={outputCostPerM}
            />
          </div>
        </div>

        {!isExpanded && menus}
      </div>

      {isExpanded && (
        <ExpandedChatInput
          value={value}
          textareaRef={expandedRef}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onSend={handleSend}
          onPreview={onPreview ? handlePreview : undefined}
          onClose={() => setIsExpanded(false)}
          excludeContext={excludeContext}
          onExcludeContextChange={setExcludeContext}
          canSend={hasContent && !isDisabled}
          menus={menus}
        />
      )}
    </div>
  )
}
