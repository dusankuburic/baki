import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Eye, Maximize2, FileText, X, AtSign, Slash} from 'lucide-react'
import {useRef, useEffect, useCallback, useState, useMemo} from 'react'
import type {SourceFileInfo} from '@/types'
import {useChatStore, MAX_CONCURRENT_STREAMS} from '@/stores/chatStore'
import {useOptionalToast} from '@/components/shared/Toast'
import {isImeKeydown} from '@/lib/imeKey'
import FileAutocomplete from './FileAutocomplete'
import SlashCommandAutocomplete, {type SlashCommand} from './SlashCommandAutocomplete'
import ExpandedChatInput from './ExpandedChatInput'
import ChatUsageMeter from './ChatUsageMeter'
import {ComposerChip, ComposerIconButton, ContextToggle, SendButton} from './ComposerControls'
import {useKeyboard} from '@/hooks/useKeyboard'

const EMPTY_SOURCE_FILES: SourceFileInfo[] = []

interface Props {
  onSend: (text: string, files: string[], excludeContext?: boolean) => void
  // onConfirmed runs once the previewed message is actually sent — the
  // composer is only cleared then (a cancelled preview keeps the draft).
  onPreview?: (text: string, files: string[], excludeContext?: boolean, onConfirmed?: () => void) => void
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
  // The selected model's context window, for the prompt-size warning.
  contextLimit?: number
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
  contextLimit,
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
  // The in-progress draft, stashed when ArrowUp first steps into sent-message
  // history and restored when ArrowDown steps back out — recalling history
  // used to overwrite the draft for good.
  const historyDraftRef = useRef<string | null>(null)
  const [excludeContext, setExcludeContext] = useState(false)
  const [taggedFiles, setTaggedFiles] = useState<string[]>([])

