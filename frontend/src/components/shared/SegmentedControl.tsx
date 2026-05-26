import clsx from 'clsx'
import {type LucideIcon} from 'lucide-react'

type SegmentedControlProps<T> = {
    value: T
    onChange: (value: T) => void
    options: {value: T; label: string; icon?: LucideIcon}[]
    size?: 'sm' | 'md'
    className?: string
}

export default function SegmentedControl<T extends string>({
    value,
    onChange,
    options,
    size = 'md',
    className,
}: SegmentedControlProps<T>) {
    return (
        <div
            className={clsx(
                'inline-flex bg-surface-2 border border-border-default rounded-md p-0.5 gap-0.5',
                className
            )}
            role="radiogroup"
        >
            {options.map(opt => {
                const Icon = opt.icon
                const isActive = opt.value === value
                return (
                    <button
                        key={opt.value}
                        className={clsx(
                            'px-3 rounded-sm cursor-pointer transition-colors duration-fast text-sm',
                            size === 'sm' ? 'h-6' : 'h-7',
                            isActive
                                ? 'bg-surface-3 text-text-primary shadow-xs'
                                : 'text-text-secondary hover:text-text-primary'
                        )}
                        onClick={() => onChange(opt.value)}
                        role="radio"
                        aria-checked={isActive}
                    >
                        <span className="inline-flex items-center gap-1.5">
                            {Icon && <Icon size={14} />}
                            {opt.label}
                        </span>
                    </button>
                )
            })}
        </div>
    )
}
