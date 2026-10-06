import {useTranslation} from 'react-i18next'
import {useEffect} from 'react'
import clsx from 'clsx'
import {ArrowUp, AtSign, Eye, Slash} from 'lucide-react'
import {Button, Modal} from '@/components/shared'
import {ComposerChip, ComposerHints, ContextToggle} from './ComposerControls'

interface Props {
  value: string
  textareaRef: React.RefObject<HTMLTextAreaElement | null>
  // The composer's own handlers, so the expanded editor is a different SHELL
  // around identical behaviour rather than a second implementation: @-mentions,
  // /commands, Enter-to-send and ArrowUp history all keep working here.
  onChange: (e: React.ChangeEvent<HTMLTextAreaElement>) => void
  onKeyDown: (e: React.KeyboardEvent) => void
  onBlur?: () => void
  onSend: () => void
  onPreview?: () => void
  onClose: () => void
  excludeContext: boolean
  onExcludeContextChange: (val: boolean) => void
  canSend: boolean
  menus: React.ReactNode
  // The toolbar chips insert "@" / "/" at the caret through the composer.
  onInsertTrigger: (char: '@' | '/') => void
  // The composer's live "~N tokens" estimate; empty while there is no text.
  tokenLabel?: string
  tokenClass?: string
}

export default function ExpandedChatInput({
  value,
  textareaRef,
  onChange,
  onKeyDown,
  onBlur,
  onSend,
  onPreview,
  onClose,
  excludeContext,
  onExcludeContextChange,
  canSend,
  menus,
  onInsertTrigger,
  tokenLabel,
  tokenClass,
}: Props) {
  const {t} = useTranslation('chat')

  // The editor wants the caret in the textarea, at the end of the existing
  // draft. The Modal is told to focus it too (initialFocusRef): its default
  // is the first tabbable element — the title bar's Close button — and it
  // focuses a frame AFTER this effect, so the editor used to open with focus
  // on Close (typing went nowhere; Space or Enter closed it).
  useEffect(() => {
    const el = textareaRef.current
    if (!el) return
    el.focus()
    el.setSelectionRange(el.value.length, el.value.length)
  }, [textareaRef])

  return (
    <Modal
      isOpen
      onClose={onClose}
      title={t('compose.title')}
      size="xl"
      height="tall"
      // The body is a fixed flex column that never scrolls itself: the editor
      // well scrolls, and the @ / slash menus (positioned above the toolbar)
      // are no longer clipped by a scrolling body.
      bodyScroll={false}
      initialFocusRef={textareaRef}
      footer={
        <>
          <ComposerHints className="mr-auto" />
          {onPreview && (
            <Button variant="secondary" size="md" icon={Eye} onClick={onPreview}>
              {t('compose.preview')}
            </Button>
          )}
          <Button
            variant="primary"
            size="md"
            icon={ArrowUp}
            disabled={!canSend}
            // The composer closes the editor itself once it has sent or
            // queued; a refused send (concurrency cap) keeps the editor and
            // its text.
            onClick={() => onSend()}
          >
            {t('compose.send')}
          </Button>
        </>
      }
    >
      <div className="flex flex-col h-full p-6">
        <div
          className={clsx(
            'relative flex-1 min-h-0 flex flex-col rounded-lg border border-border-default bg-surface-2',
            'transition-[border-color,box-shadow] duration-fast',
            'focus-within:border-brand-500/60 focus-within:ring-2 focus-within:ring-brand-500/15',
          )}
        >
          <textarea
            ref={textareaRef}
            className="flex-1 min-h-0 w-full bg-transparent px-4 py-3 rounded-t-lg text-base leading-relaxed text-text-primary placeholder:text-text-tertiary resize-none focus:outline-hidden font-sans custom-scrollbar"
            placeholder={t('compose.placeholder')}
            value={value}
            onChange={onChange}
            onKeyDown={onKeyDown}
            onBlur={onBlur}
            aria-label={t('composer.inputAria')}
          />
          <div className="relative flex items-center gap-1.5 px-2 pb-2 pt-2 border-t border-border-subtle">
            <ComposerChip
              icon={AtSign}
              label={t('composer.insertFile')}
              hint={t('composer.insertFileHint')}
              onClick={() => onInsertTrigger('@')}
            />
            <ComposerChip
              icon={Slash}
              label={t('composer.insertCommand')}
              hint={t('composer.insertCommandHint')}
              onClick={() => onInsertTrigger('/')}
            />
            <ContextToggle
              checked={excludeContext}
              onChange={onExcludeContextChange}
              label={t('compose.excludeContext')}
            />
            {tokenLabel && <span className={clsx('ml-auto text-2xs tabular-nums', tokenClass)}>{tokenLabel}</span>}
            {/* The menus anchor above this toolbar, inside the editor well. */}
            {menus}
          </div>
        </div>
      </div>
    </Modal>
  )
}
