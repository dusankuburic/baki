import {describe, it, expect} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useFormValidation} from './useFormValidation'

type Field = 'email' | 'url' | 'days'

function setup(errors: {email?: string; url?: string; days?: string}) {
  const fields = ['email', 'url', 'days'] as const satisfies readonly Field[]
  return renderHook(() =>
    useFormValidation<Field>({
      fields,
      validate: () => errors,
    }),
  )
}

describe('useFormValidation', () => {
  it('hides every error until the field is touched or the form submitted', () => {
    const {result} = setup({email: 'bad email', url: 'bad url'})
    expect(result.current.errorFor('email')).toBeUndefined()
    expect(result.current.errorFor('url')).toBeUndefined()

    act(() => result.current.touch('email'))
    expect(result.current.errorFor('email')).toBe('bad email')
    expect(result.current.errorFor('url')).toBeUndefined()
    expect(result.current.errors.url).toBe('bad url')
  })

  it('fieldProps wires the revealed error and reveals on blur', () => {
    const {result} = setup({days: 'not a number'})
    expect(result.current.fieldProps('days').error).toBeUndefined()

    act(() => result.current.fieldProps('days').onBlur())
    expect(result.current.fieldProps('days').error).toBe('not a number')
  })

  it('firstInvalid follows the declared field order, not object order', () => {
    const {result} = setup({days: 'x', email: 'y'})
    expect(result.current.firstInvalid).toBe('email')
  })

  it('submitValidate flips to submitted, reveals everything and names the first invalid field', () => {
    const bad = setup({days: 'x', email: 'y'})
    let first: string | null = null
    act(() => {
      first = bad.result.current.submitValidate()
    })
    expect(first).toBe('email')
    expect(bad.result.current.errorFor('email')).toBe('y')
    expect(bad.result.current.errorFor('days')).toBe('x')

    const ok = setup({})
    let okFirst: string | null = 'sentinel'
    act(() => {
      okFirst = ok.result.current.submitValidate()
    })
    expect(okFirst).toBeNull()
  })

  it('reset returns to pristine: touched and submitted are cleared', () => {
    const {result} = setup({email: 'bad email'})
    act(() => {
      result.current.touch('email')
    })
    act(() => {
      result.current.submitValidate()
    })
    expect(result.current.errorFor('email')).toBe('bad email')

    act(() => {
      result.current.reset()
    })
    expect(result.current.errorFor('email')).toBeUndefined()
  })

  it('reads errors by own key only — a field named `constructor` finds nothing through the prototype', () => {
    const fields = ['key', 'constructor'] as const satisfies readonly string[]
    const {result} = renderHook(() =>
      useFormValidation<string>({fields, validate: () => ({key: 'required'})}),
    )
    expect(result.current.errorFor('constructor')).toBeUndefined()
    expect(result.current.firstInvalid).toBe('key')
    let first: string | null = 'sentinel'
    act(() => {
      first = result.current.submitValidate()
    })
    expect(first).toBe('key')
  })
})
