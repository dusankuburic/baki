import clsx from 'clsx'

type CheckboxProps = {
  checked: boolean
  onChange: (checked: boolean) => void
  label?: string
  disabled?: boolean
  className?: string
}

export default function Checkbox({checked, onChange, label, disabled = false, className}: CheckboxProps) {
  return (
    <label
      className={clsx(
        'inline-flex items-center gap-2 cursor-pointer select-none',
        disabled && 'opacity-50 cursor-not-allowed',
        className,
      )}
    >
      <button
        role="checkbox"
        aria-checked={checked}
        disabled={disabled}
        onClick={() => !disabled && onChange(!checked)}
        className={clsx(
          'w-4 h-4 rounded-sm border flex items-center justify-center transition-colors duration-fast flex-shrink-0',
          checked ? 'bg-brand-500 border-brand-500' : 'bg-surface-2 border-border-default hover:border-border-strong',
        )}
      >
        {checked && (
          <svg width="10" height="10" viewBox="0 0 10 10" fill="none" className="text-brand-foreground">
            <path
              d="M2 5L4 7L8 3"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        )}
      </button>
      {label && <span className="text-sm text-text-primary">{label}</span>}
    </label>
  )
}
