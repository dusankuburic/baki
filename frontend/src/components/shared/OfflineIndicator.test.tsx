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
