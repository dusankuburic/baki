import clsx from 'clsx'

export function formFieldIds(id: string) {
  return {labelId: `${id}-label`, hintId: `${id}-hint`, errorId: `${id}-error`}
}

type FormFieldProps = {
  id: string
  label: React.ReactNode
  children: React.ReactNode
  hint?: React.ReactNode
  error?: React.ReactNode
  aside?: React.ReactNode
  className?: string
}

export default function FormField({id, label, children, hint, error, aside, className}: FormFieldProps) {
  const {labelId, hintId, errorId} = formFieldIds(id)
  const labelElement = (
    <label id={labelId} htmlFor={id} className="block text-xs font-medium text-text-secondary">
      {label}
    </label>
  )
  return (
    <div className={clsx('space-y-1', className)}>
      {aside ? (
        <div className="flex items-baseline justify-between gap-2">
          {labelElement}
          <span className="text-2xs text-text-tertiary">{aside}</span>
        </div>
      ) : (
        labelElement
      )}
      {children}
      {hint && (
        <p id={hintId} className="text-xs text-text-tertiary">
          {hint}
        </p>
      )}
      {error && (
        <p id={errorId} className="text-xs text-semantic-error">
          {error}
        </p>
      )}
    </div>
  )
}
