import { describe, it, expect, vi, beforeEach } from 'vitest'
import { useOrgStore, type Organisation } from './orgStore'

vi.mock('@/api/client', () => ({
  request: vi.fn(),
}))

import { request } from '@/api/client'

const mockRequest = request as ReturnType<typeof vi.fn>

const fakeOrg: Organisation = {
  id: 'org-1',
  name: 'Acme Corp',
  ownerId: 'u1',
  members: [{ userId: 'u1', role: 'admin', joinedAt: '2024-01-01T00:00:00Z' }],
  createdAt: '2024-01-01T00:00:00Z',
  updatedAt: '2024-01-01T00:00:00Z',
}

const initialState = useOrgStore.getState()

beforeEach(() => {
  useOrgStore.setState(initialState, true)
  vi.resetAllMocks()
})

// ---- loadOrgs ----

describe('loadOrgs', () => {
  it('populates organisations on success', async () => {
    mockRequest.mockResolvedValue([fakeOrg])

    await useOrgStore.getState().loadOrgs()

    expect(useOrgStore.getState().organisations).toHaveLength(1)
    expect(useOrgStore.getState().organisations[0].name).toBe('Acme Corp')
  })

  it('sets error on failure', async () => {
    mockRequest.mockRejectedValue(new Error('network'))

    await useOrgStore.getState().loadOrgs()

    expect(useOrgStore.getState().error).toBeTruthy()
    expect(useOrgStore.getState().isLoading).toBe(false)
  })

  it('handles null response gracefully', async () => {
    mockRequest.mockResolvedValue(null)

    await useOrgStore.getState().loadOrgs()

    expect(useOrgStore.getState().organisations).toEqual([])
  })

  it('keeps activeOrgId when the org is still in the list', async () => {
    mockRequest.mockResolvedValue([fakeOrg])
    useOrgStore.setState({ activeOrgId: 'org-1' })

    await useOrgStore.getState().loadOrgs()

    expect(useOrgStore.getState().activeOrgId).toBe('org-1')
  })

  it('resets a stale activeOrgId (org deleted or user removed from it)', async () => {
    mockRequest.mockResolvedValue([fakeOrg])
    useOrgStore.setState({ activeOrgId: 'org-gone' })

    await useOrgStore.getState().loadOrgs()

    expect(useOrgStore.getState().activeOrgId).toBeNull()
  })

  it('resets activeOrgId when the user has no orgs at all', async () => {
    mockRequest.mockResolvedValue(null)
    useOrgStore.setState({ activeOrgId: 'org-1' })

    await useOrgStore.getState().loadOrgs()

    expect(useOrgStore.getState().activeOrgId).toBeNull()
  })

  it('keeps activeOrgId on load failure', async () => {
    mockRequest.mockRejectedValue(new Error('network'))
    useOrgStore.setState({ activeOrgId: 'org-1' })

    await useOrgStore.getState().loadOrgs()

    expect(useOrgStore.getState().activeOrgId).toBe('org-1')
  })
})

// ---- activeOrgId persistence ----

describe('activeOrgId persistence', () => {
  it('persists activeOrgId to localStorage under baki-active-org', () => {
    useOrgStore.getState().setActiveOrg('org-1')

    const raw = localStorage.getItem('baki-active-org')
    expect(raw).toBeTruthy()
    expect(JSON.parse(raw!).state.activeOrgId).toBe('org-1')
  })

  it('persists null when switching back to Personal', () => {
    useOrgStore.getState().setActiveOrg('org-1')
    useOrgStore.getState().setActiveOrg(null)

    const raw = localStorage.getItem('baki-active-org')
    expect(JSON.parse(raw!).state.activeOrgId).toBeNull()
  })

  it('does not persist the org list itself', () => {
    useOrgStore.setState({ organisations: [fakeOrg] })
    useOrgStore.getState().setActiveOrg('org-1')

    const raw = localStorage.getItem('baki-active-org')
    expect(JSON.parse(raw!).state.organisations).toBeUndefined()
  })
})

// ---- createOrg ----

