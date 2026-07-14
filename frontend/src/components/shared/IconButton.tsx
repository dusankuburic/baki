import {type LucideIcon} from 'lucide-react'
import clsx from 'clsx'
import Tooltip from './Tooltip'

type IconButtonProps = {
  icon: LucideIcon
  variant?: 'ghost' | 'solid' | 'outline'
  size?: 'sm' | 'md' | 'lg'
  label: string
  active?: boolean
  disabled?: boolean
  onClick?: () => void
  className?: string
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

export default function IconButton({
  icon: Icon,
  variant = 'ghost',
  size = 'md',
  label,
  active = false,
  disabled = false,
  onClick,
  className,
}: IconButtonProps) {
  const button = (
    <button
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
    >
      <Icon size={iconSizes[size]} />
    </button>
  )

  return <Tooltip content={label}>{button}</Tooltip>
}
