import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, within} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {ciCredentialsApi, type OrgCICredential} from '@/api/governance'
import {ApiError, VersionConflictError} from '@/api/client'
import {writeClipboard} from '@/lib/clipboard'
import type {OrgMember} from '@/stores/orgStore'
import OrgCICredentialsSection from './OrgCICredentialsSection'

vi.mock('@/api/governance', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/governance')>()),
  ciCredentialsApi: {list: vi.fn(), create: vi.fn(), revoke: vi.fn()},
}))

// The command's endpoint is the API origin the app talks to.
vi.mock('@/api/client', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/client')>()),
  getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'https://baki.example.com/', token: ''}),
}))

vi.mock('@/lib/clipboard', () => ({writeClipboard: vi.fn()}))

const api = vi.mocked(ciCredentialsApi)

const members: OrgMember[] = [
  {userId: 'u-admin', role: 'admin', joinedAt: '2026-01-01T00:00:00Z', user: {id: 'u-admin', email: 'admin@acme.io'}},
]

const credential = (over: Partial<OrgCICredential> = {}): OrgCICredential => ({
  id: 'KEYID0000000000A',
  name: 'azure-main',
  secretHint: 'abcd',
  createdBy: 'u-admin',
  createdAt: '2026-01-01T00:00:00Z',
  expiresAt: null,
  lastUsedAt: null,
  ...over,
})

const SECRET = 'baki_ci_KEYID0000000000B_c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3I'

function renderSection() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <OrgCICredentialsSection orgId="org-1" members={members} />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

async function openCreateDialog(name: string, days = '') {
  fireEvent.click(await screen.findByRole('button', {name: 'New credential'}))
  const dialog = screen.getByRole('dialog', {name: 'New CI credential'})
  fireEvent.change(within(dialog).getByLabelText('Name'), {target: {value: name}})
  if (days) fireEvent.change(within(dialog).getByLabelText('Expires after (days)'), {target: {value: days}})
  fireEvent.click(within(dialog).getByRole('button', {name: 'Create'}))
  return dialog
}

beforeEach(() => {
  vi.clearAllMocks()
  api.list.mockResolvedValue([])
  api.revoke.mockResolvedValue(undefined)
  vi.mocked(writeClipboard).mockResolvedValue(undefined)
})

