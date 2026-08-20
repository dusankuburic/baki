import {useId} from 'react'
import {type LucideIcon} from 'lucide-react'
import clsx from 'clsx'

type InputProps = React.InputHTMLAttributes<HTMLInputElement> & {
  icon?: LucideIcon
  trailingIcon?: LucideIcon
  onTrailingClick?: () => void
  error?: string
  hint?: string
}

// The error/hint text is programmatically linked to the field via a generated
// id + aria-describedby, and invalid fields carry aria-invalid — screen
// readers announce the message when the field is focused (WCAG 3.3.1/3.3.2).
export default function Input({
  icon: Icon,
  trailingIcon: TrailingIcon,
  onTrailingClick,
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
            ? 'border-semantic-error focus-within:border-semantic-error focus-within:ring-2 focus-within:ring-semantic-error/20'
            : 'border-border-default focus-within:border-focus focus-within:ring-2 focus-within:ring-brand-500/20',
        )}
      >
        {Icon && <Icon size={14} className="text-text-tertiary mr-2 flex-shrink-0" />}
        <input
          id={id ?? fallbackId}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy}
          className="bg-transparent text-sm w-full outline-none text-text-primary placeholder:text-text-disabled"
          {...rest}
        />
        {TrailingIcon && (
          <button
            type="button"
            onClick={onTrailingClick}
            className="ml-2 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
            tabIndex={-1}
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
