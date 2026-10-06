import {forwardRef} from 'react'
import {type LucideIcon} from 'lucide-react'
import clsx from 'clsx'
import Tooltip from './Tooltip'

// Every other <button> attribute passes through to the element — aria-*
// (Dropdown clones aria-haspopup/aria-expanded/aria-controls onto its
// trigger), data-*, id — and the ref lands on the <button>, so a caller can
// move focus back to it.
type IconButtonProps = Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, 'children' | 'onClick'> & {
  icon: LucideIcon
  variant?: 'ghost' | 'solid' | 'outline-solid'
  size?: 'sm' | 'md' | 'lg'
  label: string
  active?: boolean
  onClick?: (e: React.MouseEvent<HTMLButtonElement>) => void
}

const variantClasses: Record<string, string> = {
  ghost: 'bg-transparent hover:bg-surface-2 text-text-secondary hover:text-text-primary',
  solid: 'bg-surface-3 hover:bg-surface-4 text-text-primary',
  outline: 'bg-transparent border border-border-default hover:bg-surface-2 text-text-secondary hover:text-text-primary',
}

const sizeClasses: Record<string, string> = {
  sm: 'w-6 h-6',
  md: 'w-8 h-8',
  lg: 'w-10 h-10',
}

const iconSizes: Record<string, number> = {
  sm: 14,
  md: 16,
  lg: 20,
}

const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton(
  {
    icon: Icon,
    variant = 'ghost',
    size = 'md',
    label,
    active = false,
    disabled = false,
    onClick,
    className,
    // Not a submit button unless the caller says so (see Button).
    type = 'button',
    ...rest
  },
  ref,
) {
  const button = (
    <button
      ref={ref}
      type={type}
      className={clsx(
        'inline-flex items-center justify-center rounded-md transition-colors duration-fast select-none',
        'focus-visible:ring-2 focus-visible:ring-brand-500/40 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-0',
        variantClasses[variant],
        sizeClasses[size],
        active && 'bg-surface-3 text-text-primary',
        disabled && 'opacity-50 cursor-not-allowed pointer-events-none',
        className,
      )}
      aria-label={label}
      disabled={disabled}
      onClick={onClick}
      {...rest}
    >
      <Icon size={iconSizes[size]} aria-hidden="true" />
    </button>
  )

  return <Tooltip content={label}>{button}</Tooltip>
})

export default IconButton
