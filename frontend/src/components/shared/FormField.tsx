import clsx from 'clsx'

// The ids FormField gives its parts, derived from the control's id, so a
// control that does not wire its own description (a SegmentedControl, a
// custom widget) can point aria-labelledby / aria-describedby at them.
export function formFieldIds(id: string) {
  return {labelId: `${id}-label`, hintId: `${id}-hint`, errorId: `${id}-error`}
}

type FormFieldProps = {
  /** The control's id: the label's htmlFor, and the prefix of formFieldIds. */
  id: string
  /** Visible label. Every field has one. */
  label: React.ReactNode
  children: React.ReactNode
  /**
   * Help under the control, for a control that does not render its own
   * (Input, Textarea and Select take `hint` and `error` themselves — pass
   * them there instead). Id: formFieldIds(id).hintId.
   */
  hint?: React.ReactNode
  /**
   * An error under the control (below the hint, which stays), same rule as
   * `hint`. Id: formFieldIds(id).errorId. Point the control's
   * aria-describedby at both ids and set its aria-invalid.
   */
  error?: React.ReactNode
  /** At the end of the label row: a character counter, an "Optional" note. */
  aside?: React.ReactNode
  className?: string
}

// A labelled form field: the label above its control, optional help or error
// below. Promoted from the Azure AI Foundry settings form, whose private
// Field this reproduces (same markup and classes) when only id, label and
// children are passed.
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
