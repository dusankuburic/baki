import {describe, it, expect, afterEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import OfflineIndicator from './OfflineIndicator'
import {useSyncStore} from '@/stores/syncStore'

function setOnline(v: boolean) {
  Object.defineProperty(window.navigator, 'onLine', {value: v, configurable: true})
}

describe('OfflineIndicator', () => {
  afterEach(() => {
    setOnline(true)
    useSyncStore.setState({queue: [], pendingCount: 0})
  })

  it('renders nothing while online', () => {
    setOnline(true)
    const {container} = render(<OfflineIndicator />)
    expect(container).toBeEmptyDOMElement()
  })

  // The sync queue holds cursor/presence updates only — SyncManager.enqueue
  // has exactly one caller, presenceStore. The banner used to print "N
  // changes queued" over it, telling a user on flaky wifi that their edits
  // would sync on reconnect. They would not: a save while offline fails
  // outright, and an edit made offline is gone when the tab closes.
  it('says saves are disabled and never promises queued changes will sync', () => {
    setOnline(false)
    useSyncStore.setState({queue: [], pendingCount: 3})
    render(<OfflineIndicator />)
    const banner = screen.getByRole('status')
    expect(banner).toHaveTextContent(/saves are disabled/i)
    expect(banner).not.toHaveTextContent(/queued/i)
    expect(banner).not.toHaveTextContent(/will sync/i)
  })
})
