import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {createRef} from 'react'
import Select from './Select'
import FormField from './FormField'

const PURPOSES = [
  {value: 'general', label: 'General'},
  {value: 'documentation', label: 'Documentation'},
  {value: 'custom', label: 'Custom…', disabled: true},
]

describe('Select', () => {
  it('is a native select over its options, labelled through FormField', () => {
    const onChange = vi.fn()
    render(
      <FormField id="purpose" label="Purpose">
        <Select id="purpose" value="general" onChange={e => onChange(e.target.value)} options={PURPOSES} />
      </FormField>,
    )
    const select = screen.getByRole('combobox', {name: 'Purpose'})
    expect(select.tagName).toBe('SELECT')
    expect(screen.getAllByRole('option').map(o => o.textContent)).toEqual(['General', 'Documentation', 'Custom…'])
    expect(screen.getByRole('option', {name: 'Custom…'})).toBeDisabled()

    fireEvent.change(select, {target: {value: 'documentation'}})
    expect(onChange).toHaveBeenCalledWith('documentation')
  })

  it('shows an unselectable placeholder while the value is empty', () => {
    render(<Select aria-label="Resource" value="" onChange={() => {}} placeholder="Choose a resource" options={[]} />)
    const placeholder = screen.getByRole('option', {name: 'Choose a resource'})
    expect(placeholder).toBeDisabled()
    expect(placeholder).toHaveValue('')
  })

  it('takes <option> children instead of options', () => {
    render(
      <Select aria-label="Provider" defaultValue="b">
        <option value="a">Alpha</option>
        <option value="b">Beta</option>
      </Select>,
    )
    expect(screen.getByRole('combobox', {name: 'Provider'})).toHaveValue('b')
  })

  it('links its hint, and marks an error invalid and links it instead', () => {
    const {rerender} = render(
      <Select aria-label="Purpose" value="general" onChange={() => {}} options={PURPOSES} hint="Groups the picker" />,
    )
    const select = screen.getByRole('combobox', {name: 'Purpose'})
    expect(select).toHaveAccessibleDescription('Groups the picker')
    expect(select).not.toHaveAttribute('aria-invalid')

    rerender(
      <Select
        aria-label="Purpose"
        value="general"
        onChange={() => {}}
        options={PURPOSES}
        hint="Groups the picker"
        error="Pick a purpose"
      />,
    )
    expect(select).toHaveAttribute('aria-invalid', 'true')
    expect(select).toHaveAccessibleDescription('Pick a purpose')
  })

  it("keeps the caller's own aria-describedby alongside its message", () => {
    render(
      <>
        <p id="extra">Shared with the whole organisation</p>
        <Select
          aria-label="Purpose"
          aria-describedby="extra"
          value="general"
          onChange={() => {}}
          options={PURPOSES}
          error="Pick a purpose"
        />
      </>,
    )
    expect(screen.getByRole('combobox', {name: 'Purpose'})).toHaveAccessibleDescription(
      'Shared with the whole organisation Pick a purpose',
    )
  })

  it('forwards its ref to the <select>', () => {
    const ref = createRef<HTMLSelectElement>()
    render(<Select ref={ref} aria-label="Purpose" options={PURPOSES} defaultValue="general" />)
    expect(ref.current).toBe(screen.getByRole('combobox', {name: 'Purpose'}))
  })
})
