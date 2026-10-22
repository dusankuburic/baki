import {useId} from 'react'
import {type LucideIcon} from 'lucide-react'
import clsx from 'clsx'

type InputProps = React.InputHTMLAttributes<HTMLInputElement> & {
  icon?: LucideIcon
  trailingIcon?: LucideIcon
  onTrailingClick?: () => void
  trailingLabel?: string
  trailingPressed?: boolean
  error?: string
  hint?: string
}

export default function Input({
  icon: Icon,
  trailingIcon: TrailingIcon,
  onTrailingClick,
  trailingLabel,
  trailingPressed,
  error,
  hint,
  className,
  id,
  ...rest
}: InputProps) {
  const fallbackId = useId()
  const describedBy = error || hint ? `${id ?? fallbackId}-desc` : undefined
  return (
    <div className={clsx('flex flex-col', className)}>
      <div
        className={clsx(
          'flex items-center h-9 bg-surface-2 border rounded-md px-3 transition-colors duration-fast',
          error
            ? 'border-semantic-error focus-within:border-semantic-error'
            : 'border-border-default focus-within:border-border-focus',
        )}
      >
        {Icon && <Icon size={14} className="text-text-tertiary mr-2 shrink-0" />}
        <input
          id={id ?? fallbackId}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy}
          className="bg-transparent text-sm w-full border-none outline-none focus:outline-none focus-visible:outline-none ring-0 focus:ring-0 focus-visible:ring-0 shadow-none text-text-primary placeholder:text-text-disabled"
          style={{outline: 'none', border: 'none', boxShadow: 'none', ...rest.style}}
          {...rest}
        />
        {TrailingIcon && (
          <button
            type="button"
            onClick={onTrailingClick}
            className="ml-2 rounded-sm text-text-tertiary hover:text-text-secondary transition-colors duration-fast focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40"
            tabIndex={trailingLabel ? 0 : -1}
            aria-label={trailingLabel}
            aria-pressed={trailingLabel ? trailingPressed : undefined}
          >
            <TrailingIcon size={14} />
          </button>
        )}
      </div>
      {(hint || error) && (
        <span id={describedBy} className={clsx('mt-1 text-xs', error ? 'text-semantic-error' : 'text-text-tertiary')}>
          {error || hint}
        </span>
      )}
    </div>
  )
}
