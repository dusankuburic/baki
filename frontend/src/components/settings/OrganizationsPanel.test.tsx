import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import OrganizationsPanel from './OrganizationsPanel'
import {useOrgStore, type Organisation} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {ToastProvider, ConfirmProvider} from '@/components/shared'
import {providersApi} from '@/api'

// The active org's shared Azure AI Foundry section reads the org route on
// mount; without a mock the real client would fail the GET and render its
// load-error row into every test here.
vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      ...actual.providersApi,
      getOrgAzureFoundryConfig: vi.fn(),
      saveOrgAzureFoundryConfig: vi.fn(),
      deleteOrgAzureFoundryConfig: vi.fn(),
      testOrgAzureFoundryConnection: vi.fn(),
    },
  }
})

const azureApi = vi.mocked(providersApi)

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
const renameOrg = vi.fn().mockResolvedValue(undefined)

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
    renameOrg,
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
  renameOrg.mockResolvedValue(undefined)
  removeMember.mockResolvedValue(undefined)
  useAuthStore.setState({user: {id: 'u-admin', email: 'admin@acme.io', role: 'member'} as never})
  // Pending by default: the section stays a skeleton, so its Save/Remove
  // buttons cannot collide with the panel's own in the cases that do not
  // exercise it. The Azure cases below resolve it explicitly.
  azureApi.getOrgAzureFoundryConfig.mockReturnValue(new Promise(() => {}))
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

  it('renames the org for an admin', async () => {
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Rename organization'}))
    const input = screen.getByLabelText('New organization name')
    fireEvent.change(input, {target: {value: '  Acme Holdings '}})
    fireEvent.keyDown(input, {key: 'Enter'})

    await waitFor(() => expect(renameOrg).toHaveBeenCalledWith('org-1', 'Acme Holdings'))
    // Typing into the editor must not toggle the org row it sits in.
    expect(setActiveOrg).not.toHaveBeenCalled()
  })

  it('mounts the shared Azure AI Foundry section for the active org, editable by an admin', async () => {
    azureApi.getOrgAzureFoundryConfig.mockResolvedValue({configured: false})
    renderPanel()
    expect(await screen.findByText('Shared Azure AI Foundry')).toBeInTheDocument()
    expect(screen.getByText(/own Azure AI Foundry credentials take precedence/)).toBeInTheDocument()
    await waitFor(() => expect(azureApi.getOrgAzureFoundryConfig).toHaveBeenCalledWith('org-1'))
    // Unconfigured + admin: the form is offered.
    expect(await screen.findByRole('textbox', {name: 'Azure endpoint'})).toBeInTheDocument()
  })

  it('does not mount the shared Azure AI Foundry section without an active org', async () => {
    useOrgStore.setState({activeOrgId: null} as never)
    renderPanel()
    expect(await screen.findByText('Acme')).toBeInTheDocument()
    expect(screen.queryByText('Shared Azure AI Foundry')).toBeNull()
    expect(azureApi.getOrgAzureFoundryConfig).not.toHaveBeenCalled()
  })

  it('treats an unchanged name as a cancel', async () => {
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Rename organization'}))
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    await waitFor(() => expect(screen.queryByLabelText('New organization name')).not.toBeInTheDocument())
    expect(renameOrg).not.toHaveBeenCalled()
  })

  describe('as a non-admin member', () => {
    beforeEach(() => {
      useAuthStore.setState({user: {id: 'u-2', email: 'bob@acme.io', role: 'member'} as never})
    })

    it('offers neither rename nor member removal', async () => {
      renderPanel()
      await screen.findByText('Acme')
      expect(screen.queryByRole('button', {name: 'Rename organization'})).toBeNull()
      expect(screen.queryByLabelText('Remove member')).toBeNull()
    })

    it('shows the shared Azure AI Foundry section read-only', async () => {
      azureApi.getOrgAzureFoundryConfig.mockResolvedValue({
        configured: true,
        endpoint: 'https://acme.openai.azure.com',
        authMode: 'api_key',
        hasSecret: true,
        chatDeployment: 'gpt-4.1',
        shared: true,
        updatedBy: 'admin@acme.io',
      })
      renderPanel()
      expect(await screen.findByText('https://acme.openai.azure.com')).toBeInTheDocument()
      expect(screen.getByText('Shared by your organisation · set by admin@acme.io')).toBeInTheDocument()
      expect(screen.queryByRole('textbox', {name: 'Azure endpoint'})).toBeNull()
      for (const name of ['Save', 'Edit', 'Test']) {
        expect(screen.queryByRole('button', {name})).toBeNull()
      }
    })

    // Members had no way out of an org short of asking an admin, although the
    // server has always allowed removing your own membership.
    it('leaves the org via their own row after confirmation', async () => {
      renderPanel()
      const leave = await screen.findAllByRole('button', {name: 'Leave organization'})
      // Only the caller's own row carries the control.
      expect(leave).toHaveLength(1)
      fireEvent.click(leave[0])
      fireEvent.click(await screen.findByRole('button', {name: 'Leave'}))

      await waitFor(() => expect(removeMember).toHaveBeenCalledWith('org-1', 'u-2'))
    })
  })
})
