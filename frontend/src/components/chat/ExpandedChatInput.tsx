import {useTranslation} from 'react-i18next'
import {useEffect} from 'react'
import clsx from 'clsx'
import {ArrowUp, AtSign, Eye, Slash} from 'lucide-react'
import {Button, Modal} from '@/components/shared'
import {ComposerChip, ComposerHints, ContextToggle} from './ComposerControls'

interface Props {
  value: string
  textareaRef: React.RefObject<HTMLTextAreaElement | null>
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
  onInsertTrigger: (char: '@' | '/') => void
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
            {menus}
          </div>
        </div>
      </div>
    </Modal>
  )
}
