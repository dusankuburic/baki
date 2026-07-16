import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useAuthStore} from '@/stores/authStore'

vi.mock('@/api/admin', () => ({
  adminApi: {
    getMigrationStatus: vi.fn().mockResolvedValue({status: 'idle', message: 'No migration running'}),
    listUsers: vi.fn().mockResolvedValue([
      {id: 'u1', email: 'alice@example.com', role: 'admin', displayName: 'Alice'},
      {id: 'u2', email: 'bob@example.com', role: 'member', displayName: 'Bob'},
    ]),
    listAuditEvents: vi.fn().mockResolvedValue([]),
    setUserRole: vi.fn().mockResolvedValue(undefined),
    startMigration: vi.fn().mockResolvedValue(undefined),
  },
}))

import {AdminDashboard} from './AdminDashboard'

function renderDashboard() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <AdminDashboard />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

describe('AdminDashboard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    useAuthStore.setState({
      user: {id: 'admin1', email: 'admin@example.com', role: 'admin', displayName: 'Admin'},
    })
  })

  it('renders admin dashboard for admin user', async () => {
    renderDashboard()
    // The dashboard should render the admin sections
    await waitFor(() => {
      // Check for the Users section or stat cards
      const content = document.body.textContent ?? ''
      expect(content).toMatch(/user|audit|migration/i)
    })
  })

  it('shows non-admin message for non-admin users', async () => {
    useAuthStore.setState({
      user: {id: 'viewer1', email: 'viewer@example.com', role: 'viewer', displayName: 'Viewer'},
    })
    renderDashboard()
    await waitFor(() => {
      const content = document.body.textContent ?? ''
      expect(content).toMatch(/admin|permission|forbidden/i)
    })
  })

  it('fetches users and migration status on mount', async () => {
    renderDashboard()
    const {adminApi} = await import('@/api/admin')
    await waitFor(() => {
      expect(adminApi.listUsers).toHaveBeenCalled()
      expect(adminApi.getMigrationStatus).toHaveBeenCalled()
    })
  })
})
