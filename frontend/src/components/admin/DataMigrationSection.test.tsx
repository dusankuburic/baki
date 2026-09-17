import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'
import {DataMigrationSection, migrationPhase} from './DataMigrationSection'

describe('DataMigrationSection', () => {
  it('derives the phase from the server status shape', () => {
    expect(migrationPhase(null)).toBeNull()
    expect(migrationPhase({configured: false, running: false})).toBe('not configured')
    expect(migrationPhase({configured: true, running: true})).toBe('running')
    expect(migrationPhase({configured: true, running: false, error: 'boom'})).toBe('failed')
    expect(migrationPhase({configured: true, running: false})).toBe('idle')
  })

  it('disables Start and explains PAD_DATA_DIR when migration is not configured', () => {
    render(<DataMigrationSection status={{configured: false, running: false}} isLoading={false} onStart={vi.fn()} />)
    expect(screen.getByRole('button', {name: /start migration/i})).toBeDisabled()
    expect(screen.getByText('PAD_DATA_DIR')).toBeInTheDocument()
  })

  it('lists what a completed run did not migrate', () => {
    render(
      <DataMigrationSection
        status={{
          configured: true,
          running: false,
          result: {
            FlowsMigrated: 2,
            FlowsSkipped: 0,
            FlowsFailed: 0,
            ConversationsMigrated: 1,
            ConversationsSkipped: 0,
            ConversationsFailed: 0,
            SettingsMoved: true,
            NotMigrated: ['users and sessions', 'flow versions'],
            Errors: null,
            Duration: 1e9,
          },
        }}
        isLoading={false}
        onStart={vi.fn()}
      />,
    )
    expect(screen.getByText('completed')).toBeInTheDocument()
    expect(screen.getByText('Not migrated: users and sessions, flow versions.')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: /start migration/i})).toBeEnabled()
  })
})
