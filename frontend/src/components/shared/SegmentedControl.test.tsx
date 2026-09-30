import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {useState} from 'react'
import SegmentedControl, {type SegmentedOption} from './SegmentedControl'

type Kind = 'chat' | 'claude' | 'agent' | 'embedding'

const KINDS: SegmentedOption<Kind>[] = [
  {value: 'chat', label: 'Chat'},
  {value: 'claude', label: 'Claude'},
  {value: 'agent', label: 'Agent'},
  {value: 'embedding', label: 'Embedding'},
]

// A controlled group, the way every consumer uses it.
function Harness({
  onChange = () => {},
  options = KINDS,
  initial = 'chat',
  activation,
  disabled,
}: {
  onChange?: (value: Kind) => void
  options?: SegmentedOption<Kind>[]
  initial?: Kind
  activation?: 'auto' | 'manual'
  disabled?: boolean
}) {
  const [value, setValue] = useState<Kind>(initial)
  return (
    <SegmentedControl
      label="Kind"
      value={value}
      onChange={next => {
        setValue(next)
        onChange(next)
      }}
      options={options}
      activation={activation}
      disabled={disabled}
    />
  )
}

const radio = (name: string) => screen.getByRole('radio', {name})
const key = (name: string, k: string) => fireEvent.keyDown(radio(name), {key: k})

