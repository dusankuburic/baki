import {describe, it, expect, vi, beforeEach, onTestFinished} from 'vitest'
import {render, screen, fireEvent, waitFor, act, within} from '@testing-library/react'
import OrganizationsPanel from './OrganizationsPanel'
import {useOrgStore, type Organisation} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {ToastProvider, ConfirmProvider} from '@/components/shared'
import {providersApi} from '@/api'
import {ApiError, PermissionDeniedError} from '@/api/client'
import {useChatStore} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'
import {resetAllStores} from '@/stores/storeRegistry'
import type {AzureConnection, AzureConnectionsListing, AzureModelEntry} from '@/types'

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {
      ...actual.providersApi,
      listAzureConnections: vi.fn(),
      listAzureModels: vi.fn(),
      createAzureConnection: vi.fn(),
      updateAzureConnection: vi.fn(),
    },
  }
})

const azureApi = vi.mocked(providersApi)
const ORG_1 = {kind: 'org', orgId: 'org-1'} as const

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

function sharedResource(over: Partial<AzureConnection> = {}): AzureConnection {
  return {
    id: 'default',
    name: 'acme-res',
    shared: true,
    legacy: true,
    config: {endpoint: 'https://acme.openai.azure.com', authMode: 'api_key', hasSecret: true},
    status: 'ready',
    canDiscover: false,
    supportsAgents: false,
    suggestDiscovery: false,
    modelCount: 1,
    updatedAt: '2026-09-28T10:00:00Z',
    sharedBy: 'admin@acme.io',
    ...over,
  }
}

const sharedModel: AzureModelEntry = {
  id: 'gpt4100000',
  modelId: 'm/gpt4100000',
  connectionId: 'default',
  connectionName: 'acme-res',
  kind: 'chat',
  deployment: 'gpt-4.1',
  target: 'gpt-4.1',
  displayName: 'GPT-4.1',
  purpose: 'general',
  isDefault: true,
  isKnowledgeEmbedding: false,
  shared: true,
  status: 'ready',
  updatedAt: '2026-09-28T10:00:00Z',
}

function listing(connections: AzureConnection[]): AzureConnectionsListing {
  return {connections, registryState: 'ok', limits: {connections: 10, models: 50}, knowledgeEmbeddingEnabled: false}
}

