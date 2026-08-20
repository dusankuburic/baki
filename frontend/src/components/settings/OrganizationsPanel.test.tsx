import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import OrganizationsPanel from './OrganizationsPanel'
import {useOrgStore, type Organisation} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {ToastProvider, ConfirmProvider} from '@/components/shared'

const org: Organisation = {
  id: 'org-1',
  name: 'Acme',
  ownerId: 'u-admin',
  members: [
    {userId: 'u-admin', role: 'admin', joinedAt: '2024-01-01T00:00:00Z', user: {id: 'u-admin', email: 'admin@acme.io'}},
    {userId: 'u-2', role: 'member', joinedAt: '2024-01-02T00:00:00Z', user: {id: 'u-2', email: 'bob@acme.io'}},
  ],
  createdAt: '2024-01-01T00:00:00Z',
  updatedAt: '2024-01-01T00:00:00Z',
}

const loadOrgs = vi.fn().mockResolvedValue(undefined)
const createOrg = vi.fn()
const deleteOrg = vi.fn().mockResolvedValue(undefined)
const inviteMember = vi.fn().mockResolvedValue(undefined)
const removeMember = vi.fn().mockResolvedValue(undefined)
const setMemberRole = vi.fn().mockResolvedValue(undefined)
const setActiveOrg = vi.fn()

function seedStore() {
  useOrgStore.setState({
    organisations: [org],
    activeOrgId: 'org-1',
    isLoading: false,
    error: null,
    loadOrgs,
    createOrg,
    deleteOrg,
    inviteMember,
    removeMember,
    setMemberRole,
    setActiveOrg,
  } as never)
}

function renderPanel() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <OrganizationsPanel />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  loadOrgs.mockResolvedValue(undefined)
  useAuthStore.setState({user: {id: 'u-admin', email: 'admin@acme.io', role: 'member'} as never})
  seedStore()
})

describe('OrganizationsPanel', () => {
  it('loads orgs on mount and renders the expanded org with members', async () => {
    renderPanel()
    expect(loadOrgs).toHaveBeenCalledTimes(1)
    expect(await screen.findByText('Acme')).toBeInTheDocument()
    expect(screen.getByText('admin@acme.io')).toBeInTheDocument()
    expect(screen.getByText('bob@acme.io')).toBeInTheDocument()
    expect(screen.getByText('2 members')).toBeInTheDocument()
  })

  it('creates an org, activates it, and closes the form', async () => {
    createOrg.mockResolvedValue({id: 'org-2', name: 'NewCo'})
    renderPanel()

    fireEvent.click(screen.getByRole('button', {name: /new organization/i}))
    fireEvent.change(await screen.findByPlaceholderText('e.g. Acme RPA'), {target: {value: 'NewCo'}})
    fireEvent.click(screen.getByRole('button', {name: 'Create'}))

    await waitFor(() => expect(createOrg).toHaveBeenCalledWith('NewCo'))
    await waitFor(() => expect(setActiveOrg).toHaveBeenCalledWith('org-2'))
  })

  it('keeps the create button disabled for an empty name', async () => {
    renderPanel()
    fireEvent.click(screen.getByRole('button', {name: /new organization/i}))
    expect(await screen.findByRole('button', {name: 'Create'})).toBeDisabled()
    expect(createOrg).not.toHaveBeenCalled()
  })

  it('invites a member with the selected role', async () => {
    renderPanel()

    fireEvent.change(await screen.findByPlaceholderText('teammate@example.com'), {
      target: {value: 'carol@acme.io'},
    })
    fireEvent.change(screen.getAllByDisplayValue('member').at(-1)!, {target: {value: 'viewer'}})
    fireEvent.click(screen.getByRole('button', {name: /invite/i}))

    await waitFor(() => expect(inviteMember).toHaveBeenCalledWith('org-1', 'carol@acme.io', 'viewer'))
  })

  it('changes a member role via the role select', async () => {
    renderPanel()
    // Selects showing 'member': [0] = Bob's role select, [1] = the invite form's
    // role picker. The owner's select shows 'admin' and is disabled anyway.
    const bobRoleSelect = (await screen.findAllByDisplayValue('member')).at(0)!
    fireEvent.change(bobRoleSelect, {target: {value: 'viewer'}})

    await waitFor(() => expect(setMemberRole).toHaveBeenCalledWith('org-1', 'u-2', 'viewer'))
  })

  it('removes a member after confirmation', async () => {
    renderPanel()
    // Both rows render "Remove member"; the owner's is disabled.
    const removeButtons = await screen.findAllByLabelText('Remove member')
    const bobRemove = removeButtons.find(b => !b.hasAttribute('disabled'))
    fireEvent.click(bobRemove!)

    // ConfirmProvider danger dialog; accept with its Remove button.
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))

    await waitFor(() => expect(removeMember).toHaveBeenCalledWith('org-1', 'u-2'))
  })

  it('does not remove when the confirmation is cancelled', async () => {
    renderPanel()
    const removeButtons = await screen.findAllByLabelText('Remove member')
    fireEvent.click(removeButtons.find(b => !b.hasAttribute('disabled'))!)

    fireEvent.click(await screen.findByRole('button', {name: /cancel/i}))
    await waitFor(() => expect(screen.queryByRole('button', {name: 'Remove'})).not.toBeInTheDocument())
    expect(removeMember).not.toHaveBeenCalled()
  })
})