describe('SegmentedControl', () => {
  it('is a radio group of plain buttons with a single tab stop on the selected option', () => {
    render(<Harness initial="claude" />)
    expect(screen.getByRole('radiogroup', {name: 'Kind'})).toBeInTheDocument()
    for (const opt of KINDS) expect(radio(opt.label)).toHaveAttribute('type', 'button')
    expect(radio('Claude')).toHaveAttribute('aria-checked', 'true')
    expect(radio('Claude')).toHaveAttribute('tabindex', '0')
    for (const name of ['Chat', 'Agent', 'Embedding']) expect(radio(name)).toHaveAttribute('tabindex', '-1')
  })

  // A <button> without a type is a submit button: picking an option inside a
  // form used to submit it.
  it('does not submit the form it sits in', () => {
    const submit = vi.fn((e: React.FormEvent) => e.preventDefault())
    render(
      <form onSubmit={submit}>
        <Harness />
      </form>,
    )
    fireEvent.click(radio('Claude'))
    expect(submit).not.toHaveBeenCalled()
    expect(radio('Claude')).toHaveAttribute('aria-checked', 'true')
  })

  it('auto activation: the arrows move focus AND select, wrapping at the ends; Home/End jump', () => {
    const onChange = vi.fn()
    render(<Harness onChange={onChange} />)
    radio('Chat').focus()

    key('Chat', 'ArrowRight')
    expect(document.activeElement).toBe(radio('Claude'))
    expect(radio('Claude')).toHaveAttribute('aria-checked', 'true')
    expect(onChange).toHaveBeenLastCalledWith('claude')

    key('Claude', 'ArrowDown')
    expect(radio('Agent')).toHaveAttribute('aria-checked', 'true')

    key('Agent', 'Home')
    expect(document.activeElement).toBe(radio('Chat'))
    expect(radio('Chat')).toHaveAttribute('aria-checked', 'true')

    key('Chat', 'ArrowLeft')
    expect(document.activeElement).toBe(radio('Embedding'))
    expect(radio('Embedding')).toHaveAttribute('aria-checked', 'true')

    key('Embedding', 'ArrowUp')
    expect(radio('Agent')).toHaveAttribute('aria-checked', 'true')

    key('Agent', 'End')
    expect(radio('Embedding')).toHaveAttribute('aria-checked', 'true')
    expect(onChange).toHaveBeenCalledTimes(6)
  })

  it('manual activation: the arrows only move focus; Enter or Space selects', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<Harness onChange={onChange} activation="manual" />)
    radio('Chat').focus()

    await user.keyboard('{ArrowRight}')
    expect(document.activeElement).toBe(radio('Claude'))
    expect(radio('Chat')).toHaveAttribute('aria-checked', 'true')
    expect(radio('Claude')).toHaveAttribute('aria-checked', 'false')
    expect(onChange).not.toHaveBeenCalled()

    await user.keyboard('{Enter}')
    expect(onChange).toHaveBeenLastCalledWith('claude')
    expect(radio('Claude')).toHaveAttribute('aria-checked', 'true')

    await user.keyboard('{ArrowRight}')
    await user.keyboard(' ')
    expect(onChange).toHaveBeenLastCalledWith('agent')
    expect(onChange).toHaveBeenCalledTimes(2)
  })

  // With manual activation the focused option is not the selected one; the
  // tab stop has to follow focus, or Shift+Tab would land on the selected
  // option instead of leaving the group.
  it('manual activation: the focused option holds the tab stop until focus leaves the group', async () => {
    const user = userEvent.setup()
    render(
      <>
        <Harness activation="manual" />
        <button>after</button>
      </>,
    )
    radio('Chat').focus()
    await user.keyboard('{ArrowRight}')
    expect(radio('Claude')).toHaveAttribute('tabindex', '0')
    expect(radio('Chat')).toHaveAttribute('tabindex', '-1')

    await user.tab()
    expect(document.activeElement).toBe(screen.getByRole('button', {name: 'after'}))
    expect(radio('Chat')).toHaveAttribute('tabindex', '0')
    expect(radio('Claude')).toHaveAttribute('tabindex', '-1')
  })

  it('skips a disabled option with the arrows and never selects it', () => {
    const onChange = vi.fn()
    const options = KINDS.map(o => (o.value === 'agent' ? {...o, disabled: true} : o))
    render(<Harness onChange={onChange} options={options} initial="claude" />)
    expect(radio('Agent')).toHaveAttribute('aria-disabled', 'true')

    radio('Claude').focus()
    key('Claude', 'ArrowRight')
    expect(document.activeElement).toBe(radio('Embedding'))
    key('Embedding', 'ArrowLeft')
    expect(document.activeElement).toBe(radio('Claude'))

    onChange.mockClear()
    fireEvent.click(radio('Agent'))
    expect(onChange).not.toHaveBeenCalled()
    expect(radio('Claude')).toHaveAttribute('aria-checked', 'true')
  })

  // The Model modal disables Agent when the resource cannot run agents; a
  // model that already is an agent keeps showing it, and the group stays
  // reachable from the keyboard.
  it('keeps the tab stop on a selected option that is disabled, and steps off it', () => {
    const options = KINDS.map(o => (o.value === 'agent' ? {...o, disabled: true} : o))
    render(<Harness options={options} initial="agent" />)
    expect(radio('Agent')).toHaveAttribute('tabindex', '0')
    radio('Agent').focus()
    key('Agent', 'ArrowRight')
    expect(document.activeElement).toBe(radio('Embedding'))
    expect(radio('Embedding')).toHaveAttribute('aria-checked', 'true')
  })

  it('stays read-only when the whole group is disabled', () => {
    const onChange = vi.fn()
    render(<Harness onChange={onChange} disabled />)
    for (const opt of KINDS) expect(radio(opt.label)).toHaveAttribute('aria-disabled', 'true')
    fireEvent.click(radio('Claude'))
    radio('Chat').focus()
    key('Chat', 'ArrowRight')
    expect(onChange).not.toHaveBeenCalled()
    expect(radio('Chat')).toHaveAttribute('aria-checked', 'true')
  })

  it('takes a labelling element, a description and the full width', () => {
    render(
      <>
        <span id="kind-label">Kind</span>
        <span id="kind-hint">Agents need a project endpoint</span>
        <SegmentedControl
          value="chat"
          onChange={() => {}}
          options={KINDS}
          labelledBy="kind-label"
          describedBy="kind-hint"
          fullWidth
        />
      </>,
    )
    const group = screen.getByRole('radiogroup', {name: 'Kind'})
    expect(group).toHaveAccessibleDescription('Agents need a project endpoint')
    expect(group).toHaveClass('w-full')
    expect(radio('Chat')).toHaveClass('flex-1')
  })
})