  // Two textareas exist while the expanded editor is open (the inline one
  // stays mounted behind the modal), so selection-dependent helpers must act
  // on whichever one the user is actually typing in.
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const expandedRef = useRef<HTMLTextAreaElement>(null)
  const activeTextarea = useCallback(() => (isExpandedRef.current ? expandedRef.current : textareaRef.current), [])
  // The @/slash menus listen on window; they only act on keys typed here.
  const isComposerTarget = useCallback(
    (target: EventTarget | null) =>
      target !== null && (target === textareaRef.current || target === expandedRef.current),
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
    // The composer stays mounted across thread switches, so everything else
    // that belongs to the thread being left resets here: @-file chips, the
    // history cursor and its stashed draft, the context toggle, open menus.
    setTaggedFiles([])
    setHistoryIndex(-1)
    historyDraftRef.current = null
    setExcludeContext(false)
    setAutocompleteQuery(null)
    setFileMatches(0)
    setSlashQuery(null)
    setSlashMatches(0)
    const threadId = activeThreadId
    return () => {
      // While the composer shows a RECALLED history entry, the draft is the
      // stashed text, not the recalled message — saving the latter lost the
      // draft to an ArrowUp pressed once too often before a thread switch.
      setDraft(threadId, historyDraftRef.current ?? valueRef.current)
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
  const toast = useOptionalToast()

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
  // A menu left open after the composer lost focus kept intercepting keys
  // (see isComposerTarget) — close both when focus leaves.
  const closeMenus = useCallback(() => {
    closeFileMenu()
    closeSlashMenu()
  }, [closeFileMenu, closeSlashMenu])

  // The cap lives in CSS (.chat-composer-input: a share of the window, so it
  // scales with the screen) and is read back here, so the two never disagree.
  const adjustHeight = useCallback(() => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    const max = parseFloat(getComputedStyle(el).maxHeight) || 200
    el.style.height = Math.min(el.scrollHeight, max) + 'px'
  }, [])

  useEffect(() => {
    adjustHeight()
  }, [value, adjustHeight])

  // resetComposer empties the FULL composer after a send, queue or confirmed
  // preview (F1.7): attachments left behind were double-represented (chip +
  // sent payload) and silently re-attached to the next message.
  const resetComposer = useCallback(() => {
    setValue('')
    if (activeThreadId) setDraft(activeThreadId, '')
    setHistoryIndex(-1)
    historyDraftRef.current = null
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
  }, [activeThreadId, setDraft])

  // closeExpanded leaves the expanded editor the way its Send button does and
  // hands focus back to the inline composer: the dialog's own restore lands
  // on whatever opened it — the Expand button, so Enter reopened the editor.
  const closeExpanded = useCallback(() => {
    if (!isExpandedRef.current) return
    setIsExpanded(false)
    requestAnimationFrame(() => textareaRef.current?.focus())
  }, [])

  // handleSend sends or queues the composer's content; true when it did.
  const handleSend = useCallback((): boolean => {
    const trimmed = value.trim()
    if (!trimmed && taggedFiles.length === 0) return false
    if (isDisabled) return false
    if (canQueue) {
      onQueue?.(trimmed, taggedFiles, excludeContext)
      resetComposer()
      closeExpanded()
      return true
    }
    // At the concurrency cap the send is refused — keep the text instead of
    // clearing a message that never went anywhere, and say why (the disabled
    // Send button's tooltip is out of reach of an Enter press).
    if (atCap) {
      toast?.warning(t('errors.busyTitle'), {description: t('errors.busyBody', {count: MAX_CONCURRENT_STREAMS})})
      return false
    }
    onSend(trimmed, taggedFiles, excludeContext)
    resetComposer()
    // Enter in the expanded editor sends like its Send button — which also
    // closes it; it used to leave an empty editor open.
    closeExpanded()
    return true
  }, [
    value,
    taggedFiles,
    isDisabled,
    canQueue,
    atCap,
    onQueue,
    onSend,
    excludeContext,
    resetComposer,
    closeExpanded,
    toast,
    t,
  ])

  // mod+enter sends from either composer. The expanded editor renders in a
  // portal outside the chat panel, so this checks the event target instead of
  // a containerRef. An open @/slash menu claims Enter in its capture listener
  // first (defaultPrevented), and then the chord picks the item, as Enter does.
  useKeyboard({
    scope: 'chat',
    handlers: {
      'ai.send': e => {
        if (e.defaultPrevented || isImeKeydown(e)) return
        if (e.target !== textareaRef.current && e.target !== expandedRef.current) return
        e.preventDefault()
        handleSend()
      },
    },
  })

  const handlePreview = useCallback(() => {
    const trimmed = value.trim()
    if ((!trimmed && taggedFiles.length === 0) || isDisabled || !onPreview) return
    onPreview(trimmed, taggedFiles, excludeContext, resetComposer)
  }, [value, taggedFiles, isDisabled, onPreview, excludeContext, resetComposer])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      // An IME is composing (CJK input): Enter confirms the candidate, it must
      // not send the half-composed message.
      if (isImeKeydown(e)) return
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
          if (historyIndex === -1) historyDraftRef.current = value
          const next = historyIndex + 1
          setHistoryIndex(next)
          setValue(history[next])
          e.preventDefault()
        }
      } else if (e.key === 'ArrowDown' && historyIndex >= 0) {
        // History Down — past the newest entry, the stashed draft returns.
        const next = historyIndex - 1
        setHistoryIndex(next)
        if (next >= 0) {
          setValue(history[next])
        } else {
          setValue(historyDraftRef.current ?? '')
          historyDraftRef.current = null
        }
        e.preventDefault()
      }
    },
    [
      handleSend,
      autocompleteQuery,
      slashQuery,
      fileMatches,
      slashMatches,
      value,
      history,
      historyIndex,
      activeTextarea,
    ],
  )

  // syncMenus opens or closes the @ and / menus for the text before the
  // caret — the same rule whether the trigger was typed or put there by a
  // toolbar chip.
  const syncMenus = useCallback((val: string, pos: number) => {
    const textBefore = val.slice(0, pos)
    const lastSpace = textBefore.lastIndexOf(' ')

    // @ file mentions
    const lastAt = textBefore.lastIndexOf('@')
    if (lastAt !== -1 && lastAt > lastSpace) {
      setAutocompleteQuery(textBefore.slice(lastAt + 1))
      setSlashQuery(null)
      setSlashMatches(0)
      return
    }
    // / slash commands
    const lastSlash = textBefore.lastIndexOf('/')
    if (lastSlash !== -1 && (lastSlash === 0 || textBefore[lastSlash - 1] === ' ') && lastSlash > lastSpace) {
      setSlashQuery(textBefore.slice(lastSlash + 1))
      setAutocompleteQuery(null)
      setFileMatches(0)
      return
    }
    setAutocompleteQuery(null)
    setSlashQuery(null)
    setFileMatches(0)
    setSlashMatches(0)
  }, [])

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const val = e.target.value
    setValue(val)
    // Editing a recalled message makes it the draft.
    setHistoryIndex(-1)
    historyDraftRef.current = null
    syncMenus(val, e.target.selectionStart)
  }

  // insertTrigger puts "@" or "/" at the caret — with a separating space when
  // it would otherwise glue onto a word — and opens the matching menu, exactly
  // as typing the character does. Wired to the toolbar chips.
  const insertTrigger = useCallback(
    (char: '@' | '/') => {
      const el = activeTextarea()
      const start = el?.selectionStart ?? value.length
      const end = el?.selectionEnd ?? start
      const before = value.slice(0, start)
      const inserted = (before.length > 0 && !/\s$/.test(before) ? ' ' : '') + char
      const next = before + inserted + value.slice(end)
      const caret = before.length + inserted.length
      setValue(next)
      setHistoryIndex(-1)
      historyDraftRef.current = null
      syncMenus(next, caret)
      requestAnimationFrame(() => {
        const target = activeTextarea()
        if (!target) return
        target.focus()
        target.setSelectionRange(caret, caret)
      })
    },
    [value, activeTextarea, syncMenus],
  )

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
          isOwnTarget={isComposerTarget}
        />
      )}
      {slashQuery !== null && (
        <SlashCommandAutocomplete
          query={slashQuery}
          onSelect={handleSelectCommand}
          onClose={closeSlashMenu}
          onMatchCount={setSlashMatches}
          isOwnTarget={isComposerTarget}
          canClear={!!onClearThread}
        />
      )}
    </>
  )

  const hasContent = value.trim().length > 0 || taggedFiles.length > 0
  const tokenEstimate = Math.ceil(value.length / 4)
  // Measured against the model's CONTEXT window; an unknown window (custom
  // model, no metadata) means no warning at all. The reply cap (4096) used
  // to stand in and turned the counter red at ~16k characters.
  const tokenPct = contextLimit && contextLimit > 0 ? tokenEstimate / contextLimit : 0
  const tokenClass =
    tokenPct >= 0.95 ? 'text-semantic-error' : tokenPct >= 0.8 ? 'text-semantic-warning' : 'text-text-tertiary'

  return (
    <div className="chat-gutter pb-3 pt-1">
      <div
        className={clsx(
          'relative flex flex-col bg-surface-2 border border-border-default rounded-xl shadow-sm',
          'transition-[border-color,box-shadow] duration-fast hover:border-border-strong',
          'focus-within:border-brand-500/60 focus-within:ring-2 focus-within:ring-brand-500/15',
        )}
      >
        {/* Attached files */}
        {taggedFiles.length > 0 && (
          <div className="flex flex-wrap gap-2 px-3 pt-2.5">
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
                    aria-label={t('composer.removeFile', {name})}
                    title={t('composer.removeFile', {name})}
                  >
                    <X size={12} />
                  </button>
                </div>
              )
            })}
          </div>
        )}

        {queued && (
          <div className="flex items-center gap-2 px-3 py-1.5 bg-brand-500/5 border-b border-border-subtle/50 first:rounded-t-xl text-2xs text-text-secondary">
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

        <textarea
          ref={textareaRef}
          data-chat-composer=""
          className="w-full bg-transparent border-none outline-none text-sm leading-relaxed text-text-primary placeholder:text-text-tertiary resize-none px-3.5 pt-3 pb-1 min-h-[22px] chat-composer-input scrollbar-none font-sans"
          aria-label={t('composer.inputAria')}
          placeholder={canQueue ? t('composer.queuePlaceholder') : placeholder || t('composer.placeholder')}
          value={value}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onBlur={closeMenus}
          disabled={isDisabled}
          rows={1}
        />

        {/* Toolbar: ALWAYS rendered, at a fixed height. Gating rows on content
            made the whole conversation jump on the first keystroke and again
            on send. */}
        <div className="flex items-center gap-1.5 px-2 pb-2 pt-1 min-w-0">
          <ComposerChip
            icon={AtSign}
            label={t('composer.insertFile')}
            hint={t('composer.insertFileHint')}
            labelClass="cq-chip-label"
            onClick={() => insertTrigger('@')}
          />
          <ComposerChip
            icon={Slash}
            label={t('composer.insertCommand')}
            hint={t('composer.insertCommandHint')}
            labelClass="cq-chip-label"
            onClick={() => insertTrigger('/')}
          />
          <ContextToggle
            checked={excludeContext}
            onChange={setExcludeContext}
            label={t('composer.excludeContext')}
            labelClass="cq-exclude-label"
          />
          <div className="ml-auto flex items-center gap-1.5 min-w-0">
            {hasContent && (
              <span className={clsx('text-2xs tabular-nums truncate', tokenClass)}>
                {t('composer.tokenEstimate', {count: tokenEstimate})}
              </span>
            )}
            <ChatUsageMeter
              promptTokens={promptTokens}
              completionTokens={completionTokens}
              inputCostPerM={inputCostPerM}
              outputCostPerM={outputCostPerM}
            />
            {onPreview && hasContent && !isDisabled && (
              <ComposerIconButton icon={Eye} label={t('composer.previewContext')} onClick={handlePreview} />
            )}
            <ComposerIconButton
              icon={Maximize2}
              label={t('composer.expandEditor')}
              onClick={() => setIsExpanded(true)}
            />
            <SendButton
              mode={streaming ? 'stop' : 'send'}
              disabled={!hasContent || isDisabled || atCap}
              title={capTooltip}
              onClick={streaming ? onCancel : handleSend}
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
          onBlur={closeMenus}
          onSend={handleSend}
          onPreview={onPreview ? handlePreview : undefined}
          onClose={() => setIsExpanded(false)}
          excludeContext={excludeContext}
          onExcludeContextChange={setExcludeContext}
          canSend={hasContent && !isDisabled && (canQueue || !atCap)}
          menus={menus}
          onInsertTrigger={insertTrigger}
          tokenLabel={hasContent ? t('composer.tokenEstimate', {count: tokenEstimate}) : undefined}
          tokenClass={tokenClass}
        />
      )}
    </div>
  )
}
