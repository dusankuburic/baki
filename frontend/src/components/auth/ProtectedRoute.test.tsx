import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {StrictMode} from 'react'
import {render, screen, waitFor} from '@testing-library/react'

// Force the web (non-desktop) path so the auth gate / recovery views run.
vi.mock('@/platform/guards', () => ({
  isTauri: () => false,
  isWeb: () => true,
}))

import ProtectedRoute from './ProtectedRoute'
import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'

function setHash(h: string) {
  window.history.replaceState(null, '', '/' + (h ? '#' + h : ''))
}

beforeEach(() => {
  // Logged-out, not loading; stub side-effecting loaders.
  useAuthStore.setState({
    isAuthenticated: false,
    isLoading: false,
    loadFromStorage: vi.fn().mockResolvedValue(undefined),
  })
  useOrgStore.setState({loadOrgs: vi.fn().mockResolvedValue(undefined)})
})

afterEach(() => setHash(''))

describe('ProtectedRoute recovery deep links (StrictMode)', () => {
  // Regression: the recovery hash was read in a useState initializer that also
  // stripped the fragment. Under StrictMode the initializer runs twice, so the
  // 2nd run saw an empty hash and the reset view never rendered. Rendering under
  // <StrictMode> here reproduces that double-invocation.
  it('renders the reset view from #resetPassword and then clears the hash', async () => {
    setHash('resetPassword=tok-123')
    render(
      <StrictMode>
        <ProtectedRoute>
          <div>app-content</div>
        </ProtectedRoute>
      </StrictMode>,
    )

    expect(await screen.findByRole('heading', {name: /choose a new password/i})).toBeInTheDocument()
    expect(screen.queryByText('app-content')).not.toBeInTheDocument()
    // The single-use token is stripped from the URL by the effect.
    await waitFor(() => expect(window.location.hash).toBe(''))
  })

  it('shows the login gate when there is no recovery hash', async () => {
    setHash('')
    render(
      <StrictMode>
        <ProtectedRoute>
          <div>app-content</div>
        </ProtectedRoute>
      </StrictMode>,
    )
    expect(await screen.findByRole('heading', {name: /sign in/i})).toBeInTheDocument()
  })
})
