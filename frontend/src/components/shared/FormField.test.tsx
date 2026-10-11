import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import FormField, {formFieldIds} from './FormField'
import Input from './Input'
import SegmentedControl from './SegmentedControl'

describe('FormField', () => {
  it('labels its control through the id', () => {
    render(
      <FormField id="endpoint" label="Endpoint">
        <Input id="endpoint" value="" onChange={() => {}} />
      </FormField>,
    )
    expect(screen.getByLabelText('Endpoint')).toHaveAttribute('id', 'endpoint')
  })

  it('renders the markup of the Field it replaces when given only id, label and children', () => {
    const {container} = render(
      <FormField id="name" label="Name">
        <Input id="name" value="" onChange={() => {}} />
      </FormField>,
    )
    const field = container.firstElementChild as HTMLElement
    expect(field).toHaveClass('space-y-1')
    const label = field.firstElementChild as HTMLElement
    expect(label.tagName).toBe('LABEL')
    expect(label).toHaveAttribute('for', 'name')
    expect(label).toHaveClass('block', 'text-xs', 'font-medium', 'text-text-secondary')
    expect(field.querySelectorAll('p')).toHaveLength(0)
  })

  it('gives a control that has no hint of its own a label, a hint and an error to point at', () => {
    const {labelId, hintId, errorId} = formFieldIds('kind')
    render(
      <FormField id="kind" label="Kind" hint="Agents need a project endpoint" error="Choose a kind">
        <SegmentedControl
          value="chat"
          onChange={() => {}}
          options={[
            {value: 'chat', label: 'Chat'},
            {value: 'agent', label: 'Agent', disabled: true},
          ]}
          labelledBy={labelId}
          describedBy={`${hintId} ${errorId}`}
        />
      </FormField>,
    )
    const group = screen.getByRole('radiogroup', {name: 'Kind'})
    expect(group).toHaveAccessibleDescription('Agents need a project endpoint Choose a kind')
  })

  it('puts the aside at the end of the label row', () => {
    render(
      <FormField id="description" label="Description" aside="12 / 200">
        <textarea id="description" />
      </FormField>,
    )
    const aside = screen.getByText('12 / 200')
    expect(aside.parentElement).toContainElement(screen.getByText('Description'))
    expect(screen.getByLabelText('Description').tagName).toBe('TEXTAREA')
  })
})
