import clsx from 'clsx'

type SwitchProps = {
  checked: boolean
  onChange: (checked: boolean) => void
  label?: string
  disabled?: boolean
  className?: string
}

export default function Switch({checked, onChange, label, disabled = false, className}: SwitchProps) {
  return (
    <label
      className={clsx(
        'inline-flex items-center gap-2 cursor-pointer select-none',
        disabled && 'opacity-50 cursor-not-allowed',
        className,
      )}
    >
      <button
        role="switch"
        aria-checked={checked}
        disabled={disabled}
        onClick={() => !disabled && onChange(!checked)}
        className={clsx(
          'relative w-8 h-[18px] rounded-full transition-colors duration-fast flex-shrink-0',
          checked ? 'bg-brand-500' : 'bg-surface-4',
        )}
      >
        <span
          className={clsx(
            'absolute top-[2px] w-[14px] h-[14px] rounded-full shadow-xs transition-transform duration-fast',
            checked ? 'left-[14px] bg-brand-foreground' : 'left-[2px] bg-white',
          )}
        />
      </button>
      {label && <span className="text-sm text-text-primary">{label}</span>}
    </label>
  )
}
