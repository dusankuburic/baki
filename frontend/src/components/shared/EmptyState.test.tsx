import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import EmptyState from './EmptyState'

describe('EmptyState', () => {
  it('uses an h3 by default and the level it is given', () => {
    const {rerender} = render(<EmptyState title="No models yet" />)
    expect(screen.getByRole('heading', {name: 'No models yet', level: 3})).toBeInTheDocument()

    rerender(<EmptyState title="No models yet" headingLevel={5} />)
    expect(screen.getByRole('heading', {name: 'No models yet', level: 5})).toBeInTheDocument()
  })

  it('renders its children between the description and the action', () => {
    render(
      <EmptyState
        title="Connect an Azure AI Foundry resource"
        description="You need:"
        action={<button>Add resource</button>}
      >
        <ul>
          <li>An endpoint</li>
        </ul>
      </EmptyState>,
    )
    const description = screen.getByText('You need:')
    const list = screen.getByRole('list')
    const action = screen.getByRole('button', {name: 'Add resource'})
    expect(description.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(list.compareDocumentPosition(action) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })
})