describe('OrgCICredentialsSection', () => {
  it('lists credentials with their hint, creator, expiry and last use', async () => {
    api.list.mockResolvedValue([
      credential(),
      credential({
        id: 'KEYID0000000000C',
        name: 'old-runner',
        secretHint: 'wxyz',
        createdBy: '',
        expiresAt: '2026-01-02T00:00:00Z',
        lastUsedAt: '2026-01-01T12:00:00Z',
      }),
    ])
    renderSection()

    const fresh = (await screen.findByText('azure-main')).closest('li')!
    expect(within(fresh).getByText('…abcd')).toBeInTheDocument()
    expect(within(fresh).getByText(/Created by admin@acme\.io/)).toBeInTheDocument()
    expect(within(fresh).getByText(/never expires/)).toBeInTheDocument()
    expect(within(fresh).getByText(/never used/)).toBeInTheDocument()
    expect(within(fresh).queryByText('Expired')).not.toBeInTheDocument()

    // An erased creator, an expiry in the past and a recorded use.
    const old = screen.getByText('old-runner').closest('li')!
    expect(within(old).getByText(/Created by a former member/)).toBeInTheDocument()
    expect(within(old).getByText('Expired')).toBeInTheDocument()
    expect(within(old).getByText(/^expired /)).toBeInTheDocument()
    expect(within(old).getByText(/^last used /)).toBeInTheDocument()
    expect(api.list).toHaveBeenCalledWith('org-1', expect.any(AbortSignal))
  })

  it('creates a credential and shows it once, with a ready bakicli command', async () => {
    api.create.mockResolvedValue({...credential({id: 'KEYID0000000000B', name: 'pipeline'}), credential: SECRET})
    renderSection()

    await openCreateDialog('  pipeline  ', '30')

    await waitFor(() => expect(api.create).toHaveBeenCalledWith('org-1', {name: 'pipeline', expiresInDays: 30}))
    const dialog = await screen.findByRole('dialog', {name: 'CI credential created'})
    expect(within(dialog).getByText(SECRET)).toBeInTheDocument()
    expect(
      within(dialog).getByText(
        `PAD_CI_WEBHOOK_SECRET=${SECRET} bakicli report -endpoint https://baki.example.com -flow-id <flow-id>`,
      ),
    ).toBeInTheDocument()
    // The list is read again, so the new row appears behind the dialog.
    await waitFor(() => expect(api.list).toHaveBeenCalledTimes(2))

    fireEvent.click(within(dialog).getByRole('button', {name: 'Copy credential'}))
    await waitFor(() => expect(writeClipboard).toHaveBeenCalledWith(SECRET))
    fireEvent.click(within(dialog).getByRole('button', {name: 'Copy command'}))
    await waitFor(() => expect(writeClipboard).toHaveBeenLastCalledWith(expect.stringContaining('bakicli report')))

    // Done drops the only copy of the secret.
    fireEvent.click(within(dialog).getByRole('button', {name: 'Done'}))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByText(SECRET)).not.toBeInTheDocument()
  })

  it('sends no expiry when the field is left empty', async () => {
    api.create.mockResolvedValue({...credential(), credential: SECRET})
    renderSection()

    await openCreateDialog('forever')

    await waitFor(() => expect(api.create).toHaveBeenCalledWith('org-1', {name: 'forever'}))
  })

  it('refuses an expiry outside 1..365 days without calling the server', async () => {
    renderSection()

    const dialog = await openCreateDialog('pipeline', '400')

    expect(await within(dialog).findByText(/whole number of days from 1 to 365/)).toBeInTheDocument()
    expect(api.create).not.toHaveBeenCalled()
  })

  it('explains the cap when the org already has the maximum of active credentials', async () => {
    api.create.mockRejectedValue(new VersionConflictError('too many active CI credentials'))
    renderSection()

    const dialog = await openCreateDialog('one-too-many')

    expect(
      await screen.findByText('This organisation has reached its limit of active CI credentials'),
    ).toBeInTheDocument()
    // The form stays, with what was typed.
    expect(within(dialog).getByLabelText('Name')).toHaveValue('one-too-many')
  })

  it('explains a server that cannot store credentials (field encryption off)', async () => {
    // The server masks a 5xx message: the code tells the case.
    api.create.mockRejectedValue(new ApiError('internal server error', 503, 'CI_CREDENTIALS_UNAVAILABLE'))
    renderSection()

    await openCreateDialog('pipeline')

    expect(await screen.findByText("This server can't store CI credentials")).toBeInTheDocument()
    expect(screen.getByText(/PAD_ENCRYPTION_KEY/)).toBeInTheDocument()
  })

  it('does not blame encryption for any other 503 (a proxy, an outage)', async () => {
    api.create.mockRejectedValue(new ApiError('internal server error', 503, 'SERVICE_UNAVAILABLE'))
    renderSection()

    await openCreateDialog('pipeline')

    expect(await screen.findByText(/internal server error/)).toBeInTheDocument()
    expect(screen.queryByText("This server can't store CI credentials")).not.toBeInTheDocument()
  })

  it('names a creator who left by the name the server recorded', async () => {
    api.list.mockResolvedValue([credential({createdBy: 'gone-1', createdByName: 'Ada Former'})])
    renderSection()

    expect(await screen.findByText(/Ada Former/)).toBeInTheDocument()
  })

  it('revokes a credential after a confirm and reads the list again', async () => {
    api.list.mockResolvedValue([credential()])
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: 'Revoke azure-main'}))
    const confirm = await screen.findByRole('dialog')
    expect(within(confirm).getByText(/stops working immediately/)).toBeInTheDocument()
    fireEvent.click(within(confirm).getByRole('button', {name: 'Revoke'}))

    await waitFor(() => expect(api.revoke).toHaveBeenCalledWith('org-1', 'KEYID0000000000A'))
    await waitFor(() => expect(api.list).toHaveBeenCalledTimes(2))
  })

  it('does nothing when the revoke is cancelled', async () => {
    api.list.mockResolvedValue([credential()])
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: 'Revoke azure-main'}))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', {name: /cancel/i}))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(api.revoke).not.toHaveBeenCalled()
  })

  it('reports a failed load', async () => {
    api.list.mockRejectedValue(new ApiError('Forbidden', 403))
    renderSection()

    expect(await screen.findByText('Failed to load CI credentials.')).toBeInTheDocument()
  })
})
