import {type LucideIcon} from 'lucide-react'
import clsx from 'clsx'
import Spinner from './Spinner'

type ButtonProps = {
    variant?: 'primary' | 'secondary' | 'ghost' | 'danger'
    size?: 'sm' | 'md' | 'lg'
    loading?: boolean
    disabled?: boolean
    icon?: LucideIcon
    iconPosition?: 'left' | 'right'
    fullWidth?: boolean
    children: React.ReactNode
    onClick?: () => void
} & Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, 'onClick'>

const variantClasses: Record<string, string> = {
    primary: 'bg-brand-500 hover:bg-brand-600 active:bg-brand-700 text-white shadow-sm',
    secondary: 'bg-surface-2 hover:bg-surface-3 text-text-primary border border-border-default',
    ghost: 'bg-transparent hover:bg-surface-2 text-text-secondary hover:text-text-primary',
    danger: 'bg-semantic-error/15 hover:bg-semantic-error/25 text-semantic-error border border-semantic-error/30',
}

const sizeClasses: Record<string, string> = {
    sm: 'h-7 px-[10px] text-xs gap-1',
    md: 'h-8 px-[14px] text-sm gap-2',
    lg: 'h-10 px-[18px] text-base gap-2',
}

export default function Button({
    variant = 'secondary',
    size = 'md',
    loading = false,
    disabled = false,
    icon: Icon,
    iconPosition = 'left',
    fullWidth = false,
    children,
    onClick,
    className,
    ...rest
}: ButtonProps) {
    const isDisabled = disabled || loading

    return (
        <button
            className={clsx(
                'inline-flex items-center justify-center rounded-md font-medium transition-colors duration-fast select-none',
                'focus-visible:ring-2 focus-visible:ring-brand-500/40 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-0',
                'active:shadow-[inset_0_1px_2px_rgba(0,0,0,0.1)]',
                variantClasses[variant],
                sizeClasses[size],
                fullWidth && 'w-full',
                isDisabled && 'opacity-50 cursor-not-allowed pointer-events-none',
                className
            )}
            disabled={isDisabled}
            onClick={onClick}
            {...rest}
        >
            {loading && iconPosition === 'left' && (
                <Spinner size={14} />
            )}
            {!loading && Icon && iconPosition === 'left' && (
                <Icon size="1em" />
            )}
            <span className={clsx(loading && 'opacity-50')}>{children}</span>
            {!loading && Icon && iconPosition === 'right' && (
                <Icon size="1em" />
            )}
            {loading && iconPosition === 'right' && (
                <Spinner size={14} />
            )}
        </button>
    )
}