function seedAzure(connections: AzureConnection[] = [], models: AzureModelEntry[] = []) {
  azureApi.listAzureConnections.mockResolvedValue(listing(connections))
  azureApi.listAzureModels.mockResolvedValue({models})
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

const DISABLED_NOTE = /disabled the shared Azure AI Foundry credentials they supplied/

beforeEach(async () => {
  vi.clearAllMocks()
  await resetAllStores()
  loadOrgs.mockResolvedValue(undefined)
  renameOrg.mockResolvedValue(undefined)
  removeMember.mockResolvedValue(undefined)
  useAuthStore.setState({user: {id: 'u-admin', email: 'admin@acme.io', role: 'member'} as never})
  azureApi.listAzureConnections.mockReturnValue(new Promise(() => {}))
  azureApi.listAzureModels.mockReturnValue(new Promise(() => {}))
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

  it("offers the organisation's AI provider allowlist to its admins only", async () => {
    const {unmount} = renderPanel()
    expect(await screen.findByRole('heading', {name: 'Allowed AI providers'})).toBeInTheDocument()
    unmount()
    useAuthStore.setState({user: {id: 'u-2', email: 'bob@acme.io', role: 'member'} as never})
    renderPanel()
    await screen.findByText('Acme')
    expect(screen.queryByRole('heading', {name: 'Allowed AI providers'})).toBeNull()
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
    fireEvent.change(screen.getAllByDisplayValue('Member').at(-1)!, {target: {value: 'viewer'}})
    fireEvent.click(screen.getByRole('button', {name: /invite/i}))

    await waitFor(() => expect(inviteMember).toHaveBeenCalledWith('org-1', 'carol@acme.io', 'viewer'))
  })

  it('changes a member role via the role select', async () => {
    renderPanel()
    const bobRoleSelect = (await screen.findAllByDisplayValue('Member')).at(0)!
    fireEvent.change(bobRoleSelect, {target: {value: 'viewer'}})

    await waitFor(() => expect(setMemberRole).toHaveBeenCalledWith('org-1', 'u-2', 'viewer'))
  })

  it('removes a member after confirmation', async () => {
    renderPanel()
    const removeButtons = await screen.findAllByLabelText('Remove member')
    const bobRemove = removeButtons.find(b => !b.hasAttribute('disabled'))
    fireEvent.click(bobRemove!)

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
    expect(setActiveOrg).not.toHaveBeenCalled()
  })

  it('mounts the shared Azure AI Foundry block for the active org, editable by an admin', async () => {
    seedAzure()
    renderPanel()
    expect(await screen.findByText('Shared Azure AI Foundry')).toBeInTheDocument()
    expect(screen.getByText(/Shared models appear alongside members' own/)).toBeInTheDocument()
    await waitFor(() => expect(azureApi.listAzureConnections).toHaveBeenCalledWith(ORG_1))
    expect(
      await screen.findByRole('heading', {level: 5, name: 'Connect an Azure AI Foundry resource'}),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Add resource'})).toBeInTheDocument()
  })

  it('does not mount the shared Azure AI Foundry block without an active org', async () => {
    useOrgStore.setState({activeOrgId: null} as never)
    renderPanel()
    expect(await screen.findByText('Acme')).toBeInTheDocument()
    expect(screen.queryByText('Shared Azure AI Foundry')).toBeNull()
    expect(azureApi.listAzureConnections).not.toHaveBeenCalled()
  })

  it('treats an unchanged name as a cancel', async () => {
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Rename organization'}))
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    await waitFor(() => expect(screen.queryByLabelText('New organization name')).not.toBeInTheDocument())
    expect(renameOrg).not.toHaveBeenCalled()
  })

  it('refetches the chat providers after a shared resource is added', async () => {
    const original = useChatStore.getState().bumpProviderEpoch
    const bumpProviderEpoch = vi.fn()
    useChatStore.setState({bumpProviderEpoch})
    onTestFinished(() => useChatStore.setState({bumpProviderEpoch: original}))
    seedAzure()
    azureApi.createAzureConnection.mockResolvedValue(sharedResource({modelCount: 0}))
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'Add resource'}))
    const dialog = screen.getByRole('dialog', {name: 'Add resource'})
    fireEvent.change(within(dialog).getByRole('textbox', {name: 'Endpoint'}), {
      target: {value: 'https://acme.openai.azure.com'},
    })
    fireEvent.change(within(dialog).getByLabelText('API key', {selector: 'input'}), {target: {value: 'sk-org'}})
    expect(within(dialog).queryByRole('radio', {name: 'Azure CLI'})).toBeNull()
    fireEvent.click(within(dialog).getByRole('button', {name: 'Add resource'}))
    await waitFor(() => expect(bumpProviderEpoch).toHaveBeenCalledTimes(1))
    expect(azureApi.createAzureConnection).toHaveBeenCalledWith(ORG_1, {
      name: 'acme',
      config: {endpoint: 'https://acme.openai.azure.com', auth: {mode: 'api_key', key: 'sk-org'}},
    })
  })

  it('hides the shared Azure AI Foundry block when the deployment has no org keystore', async () => {
    azureApi.listAzureConnections.mockRejectedValue(new ApiError('no org keystore', 404, 'ORG_KEYSTORE_UNAVAILABLE'))
    azureApi.listAzureModels.mockResolvedValue({models: []})
    renderPanel()
    await waitFor(() => expect(azureApi.listAzureConnections).toHaveBeenCalled())
    await waitFor(() => expect(screen.queryByText('Shared Azure AI Foundry')).toBeNull())
    expect(screen.queryByText(/Shared models appear alongside members' own/)).toBeNull()
    expect(screen.getByText('bob@acme.io')).toBeInTheDocument()
  })

  it('shows localized permission copy when an org action answers 403', async () => {
    setMemberRole.mockImplementationOnce(async () => {
      useOrgStore.setState({error: 'forbidden'} as never)
      throw new PermissionDeniedError('forbidden')
    })
    renderPanel()
    const bobRoleSelect = (await screen.findAllByDisplayValue('Member')).at(0)!
    fireEvent.change(bobRoleSelect, {target: {value: 'viewer'}})

    const copy = /You don't have permission to do that in this organisation/
    await waitFor(() => expect(screen.getByText(copy).closest('[role="alert"]')).not.toBeNull())
    expect(screen.queryByText('forbidden')).toBeNull()
    fireEvent.click(screen.getByRole('button', {name: 'Dismiss'}))
    await waitFor(() => expect(screen.queryByText(copy)).toBeNull())
  })

  it('labels roles through i18n, and the owner with a badge', async () => {
    renderPanel()
    await screen.findByText('Acme')
    expect(screen.getAllByRole('option', {name: 'Viewer'}).length).toBeGreaterThan(0)
    expect(screen.getByText('Owner').closest('span.uppercase')).not.toBeNull()
  })

  describe('as an owner whose admin role was changed', () => {
    beforeEach(() => {
      useOrgStore.setState({
        organisations: [
          {
            ...org,
            members: [{...org.members[0], role: 'member'}, org.members[1]],
          },
        ],
      } as never)
    })

    it('offers no management controls and a read-only shared block', async () => {
      seedAzure()
      renderPanel()
      await screen.findByText('Acme')
      expect(screen.queryByRole('button', {name: 'Rename organization'})).toBeNull()
      expect(screen.queryByLabelText('Remove member')).toBeNull()
      expect(screen.queryByPlaceholderText('teammate@example.com')).toBeNull()
      expect(
        await screen.findByText('Your organisation has not shared any Azure AI Foundry models yet.'),
      ).toBeInTheDocument()
      expect(screen.queryByRole('button', {name: 'Add resource'})).toBeNull()
      expect(screen.getByText('Owner')).toBeInTheDocument()
      expect(screen.getByRole('button', {name: 'Delete organization'})).toBeInTheDocument()
    })
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

    it('shows the shared Azure AI Foundry block read-only', async () => {
      seedAzure([sharedResource()], [sharedModel])
      renderPanel()
      expect(await screen.findByRole('heading', {level: 5, name: 'acme-res'})).toBeInTheDocument()
      expect(screen.getByText(/Shared by admin@acme\.io/)).toBeInTheDocument()
      expect(screen.getByText('Only organisation admins can change these resources and models.')).toBeInTheDocument()
      expect(screen.getByText('GPT-4.1')).toBeInTheDocument()
      for (const name of [/^Add/, /^Edit/, /More actions/]) {
        expect(screen.queryByRole('button', {name})).toBeNull()
      }
    })

    it('leaves the org via their own row after confirmation', async () => {
      renderPanel()
      const leave = await screen.findAllByRole('button', {name: 'Leave organization'})
      expect(leave).toHaveLength(1)
      fireEvent.click(leave[0]!)
      fireEvent.click(await screen.findByRole('button', {name: 'Leave'}))

      await waitFor(() => expect(removeMember).toHaveBeenCalledWith('org-1', 'u-2'))
    })
  })

  it("tells a viewer the organisation's models are not theirs to use, without loading them", async () => {
    useOrgStore.setState({
      organisations: [
        {
          ...org,
          members: [
            org.members[0],
            {userId: 'u-3', role: 'viewer', joinedAt: '2024-01-03T00:00:00Z', user: {id: 'u-3', email: 'vi@acme.io'}},
          ],
        },
      ],
    } as never)
    useAuthStore.setState({user: {id: 'u-3', email: 'vi@acme.io', role: 'member'} as never})
    renderPanel()
    expect(
      await screen.findByText(/Shared Azure AI Foundry models aren't available to viewers and guests/),
    ).toBeInTheDocument()
    expect(azureApi.listAzureConnections).not.toHaveBeenCalled()
  })

  it('explains and refetches the shared Azure block when a role change disables a credential', async () => {
    seedAzure()
    setMemberRole.mockResolvedValueOnce({sharedCredentialDisabled: true})
    renderPanel()
    await waitFor(() => expect(azureApi.listAzureConnections).toHaveBeenCalledTimes(1))
    await screen.findByText('Connect an Azure AI Foundry resource')

    const bobRoleSelect = (await screen.findAllByDisplayValue('Member')).at(0)!
    fireEvent.change(bobRoleSelect, {target: {value: 'viewer'}})
    await waitFor(() => expect(setMemberRole).toHaveBeenCalledWith('org-1', 'u-2', 'viewer'))

    expect(await screen.findByText(DISABLED_NOTE)).toBeInTheDocument()
    await waitFor(() => expect(azureApi.listAzureConnections).toHaveBeenCalledTimes(2))
  })

  it('shows the disabled-credential note only in its own org, and refetches the chat providers', async () => {
    const original = useChatStore.getState().bumpProviderEpoch
    const bumpProviderEpoch = vi.fn()
    useChatStore.setState({bumpProviderEpoch})
    onTestFinished(() => useChatStore.setState({bumpProviderEpoch: original}))
    const orgB: Organisation = {...org, id: 'org-2', name: 'Globex'}
    useOrgStore.setState({organisations: [org, orgB]} as never)
    seedAzure()
    setMemberRole.mockResolvedValueOnce({sharedCredentialDisabled: true})
    renderPanel()

    const bobRoleSelect = (await screen.findAllByDisplayValue('Member')).at(0)!
    fireEvent.change(bobRoleSelect, {target: {value: 'viewer'}})
    expect(await screen.findByText(DISABLED_NOTE)).toBeInTheDocument()
    expect(bumpProviderEpoch).toHaveBeenCalledTimes(1)

    act(() => {
      useOrgStore.setState({activeOrgId: 'org-2'} as never)
    })
    await waitFor(() => expect(azureApi.listAzureConnections).toHaveBeenCalledWith({kind: 'org', orgId: 'org-2'}))
    expect(await screen.findByText('Shared Azure AI Foundry')).toBeInTheDocument()
    expect(screen.queryByText(DISABLED_NOTE)).toBeNull()
  })

  it('clears the disabled-credential note once the credentials are entered again', async () => {
    seedAzure([sharedResource({status: 'needs_credentials'})], [{...sharedModel, status: 'needs_credentials'}])
    azureApi.updateAzureConnection.mockResolvedValue(sharedResource())
    removeMember.mockResolvedValueOnce({sharedCredentialDisabled: true})
    renderPanel()
    const removeButtons = await screen.findAllByLabelText('Remove member')
    fireEvent.click(removeButtons.find(b => !b.hasAttribute('disabled'))!)
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))
    const note = await screen.findByText(DISABLED_NOTE)
    const region = note.closest('[role="status"]')!
    expect(region).toBeInTheDocument()

    fireEvent.click(await screen.findByRole('button', {name: 'Enter credentials for acme-res'}))
    const dialog = screen.getByRole('dialog', {name: 'Enter credentials for acme-res'})
    fireEvent.change(within(dialog).getByLabelText('API key', {selector: 'input'}), {target: {value: 'sk-org'}})
    fireEvent.click(within(dialog).getByRole('button', {name: 'Save credentials'}))

    await waitFor(() => expect(azureApi.updateAzureConnection).toHaveBeenCalled())
    await waitFor(() => expect(screen.queryByText(DISABLED_NOTE)).toBeNull())
    expect(region).toBeInTheDocument()
    expect(region).toBeEmptyDOMElement()
  })

  it('does not claim a disabled credential when the departure disabled nothing', async () => {
    seedAzure()
    setMemberRole.mockResolvedValueOnce({sharedCredentialDisabled: false})
    renderPanel()
    const bobRoleSelect = (await screen.findAllByDisplayValue('Member')).at(0)!
    fireEvent.change(bobRoleSelect, {target: {value: 'viewer'}})
    await waitFor(() => expect(setMemberRole).toHaveBeenCalled())
    await waitFor(() => expect(azureApi.listAzureConnections).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(DISABLED_NOTE)).toBeNull()
  })

  it('explains when a removal disables the shared credential', async () => {
    seedAzure()
    removeMember.mockResolvedValueOnce({sharedCredentialDisabled: true})
    renderPanel()
    const removeButtons = await screen.findAllByLabelText('Remove member')
    fireEvent.click(removeButtons.find(b => !b.hasAttribute('disabled'))!)
    fireEvent.click(await screen.findByRole('button', {name: 'Remove'}))
    await waitFor(() => expect(removeMember).toHaveBeenCalledWith('org-1', 'u-2'))
    expect(await screen.findByText(DISABLED_NOTE)).toBeInTheDocument()
  })

  describe('deep links from "Manage in Organizations"', () => {
    it("focuses the active organisation's Azure AI Foundry heading once its models have loaded", async () => {
      seedAzure([sharedResource()], [sharedModel])
      useUIStore.setState({settingsAnchor: 'org-azure-foundry-org-1'})
      renderPanel()
      const heading = await screen.findByRole('heading', {level: 4, name: 'Shared Azure AI Foundry'})
      await waitFor(() => expect(heading).toHaveFocus())
      expect(useUIStore.getState().settingsAnchor).toBeNull()
    })

    it('lands on the name of an organisation that is not expanded, without switching to it', async () => {
      const orgB: Organisation = {...org, id: 'org-2', name: 'Globex'}
      useOrgStore.setState({organisations: [org, orgB]} as never)
      useUIStore.setState({settingsAnchor: 'org-azure-foundry-org-2'})
      renderPanel()
      await waitFor(() => expect(screen.getByText('Globex')).toHaveFocus())
      expect(setActiveOrg).not.toHaveBeenCalled()
      expect(useUIStore.getState().settingsAnchor).toBeNull()
    })

    it('spends an anchor that names no organisation of the list', async () => {
      useUIStore.setState({settingsAnchor: 'org-azure-foundry-gone'})
      renderPanel()
      await waitFor(() => expect(useUIStore.getState().settingsAnchor).toBeNull())
    })
  })
})
