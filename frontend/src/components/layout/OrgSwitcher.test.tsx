import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'

const isTauriMock = vi.fn(() => false)
vi.mock('@/platform/guards', () => ({
    isTauri: () => isTauriMock(),
    isWeb: () => !isTauriMock(),
}))

vi.mock('@/api/client', () => ({
    request: vi.fn().mockResolvedValue([]),
    registerRefreshCallback: vi.fn(),
    invalidateConfigCache: vi.fn(),
    setSessionToken: vi.fn(),
}))

import OrgSwitcher from './OrgSwitcher'
import { useOrgStore, type Organisation } from '@/stores/orgStore'
import { useAuthStore } from '@/stores/authStore'

function makeOrg(id: string, name: string): Organisation {
    return {
        id,
        name,
        ownerId: 'u1',
        members: [],
        createdAt: '2024-01-01T00:00:00Z',
        updatedAt: '2024-01-01T00:00:00Z',
    }
}

const initialOrgState = useOrgStore.getState()

beforeEach(() => {
    isTauriMock.mockReturnValue(false)
    useOrgStore.setState(initialOrgState, true)
    useAuthStore.setState({ isAuthenticated: true })
})

describe('OrgSwitcher', () => {
    it('defaults to Personal when no org is active', () => {
        render(<OrgSwitcher />)
        expect(screen.getByText('Personal')).toBeInTheDocument()
    })

    it('shows the active org name in the trigger', () => {
        useOrgStore.setState({
            organisations: [makeOrg('org-1', 'Acme Corp')],
            activeOrgId: 'org-1',
        })
        render(<OrgSwitcher />)
        expect(screen.getByText('Acme Corp')).toBeInTheDocument()
    })

    it('switches the active org when an item is selected', () => {
        useOrgStore.setState({
            organisations: [makeOrg('org-1', 'Acme Corp')],
            activeOrgId: null,
        })
        render(<OrgSwitcher />)

        fireEvent.click(screen.getByTitle('Switch organization'))
        fireEvent.click(screen.getByText('Acme Corp'))

        expect(useOrgStore.getState().activeOrgId).toBe('org-1')
    })

    it('switches back to Personal', () => {
        useOrgStore.setState({
            organisations: [makeOrg('org-1', 'Acme Corp')],
            activeOrgId: 'org-1',
        })
        render(<OrgSwitcher />)

        fireEvent.click(screen.getByTitle('Switch organization'))
        fireEvent.click(screen.getByText('Personal'))

        expect(useOrgStore.getState().activeOrgId).toBeNull()
    })

    it('renders nothing in Tauri (desktop) mode', () => {
        isTauriMock.mockReturnValue(true)
        const { container } = render(<OrgSwitcher />)
        expect(container.firstChild).toBeNull()
    })

    it('renders nothing when not authenticated', () => {
        useAuthStore.setState({ isAuthenticated: false })
        const { container } = render(<OrgSwitcher />)
        expect(container.firstChild).toBeNull()
    })
})
