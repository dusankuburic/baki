import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {ArrowUp, EyeOff, Square, type LucideIcon} from 'lucide-react'
import {Kbd} from '@/components/shared'

const keepFocus = (e: React.MouseEvent) => e.preventDefault()

const focusRing = 'focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40'

const chipClasses =
  'relative inline-flex items-center gap-1.5 h-6 px-2 rounded-md border border-border-default text-2xs font-medium ' +
  'text-text-secondary whitespace-nowrap select-none transition-colors duration-fast'
const chipHover = 'hover:text-text-primary hover:bg-surface-3 hover:border-border-strong'

const roundButton = 'inline-flex items-center justify-center w-7 h-7 shrink-0 transition-colors duration-fast'

interface ChipProps {
  icon: LucideIcon
  label: string
  hint: string
  onClick: () => void
  labelClass?: string
}

export function ComposerChip({icon: Icon, label, hint, onClick, labelClass}: ChipProps) {
  return (
    <button
      type="button"
      onMouseDown={keepFocus}
      onClick={onClick}
      title={hint}
      className={clsx(chipClasses, chipHover, focusRing)}
    >
      <Icon size={12} aria-hidden="true" />
      <span className={labelClass}>{label}</span>
    </button>
  )
}

interface ToggleProps {
  checked: boolean
  onChange: (checked: boolean) => void
  label: string
  labelClass?: string
}

export function ContextToggle({checked, onChange, label, labelClass}: ToggleProps) {
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={checked}
      onMouseDown={keepFocus}
      onClick={() => onChange(!checked)}
      className={clsx(
        chipClasses,
        focusRing,
        checked ? 'bg-brand-500/10 text-brand-400 border-brand-500/40 hover:bg-brand-500/15' : chipHover,
      )}
    >
      <EyeOff size={12} aria-hidden="true" />
      <span className={labelClass}>{label}</span>
    </button>
  )
}

interface IconButtonProps {
  icon: LucideIcon
  label: string
  onClick: () => void
}

export function ComposerIconButton({icon: Icon, label, onClick}: IconButtonProps) {
  return (
    <button
      type="button"
      onMouseDown={keepFocus}
      onClick={onClick}
      aria-label={label}
      title={label}
      className={clsx(
        roundButton,
        'rounded-lg text-text-tertiary hover:text-text-primary hover:bg-surface-3',
        focusRing,
      )}
    >
      <Icon size={16} aria-hidden="true" />
    </button>
  )
}

interface SendButtonProps {
  mode: 'send' | 'stop'
  disabled?: boolean
  title?: string
  onClick?: () => void
}

export function SendButton({mode, disabled = false, title, onClick}: SendButtonProps) {
  const {t} = useTranslation('chat')
  if (mode === 'stop') {
    const stop = t('composer.stopGeneration')
    return (
      <button
        type="button"
        onMouseDown={keepFocus}
        onClick={onClick}
        aria-label={stop}
        title={stop}
        className={clsx(
          roundButton,
          'rounded-full bg-semantic-error/10 text-semantic-error border border-semantic-error/30 hover:bg-semantic-error/20',
          focusRing,
        )}
      >
        <Square size={12} fill="currentColor" aria-hidden="true" />
      </button>
    )
  }
  const send = t('composer.sendMessage')
  return (
    <button
      type="button"
      onMouseDown={keepFocus}
      onClick={onClick}
      disabled={disabled}
      aria-label={send}
      title={title ?? send}
      className={clsx(
        roundButton,
        'rounded-full',
        disabled
          ? 'bg-surface-3 text-text-disabled cursor-not-allowed'
          : 'bg-brand-500 text-brand-foreground shadow-sm hover:bg-brand-600 active:bg-brand-700',
        focusRing,
      )}
    >
      <ArrowUp size={16} strokeWidth={2.5} aria-hidden="true" />
    </button>
  )
}

export function ComposerHints({className}: {className?: string}) {
  const {t} = useTranslation('chat')
  return (
    <span className={clsx('inline-flex items-center gap-1.5 text-2xs text-text-tertiary whitespace-nowrap', className)}>
      <Kbd keys={['Enter']} />
      <span>{t('composer.hintSend')}</span>
      <span aria-hidden="true">·</span>
      <Kbd keys={['Shift', 'Enter']} />
      <span>{t('composer.hintNewline')}</span>
    </span>
  )
}
