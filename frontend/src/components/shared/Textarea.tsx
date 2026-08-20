import {useId} from 'react'
import clsx from 'clsx'

type TextareaProps = React.TextareaHTMLAttributes<HTMLTextAreaElement> & {
  error?: string
  hint?: string
}

// Mirrors shared/Input: error/hint linked via aria-describedby, invalid
// fields carry aria-invalid (WCAG 3.3.1/3.3.2).
export default function Textarea({error, hint, className, id, ...rest}: TextareaProps) {
  const fallbackId = useId()
  const describedBy = error || hint ? `${id ?? fallbackId}-desc` : undefined
  return (
    <div className={clsx('flex flex-col', className)}>
      <textarea
        id={id ?? fallbackId}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy}
        className={clsx(
          'bg-surface-2 border rounded-md px-3 py-2 text-sm outline-none resize-y min-h-[80px] transition-colors duration-fast',
          'text-text-primary placeholder:text-text-disabled',
          error
            ? 'border-semantic-error focus:border-semantic-error focus:ring-2 focus:ring-semantic-error/20'
            : 'border-border-default focus:border-focus focus:ring-2 focus:ring-brand-500/20',
        )}
        {...rest}
      />
      {(hint || error) && (
        <span id={describedBy} className={clsx('mt-1 text-xs', error ? 'text-semantic-error' : 'text-text-tertiary')}>
          {error || hint}
        </span>
      )}
    </div>
  )
}
