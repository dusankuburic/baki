import {useTranslation} from 'react-i18next'
import {useEffect} from 'react'
import {Send, Eye} from 'lucide-react'
import {Button, Checkbox, Modal} from '@/components/shared'

interface Props {
  value: string
  textareaRef: React.RefObject<HTMLTextAreaElement>
  // The composer's own handlers, so the expanded editor is a different SHELL
  // around identical behaviour rather than a second implementation: @-mentions,
  // /commands, Enter-to-send and ArrowUp history all keep working here.
  onChange: (e: React.ChangeEvent<HTMLTextAreaElement>) => void
  onKeyDown: (e: React.KeyboardEvent) => void
  onSend: () => void
  onPreview?: () => void
  onClose: () => void
  excludeContext: boolean
  onExcludeContextChange: (val: boolean) => void
  canSend: boolean
  menus: React.ReactNode
}

export default function ExpandedChatInput({
  value,
  textareaRef,
  onChange,
  onKeyDown,
  onSend,
  onPreview,
  onClose,
  excludeContext,
  onExcludeContextChange,
  canSend,
  menus,
}: Props) {
  const {t} = useTranslation('chat')

  // Modal's focus trap puts focus on the first tabbable element; the editor
  // wants the caret in the textarea, at the end of the existing draft.
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
      footer={
        <>
          <label className="mr-auto flex items-center gap-2 cursor-pointer select-none">
            <Checkbox checked={excludeContext} onChange={onExcludeContextChange} />
            <span className="text-sm text-text-secondary">{t('compose.excludeContext')}</span>
          </label>
          {onPreview && (
            <Button variant="secondary" size="sm" onClick={onPreview}>
              <Eye size={14} className="mr-1.5" />
              {t('compose.preview')}
            </Button>
          )}
          <Button
            variant="primary"
            size="sm"
            disabled={!canSend}
            onClick={() => {
              onSend()
              onClose()
            }}
          >
            <Send size={14} className="mr-1.5" />
            {t('compose.send')}
          </Button>
        </>
      }
    >
      <div className="relative flex flex-col h-full">
        <p className="text-xs text-text-tertiary mb-3 shrink-0">{t('compose.hint')}</p>
        <textarea
          ref={textareaRef}
          className="flex-1 w-full min-h-0 bg-transparent text-text-primary placeholder:text-text-tertiary resize-none focus:outline-none text-base leading-relaxed font-sans custom-scrollbar"
          placeholder={t('compose.placeholder')}
          value={value}
          onChange={onChange}
          onKeyDown={onKeyDown}
          aria-label={t('composer.inputAria')}
        />
        {/* The menus anchor to the bottom of the editor area, matching the
            inline composer's placement. */}
        {menus}
      </div>
    </Modal>
  )
}
