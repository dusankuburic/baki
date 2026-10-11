import {useCallback, useState} from 'react'

export type FieldErrors<F extends string> = Partial<Record<F, string>>

export interface FormValidationState {
  submitted: boolean
}

export interface UseFormValidationOptions<F extends string> {
  fields: readonly F[]
  validate: (state: FormValidationState) => FieldErrors<F>
}

export interface UseFormValidation<F extends string> {
  errors: FieldErrors<F>
  firstInvalid: F | null
  errorFor: (field: F) => string | undefined
  fieldProps: (field: F) => {error?: string; onBlur: () => void}
  touch: (field: F) => void
  submitValidate: () => F | null
  reset: () => void
}

function errorIn<F extends string>(errors: FieldErrors<F>, field: F): string | undefined {
  return Object.prototype.hasOwnProperty.call(errors, field) ? errors[field] : undefined
}

export function useFormValidation<F extends string>({
  fields,
  validate,
}: UseFormValidationOptions<F>): UseFormValidation<F> {
  const [touched, setTouched] = useState<ReadonlySet<F>>(() => new Set())
  const [submitted, setSubmitted] = useState(false)

  const errors = validate({submitted})
  const firstInvalid = fields.find(f => errorIn(errors, f)) ?? null
  const errorFor = (field: F): string | undefined =>
    submitted || touched.has(field) ? errorIn(errors, field) : undefined
  const touch = useCallback((field: F) => {
    setTouched(prev => (prev.has(field) ? prev : new Set(prev).add(field)))
  }, [])
  const fieldProps = (field: F) => ({
    error: errorFor(field),
    onBlur: () => touch(field),
  })
  const submitValidate = (): F | null => {
    setSubmitted(true)
    const errs = validate({submitted: true})
    return fields.find(f => errorIn(errs, f)) ?? null
  }
  const reset = useCallback(() => {
    setTouched(new Set())
    setSubmitted(false)
  }, [])

  return {errors, firstInvalid, errorFor, fieldProps, touch, submitValidate, reset}
}
