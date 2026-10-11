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
  onPreview?: (text: string, files: string[], excludeContext?: boolean, onConfirmed?: () => void) => void
  onCancel?: () => void
  onClearThread?: () => void
  onShowHelp?: () => void
  disabled?: boolean
  placeholder?: string
  onQueue?: (text: string, files: string[], excludeContext?: boolean) => void
  queued?: {text: string; files: string[]; excludeContext?: boolean} | null
  onCancelQueue?: () => void
  sourceFiles?: SourceFileInfo[]
  promptTokens?: number
  completionTokens?: number
  inputCostPerM?: number
  outputCostPerM?: number
  contextLimit?: number
  guidance?: {text: string; actionLabel: string; onAction: () => void} | null
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
  guidance,
}: Props) {
  const [value, setValue] = useState('')
  const {t} = useTranslation('chat')
  const [autocompleteQuery, setAutocompleteQuery] = useState<string | null>(null)
  const [slashQuery, setSlashQuery] = useState<string | null>(null)
  const [fileMatches, setFileMatches] = useState(0)
  const [slashMatches, setSlashMatches] = useState(0)
  const [isExpanded, setIsExpanded] = useState(false)
  const isExpandedRef = useRef(false)
  const [historyIndex, setHistoryIndex] = useState(-1)
  const historyDraftRef = useRef<string | null>(null)
  const [excludeContext, setExcludeContext] = useState(false)
  const [taggedFiles, setTaggedFiles] = useState<string[]>([])

  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const expandedRef = useRef<HTMLTextAreaElement>(null)
  const activeTextarea = useCallback(() => (isExpandedRef.current ? expandedRef.current : textareaRef.current), [])
  const isComposerTarget = useCallback(
    (target: EventTarget | null) =>
      target !== null && (target === textareaRef.current || target === expandedRef.current),
    [],
  )
  const valueRef = useRef(value)
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

  useEffect(() => {
    if (!activeThreadId) return
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setValue(useChatStore.getState().drafts[activeThreadId] ?? '')
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
      setDraft(threadId, historyDraftRef.current ?? valueRef.current)
    }
  }, [activeThreadId, setDraft])

  useEffect(() => {
    if (!stagedPrompt || stagedPrompt.threadId !== activeThreadId) return
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
  const streaming = useChatStore(s => !!(s.activeThreadId && s.streams[s.activeThreadId]))
  const atCap = useChatStore(s => Object.keys(s.streams).length >= MAX_CONCURRENT_STREAMS)
  const toast = useOptionalToast()

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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeThreadId, userMsgCount])

  const isDisabled = disabled
  const sendBlocked = !!guidance
  const canQueue = streaming && !disabled && !sendBlocked && !!onQueue
  const capTooltip = atCap && !streaming ? t('composer.atCap', {count: MAX_CONCURRENT_STREAMS}) : undefined

  const closeFileMenu = useCallback(() => {
    setAutocompleteQuery(null)
    setFileMatches(0)
  }, [])
  const closeSlashMenu = useCallback(() => {
    setSlashQuery(null)
    setSlashMatches(0)
  }, [])
  const closeMenus = useCallback(() => {
    closeFileMenu()
    closeSlashMenu()
  }, [closeFileMenu, closeSlashMenu])

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

  const closeExpanded = useCallback(() => {
    if (!isExpandedRef.current) return
    setIsExpanded(false)
    requestAnimationFrame(() => textareaRef.current?.focus())
  }, [])

  const handleSend = useCallback((): boolean => {
    const trimmed = value.trim()
    if (!trimmed && taggedFiles.length === 0) return false
    if (isDisabled || sendBlocked) return false
    if (canQueue) {
      onQueue?.(trimmed, taggedFiles, excludeContext)
      resetComposer()
      closeExpanded()
      return true
    }
    if (atCap) {
      toast?.warning(t('errors.busyTitle'), {description: t('errors.busyBody', {count: MAX_CONCURRENT_STREAMS})})
      return false
    }
    onSend(trimmed, taggedFiles, excludeContext)
    resetComposer()
    closeExpanded()
    return true
  }, [
    value,
    taggedFiles,
    isDisabled,
    sendBlocked,
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
    if ((!trimmed && taggedFiles.length === 0) || isDisabled || sendBlocked || !onPreview) return
    onPreview(trimmed, taggedFiles, excludeContext, resetComposer)
  }, [value, taggedFiles, isDisabled, sendBlocked, onPreview, excludeContext, resetComposer])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (isImeKeydown(e)) return
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
        if (historyIndex < history.length - 1) {
          if (historyIndex === -1) historyDraftRef.current = value
          const next = historyIndex + 1
          setHistoryIndex(next)
          setValue(history[next] ?? '')
          e.preventDefault()
        }
      } else if (e.key === 'ArrowDown' && historyIndex >= 0) {
        const next = historyIndex - 1
        setHistoryIndex(next)
        if (next >= 0) {
          setValue(history[next] ?? '')
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

  const syncMenus = useCallback((val: string, pos: number) => {
    const textBefore = val.slice(0, pos)
    const lastSpace = textBefore.lastIndexOf(' ')

    const lastAt = textBefore.lastIndexOf('@')
    if (lastAt !== -1 && lastAt > lastSpace) {
      setAutocompleteQuery(textBefore.slice(lastAt + 1))
      setSlashQuery(null)
      setSlashMatches(0)
      return
    }
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
    setHistoryIndex(-1)
    historyDraftRef.current = null
    syncMenus(val, e.target.selectionStart)
  }

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

    const newValue = value.slice(0, atIdx) + value.slice(pos)
    setValue(newValue)

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

        {guidance && (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 bg-semantic-warning/5 border-b border-border-subtle/50 first:rounded-t-xl text-2xs text-text-secondary">
            <p className="flex-1 min-w-40">{guidance.text}</p>
            <button
              type="button"
              onClick={guidance.onAction}
              className="shrink-0 min-h-6 font-medium text-brand-400 hover:text-brand-300 transition-colors"
            >
              {guidance.actionLabel}
            </button>
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
          className="w-full bg-transparent border-none outline-hidden text-sm leading-relaxed text-text-primary placeholder:text-text-tertiary resize-none px-3.5 pt-3 pb-1 min-h-[22px] chat-composer-input scrollbar-none font-sans"
          aria-label={t('composer.inputAria')}
          placeholder={canQueue ? t('composer.queuePlaceholder') : placeholder || t('composer.placeholder')}
          value={value}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onBlur={closeMenus}
          disabled={isDisabled}
          rows={1}
        />

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
            {onPreview && hasContent && !isDisabled && !sendBlocked && (
              <ComposerIconButton icon={Eye} label={t('composer.previewContext')} onClick={handlePreview} />
            )}
            <ComposerIconButton
              icon={Maximize2}
              label={t('composer.expandEditor')}
              onClick={() => setIsExpanded(true)}
            />
            <SendButton
              mode={streaming ? 'stop' : 'send'}
              disabled={!hasContent || isDisabled || atCap || sendBlocked}
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
          canSend={hasContent && !isDisabled && !sendBlocked && (canQueue || !atCap)}
          menus={menus}
          onInsertTrigger={insertTrigger}
          tokenLabel={hasContent ? t('composer.tokenEstimate', {count: tokenEstimate}) : undefined}
          tokenClass={tokenClass}
        />
      )}
    </div>
  )
}
