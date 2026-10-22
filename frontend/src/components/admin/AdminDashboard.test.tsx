import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useAuthStore} from '@/stores/authStore'
import {useAdminStore} from '@/stores/adminStore'

const listUsers = vi.fn()
const userStats = vi.fn()
const getMigrationStatus = vi.fn()
const listAuditEvents = vi.fn()
const listAuditActions = vi.fn()
const systemHealth = vi.fn()

vi.mock('@/api/admin', () => ({
  adminApi: {
    listUsers: (...a: unknown[]) => listUsers(...a),
    userStats: (...a: unknown[]) => userStats(...a),
    getMigrationStatus: (...a: unknown[]) => getMigrationStatus(...a),
    listAuditEvents: (...a: unknown[]) => listAuditEvents(...a),
    listAuditActions: (...a: unknown[]) => listAuditActions(...a),
    systemHealth: (...a: unknown[]) => systemHealth(...a),
    getDeploymentAISettings: vi
      .fn()
      .mockResolvedValue({dailyBudget: 0, perUserDailyBudget: 0, embeddingProvider: '', embeddingModel: ''}),
    updateDeploymentAISettings: vi.fn().mockResolvedValue(undefined),
    setUserRole: vi.fn().mockResolvedValue(undefined),
    startMigration: vi.fn().mockResolvedValue(undefined),
    triggerScannerScan: vi.fn().mockResolvedValue({started: true}),
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
    useAdminStore.getState().reset()
    useAuthStore.setState({
      user: {id: 'admin1', email: 'admin@example.com', role: 'admin', displayName: 'Admin'},
    })
    listUsers.mockResolvedValue({
      items: [
        {
          id: 'u1',
          email: 'alice@example.com',
          role: 'admin',
          displayName: 'Alice',
          avatarUrl: '',
          emailVerified: true,
          createdAt: '2026-09-01T00:00:00Z',
          updatedAt: '2026-09-01T00:00:00Z',
        },
        {
          id: 'u2',
          email: 'bob@example.com',
          role: 'member',
          displayName: 'Bob',
          avatarUrl: '',
          emailVerified: false,
          createdAt: '2026-09-02T00:00:00Z',
          updatedAt: '2026-09-02T00:00:00Z',
        },
      ],
      total: 2,
      offset: 0,
      limit: 10,
    })
    userStats.mockResolvedValue({
      total: 2,
      byRole: {admin: 1, member: 1, viewer: 0, guest: 0, none: 0},
      unverified: 1,
      locked: 0,
    })
    getMigrationStatus.mockResolvedValue({configured: false, running: false})
    listAuditEvents.mockResolvedValue({events: []})
    listAuditActions.mockResolvedValue([])
    systemHealth.mockResolvedValue({
      database: {status: 'ok'},
      blob: {status: 'ok'},
      redis: {status: 'skipped'},
      overall: 'ok',
    })
  })

  it('renders admin dashboard for admin user', async () => {
    renderDashboard()
    expect(screen.getByRole('heading', {level: 1, name: 'Admin Dashboard'})).toBeInTheDocument()
    await waitFor(() => {
      const content = document.body.textContent ?? ''
      expect(content).toMatch(/user|audit|migration/i)
    })
    expect(await screen.findByText('alice@example.com')).toBeInTheDocument()
    expect(screen.getByRole('navigation', {name: 'Admin sections'})).toBeInTheDocument()
    expect(screen.getByRole('heading', {level: 2, name: 'Audit log'})).toBeInTheDocument()
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
    expect(listUsers).not.toHaveBeenCalled()
  })

  it('fetches users and migration status on mount', async () => {
    renderDashboard()
    await waitFor(() => {
      expect(listUsers).toHaveBeenCalled()
      expect(getMigrationStatus).toHaveBeenCalled()
    })
    expect(listUsers).toHaveBeenCalledWith(expect.objectContaining({limit: 10, offset: 0}), expect.anything())
    expect(userStats).toHaveBeenCalled()
    expect(systemHealth).toHaveBeenCalled()
    expect(listAuditEvents).toHaveBeenCalledWith(expect.objectContaining({limit: 16, offset: 0}), expect.anything())
  })

  it('reloads every section, health included, from the header Refresh', async () => {
    renderDashboard()
    await waitFor(() => expect(systemHealth).toHaveBeenCalledTimes(1))
    await screen.findByText('alice@example.com')
    fireEvent.click(screen.getByRole('button', {name: 'Refresh'}))
    await waitFor(() => {
      expect(listUsers).toHaveBeenCalledTimes(2)
      expect(userStats).toHaveBeenCalledTimes(2)
      expect(getMigrationStatus).toHaveBeenCalledTimes(2)
      expect(listAuditEvents).toHaveBeenCalledTimes(2)
      expect(systemHealth).toHaveBeenCalledTimes(2)
    })
  })
})
