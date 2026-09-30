import {forwardRef, useId} from 'react'
import clsx from 'clsx'

export type SelectOption = {value: string; label: string; disabled?: boolean}

type SelectProps = Omit<React.SelectHTMLAttributes<HTMLSelectElement>, 'children'> & {
  /** The choices; or pass <option>/<optgroup> elements as children. */
  options?: readonly SelectOption[]
  children?: React.ReactNode
  /** A first, unselectable option shown while the value is ''. */
  placeholder?: string
  error?: string
  hint?: string
}

// The native <select>: the platform's own picker, keyboard and screen-reader
// support, styled like shared/Input. Like Input it links its error or hint
// through aria-describedby and marks an error with aria-invalid (WCAG
// 3.3.1/3.3.2); a caller's own aria-describedby is kept alongside. The label
// is the caller's: wrap it in FormField, or pass aria-label.
const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  {options, children, placeholder, error, hint, className, id, 'aria-describedby': describedBy, ...rest},
  ref,
) {
  const fallbackId = useId()
  const selectId = id ?? fallbackId
  const messageId = error || hint ? `${selectId}-desc` : undefined
  const describedByIds = [describedBy, messageId].filter(Boolean).join(' ') || undefined
  return (
    <div className={clsx('flex flex-col', className)}>
      <select
        ref={ref}
        id={selectId}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedByIds}
        className={clsx(
          'h-9 w-full rounded-md border bg-surface-2 px-2 text-sm text-text-primary outline-none transition-colors duration-fast',
          'disabled:cursor-not-allowed disabled:opacity-60',
          error
            ? 'border-semantic-error focus:border-semantic-error'
            : 'border-border-default focus:border-border-focus',
        )}
        {...rest}
      >
        {placeholder !== undefined && (
          <option value="" disabled>
            {placeholder}
          </option>
        )}
        {options
          ? options.map(o => (
              <option key={o.value} value={o.value} disabled={o.disabled}>
                {o.label}
              </option>
            ))
          : children}
      </select>
      {messageId && (
        <span id={messageId} className={clsx('mt-1 text-xs', error ? 'text-semantic-error' : 'text-text-tertiary')}>
          {error || hint}
        </span>
      )}
    </div>
  )
})

export default Select