describe('createOrg', () => {
  it('appends the new org to the list', async () => {
    mockRequest.mockResolvedValue(fakeOrg)

    const created = await useOrgStore.getState().createOrg('Acme Corp')

    expect(created).toEqual(fakeOrg)
    expect(useOrgStore.getState().organisations).toContainEqual(fakeOrg)
  })
})

// ---- setActiveOrg ----

describe('setActiveOrg', () => {
  it('sets the activeOrgId', () => {
    useOrgStore.getState().setActiveOrg('org-1')
    expect(useOrgStore.getState().activeOrgId).toBe('org-1')
  })

  it('can be cleared to null', () => {
    useOrgStore.setState({ activeOrgId: 'org-1' })
    useOrgStore.getState().setActiveOrg(null)
    expect(useOrgStore.getState().activeOrgId).toBeNull()
  })
})

// ---- deleteOrg ----

describe('deleteOrg', () => {
  it('removes the org from the list', async () => {
    useOrgStore.setState({ organisations: [fakeOrg], activeOrgId: null })
    mockRequest.mockResolvedValue(undefined)

    await useOrgStore.getState().deleteOrg('org-1')

    expect(useOrgStore.getState().organisations).toHaveLength(0)
  })

  it('clears activeOrgId if the deleted org was active', async () => {
    useOrgStore.setState({ organisations: [fakeOrg], activeOrgId: 'org-1' })
    mockRequest.mockResolvedValue(undefined)

    await useOrgStore.getState().deleteOrg('org-1')

    expect(useOrgStore.getState().activeOrgId).toBeNull()
  })

  it('preserves activeOrgId when a different org is deleted', async () => {
    const other: Organisation = { ...fakeOrg, id: 'org-2', name: 'Other' }
    useOrgStore.setState({ organisations: [fakeOrg, other], activeOrgId: 'org-1' })
    mockRequest.mockResolvedValue(undefined)

    await useOrgStore.getState().deleteOrg('org-2')

    expect(useOrgStore.getState().activeOrgId).toBe('org-1')
  })
})

// ---- inviteMember (token invite) ----

describe('inviteMember', () => {
  // Regression: must hit the invites endpoint (which emails a token), not the
  // direct-add /members endpoint — otherwise the invite email never fires.
  it('POSTs to the org invites endpoint', async () => {
    mockRequest.mockResolvedValue(undefined)

    await useOrgStore.getState().inviteMember('org-1', 'new@example.com', 'member')

    expect(mockRequest).toHaveBeenCalledWith('/api/orgs/org-1/invites', { email: 'new@example.com', role: 'member' })
  })
})

// ---- acceptInvite ----

describe('acceptInvite', () => {
  it('accepts the token, reloads orgs, and activates the joined org', async () => {
    const joined: Organisation = { ...fakeOrg, id: 'org-9', name: 'Joined' }
    // 1st call: accept -> returns the org; 2nd call: loadOrgs -> returns list.
    mockRequest.mockResolvedValueOnce(joined).mockResolvedValueOnce([joined])

    await useOrgStore.getState().acceptInvite('tok-abc')

    expect(mockRequest).toHaveBeenNthCalledWith(1, '/api/invites/tok-abc/accept', {})
    expect(useOrgStore.getState().activeOrgId).toBe('org-9')
  })

  it('sets an error when the token is invalid', async () => {
    mockRequest.mockRejectedValue(new Error('invite not found'))

    await expect(useOrgStore.getState().acceptInvite('bad')).rejects.toThrow()
    expect(useOrgStore.getState().error).toBe('invite not found')
  })
})

// ---- setMemberRole ----

describe('setMemberRole', () => {
  // Regression: the role route is registered PUT-only (routes_chi.go). Sending
  // the default POST returned 405 and member-role changes silently failed.
  it('sends PUT to the member role endpoint', async () => {
    mockRequest.mockResolvedValue(undefined)

    await useOrgStore.getState().setMemberRole('org-1', 'u2', 'admin')

    expect(mockRequest).toHaveBeenCalledWith('/api/orgs/org-1/members/u2/role', { role: 'admin' }, 'PUT')
  })
})

// ---- clearError ----

describe('clearError', () => {
  it('resets the error field', () => {
    useOrgStore.setState({ error: 'oops' })
    useOrgStore.getState().clearError()
    expect(useOrgStore.getState().error).toBeNull()
  })
})
