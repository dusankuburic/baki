import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import {StatusRegion, TurnAnnouncer} from './StatusRegion'

// Review of R-frontend-5: role=status is implicitly aria-atomic, so a notice and a turn line in one region re-read
// each other. They are two regions now.
describe('StatusRegion and TurnAnnouncer', () => {
  function Regions({notice, line}: {notice: string; line: {text: string; seq: number}}) {
    return (
      <>
        <StatusRegion text={notice} />
        <TurnAnnouncer line={line} />
      </>
    )
  }

  it('a notice change leaves the answer line untouched, and never carries it', () => {
    const line = {text: 'Answer: Yes.', seq: 1}
    const {rerender} = render(<Regions notice="The model is gone." line={line} />)
    expect(screen.getAllByRole('status')).toHaveLength(2)
    const turn = screen.getByTestId('ai-turn-region')
    const mutations: MutationRecord[] = []
    const mo = new MutationObserver(r => mutations.push(...r))
    mo.observe(turn, {subtree: true, childList: true, characterData: true})

    rerender(<Regions notice="The model is back." line={line} />)
    mo.disconnect()
    expect(mutations).toEqual([])
    expect(screen.getByTestId('ai-status-region')).toHaveTextContent('The model is back.')
    expect(screen.getByTestId('ai-status-region').textContent).not.toContain('Answer')
  })

  it('reinserts the line for a new announcement with the same text', () => {
    const {rerender} = render(<TurnAnnouncer line={{text: 'Answer: Yes.', seq: 1}} />)
    const first = screen.getByText('Answer: Yes.')
    rerender(<TurnAnnouncer line={{text: 'Answer: Yes.', seq: 2}} />)
    expect(screen.getByText('Answer: Yes.')).not.toBe(first)
  })
})
