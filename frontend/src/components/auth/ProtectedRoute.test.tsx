import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {StrictMode} from 'react'
import {render, screen, waitFor} from '@testing-library/react'

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
  useAuthStore.setState({
    isAuthenticated: false,
    isLoading: false,
    loadFromStorage: vi.fn().mockResolvedValue(undefined),
  })
  useOrgStore.setState({loadOrgs: vi.fn().mockResolvedValue(undefined)})
})

afterEach(() => setHash(''))

describe('ProtectedRoute recovery deep links (StrictMode)', () => {
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
