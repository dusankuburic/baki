import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, waitFor, within} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {providersApi, settingsApi} from '@/api'
import {ApiError, clearRequestCache} from '@/api/client'
import type {AppSettings, ProviderSpec} from '@/types'
import OrgAIProvidersSection from './OrgAIProvidersSection'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 'mock-token', port: 9999}),
  }),
}))

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return {
    ...actual,
    providersApi: {...actual.providersApi, getProviderCatalog: vi.fn()},
    settingsApi: {
      ...actual.settingsApi,
      getOrgSettings: vi.fn(),
      updateOrgSettings: vi.fn(),
      getOrgSettingsSection: vi.fn(),
      patchOrgSettingsSection: vi.fn(),
    },
  }
})

const providers = vi.mocked(providersApi)
const settings = vi.mocked(settingsApi)

const spec = (id: string, name: string): ProviderSpec => ({
  id,
  name,
  authType: 'api_key',
  modes: ['cloud'],
  caps: ['chat'],
  credential: {kind: 'api_key', fields: []},
  endpoints: {},
  configured: false,
})

const CATALOG = [spec('claude', 'Anthropic Claude'), spec('openai', 'OpenAI'), spec('gemini', 'Google Gemini')]

const orgDoc = (ai: Record<string, unknown> = {}, rules: Record<string, unknown> = {}): AppSettings =>
  ({
    version: 1,
    profile: {},
    general: {firstRunCompleted: false},
    appearance: {},
    layout: {},
    ai: {activeProvider: '', providers: {}, ...ai},
    parser: {maxFileSizeMB: 0},
    analysis: {rules, autoAnalyzeOnOpen: false},
  }) as unknown as AppSettings

function renderSection() {
  return render(
    <ToastProvider>
      <OrgAIProvidersSection orgId="org-1" />
    </ToastProvider>,
  )
}

const limitSwitch = () => screen.findByRole('switch', {name: 'Only allow the providers selected below'})
const saveButton = () => screen.getByRole('button', {name: 'Save'})
const sentDocument = (call = 0) => settings.updateOrgSettings.mock.calls[call]![1] as AppSettings

// The section route (R-config-4) answers 404 by default: a replica from before it, which the section falls back from
// to the whole-document write. The tests of the section write set it up themselves.
const noSectionRoute = () => new ApiError('404 page not found', 404)

beforeEach(() => {
  vi.clearAllMocks()
  providers.getProviderCatalog.mockResolvedValue(CATALOG)
  settings.getOrgSettings.mockResolvedValue(orgDoc())
  settings.updateOrgSettings.mockResolvedValue(undefined)
  // mockReset, not only clearAllMocks: a test that stops early must not leave its queued answers to the next one.
  settings.getOrgSettingsSection.mockReset().mockRejectedValue(noSectionRoute())
  settings.patchOrgSettingsSection.mockReset().mockRejectedValue(noSectionRoute())
})

afterEach(() => {
  vi.restoreAllMocks()
  clearRequestCache()
})

// The org settings route behind the real client and its GET cache: a GET answers the stored document, a POST
// replaces each section its body names.
async function serveOrgSettings(initial: AppSettings) {
  const real = (await vi.importActual<typeof import('@/api/settings')>('@/api/settings')).settingsApi
  settings.getOrgSettings.mockImplementation(real.getOrgSettings)
  settings.updateOrgSettings.mockImplementation(real.updateOrgSettings)
  let stored = initial
  const posts: AppSettings[] = []
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (_url, init) => {
    if (init?.method === 'GET') return new Response(JSON.stringify(stored), {status: 200})
    const body = JSON.parse(String(init?.body)) as AppSettings
    posts.push(body)
    stored = {...stored, ...body}
    return new Response(null, {status: 204})
  })
  return {posts, write: real.updateOrgSettings}
}

describe('OrgAIProvidersSection', () => {
  it('lists the catalogue in its order and allows every provider when no list is stored', async () => {
    renderSection()
    expect(await limitSwitch()).toHaveAttribute('aria-checked', 'false')
    expect(screen.getByText('Every provider this server offers is allowed.')).toBeInTheDocument()
    fireEvent.click(await limitSwitch())
    const group = screen.getByRole('group', {name: 'Allowed AI providers'})
    expect(
      within(group)
        .getAllByRole('checkbox')
        .map(c => c.closest('label')?.textContent),
    ).toEqual(['Anthropic Claude', 'OpenAI', 'Google Gemini'])
  })

  it('saves a restriction onto the fresh document, in catalogue order, keeping the other sections', async () => {
    settings.getOrgSettings
      .mockResolvedValueOnce(orgDoc())
      .mockResolvedValueOnce(orgDoc({systemPromptSuffix: 'Be brief.'}, {'dead-code': {enabled: false, severity: 'info'}}))
    renderSection()
    fireEvent.click(await limitSwitch())
    fireEvent.click(screen.getByRole('checkbox', {name: 'Google Gemini'}))
    fireEvent.click(screen.getByRole('checkbox', {name: 'Anthropic Claude'}))
    fireEvent.click(saveButton())

    await waitFor(() => expect(settings.updateOrgSettings).toHaveBeenCalledTimes(1))
    expect(settings.updateOrgSettings.mock.calls[0]![0]).toBe('org-1')
    const sent = sentDocument()
    expect(sent.ai.allowedProviders).toEqual(['claude', 'gemini'])
    expect(sent.ai.systemPromptSuffix).toBe('Be brief.')
    expect(sent.analysis.rules).toEqual({'dead-code': {enabled: false, severity: 'info'}})
    expect(await screen.findByText('AI providers saved')).toBeInTheDocument()
  })

  it('keeps a rule override another panel saved after this section loaded (no stale cached read)', async () => {
    const server = await serveOrgSettings(orgDoc())
    renderSection()
    fireEvent.click(await limitSwitch())
    const override = {'dead-code': {enabled: false, severity: 'info'}}
    await server.write('org-1', {analysis: {rules: override}} as never)

    fireEvent.click(screen.getByRole('checkbox', {name: 'OpenAI'}))
    fireEvent.click(saveButton())
    await waitFor(() => expect(server.posts).toHaveLength(2))
    expect(server.posts[1]!.ai.allowedProviders).toEqual(['openai'])
    expect(server.posts[1]!.analysis.rules).toEqual(override)
  })

  it('clears a stored list with an explicit empty list, never by leaving the field out', async () => {
    settings.getOrgSettings.mockResolvedValue(orgDoc({allowedProviders: ['openai']}))
    renderSection()
    expect(await limitSwitch()).toHaveAttribute('aria-checked', 'true')
    expect(screen.getByRole('checkbox', {name: 'OpenAI'})).toHaveAttribute('aria-checked', 'true')
    fireEvent.click(await limitSwitch())
    fireEvent.click(saveButton())

    await waitFor(() => expect(settings.updateOrgSettings).toHaveBeenCalledTimes(1))
    const sent = sentDocument()
    expect(Object.prototype.hasOwnProperty.call(sent.ai, 'allowedProviders')).toBe(true)
    expect(sent.ai.allowedProviders).toEqual([])
  })

  it('sends nothing while the list is unchanged', async () => {
    settings.getOrgSettings.mockResolvedValue(orgDoc({allowedProviders: ['openai', 'claude']}))
    renderSection()
    await limitSwitch()
    expect(saveButton()).toHaveAttribute('aria-disabled', 'true')

    fireEvent.click(screen.getByRole('checkbox', {name: 'Google Gemini'}))
    expect(saveButton()).not.toHaveAttribute('aria-disabled')
    fireEvent.click(screen.getByRole('checkbox', {name: 'Google Gemini'}))
    expect(saveButton()).toHaveAttribute('aria-disabled', 'true')

    fireEvent.click(await limitSwitch())
    fireEvent.click(await limitSwitch())
    expect(saveButton()).toHaveAttribute('aria-disabled', 'true')
    fireEvent.click(saveButton())
    expect(settings.updateOrgSettings).not.toHaveBeenCalled()
    expect(settings.getOrgSettings).toHaveBeenCalledTimes(1)
  })

  it('refuses a limit that selects nothing, inline and only once submitted', async () => {
    renderSection()
    fireEvent.click(await limitSwitch())
    const message = 'Select at least one provider, or turn off the limit to allow every provider.'
    expect(screen.queryByText(message)).toBeNull()

    fireEvent.click(saveButton())
    const error = await screen.findByText(message)
    const group = screen.getByRole('group', {name: 'Allowed AI providers'})
    expect(group.getAttribute('aria-describedby')?.split(' ')).toContain(error.id)
    expect(screen.getByRole('checkbox', {name: 'Anthropic Claude'})).toHaveFocus()
    expect(settings.updateOrgSettings).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('checkbox', {name: 'OpenAI'}))
    expect(screen.queryByText(message)).toBeNull()
  })

  it("unchecks the ids the server's PROVIDER_UNKNOWN names, says which in a live region and focuses the first", async () => {
    settings.getOrgSettings.mockResolvedValue(orgDoc({allowedProviders: ['local', 'openai']}))
    settings.updateOrgSettings.mockRejectedValueOnce(
      new ApiError('unknown provider: "local" in allowedProviders', 400, 'PROVIDER_UNKNOWN', null, {
        providers: ['local'],
      }),
    )
    renderSection()
    const stray = await screen.findByRole('checkbox', {name: 'local (not offered by this server)'})
    fireEvent.click(screen.getByRole('checkbox', {name: 'Anthropic Claude'}))
    saveButton().focus()
    fireEvent.click(saveButton())

    const error = await screen.findByRole('alert')
    expect(error).toHaveTextContent(
      "This server doesn't know the provider local. It's unchecked now; save again to keep the rest of the list.",
    )
    const group = screen.getByRole('group', {name: 'Allowed AI providers'})
    expect(group.getAttribute('aria-describedby')?.split(' ')).toContain(error.id)
    expect(stray).toHaveAttribute('aria-checked', 'false')
    expect(stray).toHaveFocus()
    expect(screen.queryByText('Failed to save the AI providers')).toBeNull()

    fireEvent.click(saveButton())
    await waitFor(() => expect(settings.updateOrgSettings).toHaveBeenCalledTimes(2))
    expect(sentDocument(1).ai.allowedProviders).toEqual(['claude', 'openai'])
  })

  it('against an old server (no details) unchecks the selected ids the catalogue does not offer', async () => {
    settings.getOrgSettings.mockResolvedValue(orgDoc({allowedProviders: ['local', 'legacy', 'openai']}))
    settings.updateOrgSettings.mockRejectedValueOnce(
      new ApiError('unknown provider: "legacy" in allowedProviders', 400, 'PROVIDER_UNKNOWN'),
    )
    renderSection()
    await screen.findByRole('checkbox', {name: 'local (not offered by this server)'})
    fireEvent.click(screen.getByRole('checkbox', {name: 'Anthropic Claude'}))
    fireEvent.click(saveButton())

    expect(await screen.findByRole('alert')).toHaveTextContent(
      "This server doesn't know the providers local, legacy. They're unchecked now; save again to keep the rest of the list.",
    )
    expect(screen.getByRole('checkbox', {name: 'local (not offered by this server)'})).toHaveAttribute(
      'aria-checked',
      'false',
    )
    expect(screen.getByRole('checkbox', {name: 'OpenAI'})).toHaveAttribute('aria-checked', 'true')
  })

  it('without an id to name, says so in a live region and focuses the list', async () => {
    settings.updateOrgSettings.mockRejectedValueOnce(new ApiError('unknown provider', 400, 'PROVIDER_UNKNOWN'))
    renderSection()
    fireEvent.click(await limitSwitch())
    fireEvent.click(screen.getByRole('checkbox', {name: 'OpenAI'}))
    fireEvent.click(saveButton())

    expect(await screen.findByRole('alert')).toHaveTextContent(
      "The server doesn't know one of the selected providers. Reload the settings and choose again.",
    )
    expect(screen.getByRole('checkbox', {name: 'Anthropic Claude'})).toHaveFocus()
  })

  it('keeps focus on the save button after a successful save', async () => {
    renderSection()
    fireEvent.click(await limitSwitch())
    fireEvent.click(screen.getByRole('checkbox', {name: 'OpenAI'}))
    saveButton().focus()
    fireEvent.click(saveButton())
    expect(await screen.findByText('AI providers saved')).toBeInTheDocument()
    expect(saveButton()).toHaveFocus()
    expect(saveButton()).toHaveAttribute('aria-disabled', 'true')
  })

  it('keeps a stored id the catalogue does not list, until it is unchecked', async () => {
    settings.getOrgSettings.mockResolvedValue(orgDoc({allowedProviders: ['local', 'openai']}))
    renderSection()
    const stray = await screen.findByRole('checkbox', {name: 'local (not offered by this server)'})
    expect(stray).toHaveAttribute('aria-checked', 'true')

    fireEvent.click(screen.getByRole('checkbox', {name: 'Anthropic Claude'}))
    fireEvent.click(saveButton())
    await waitFor(() => expect(settings.updateOrgSettings).toHaveBeenCalledTimes(1))
    expect(sentDocument().ai.allowedProviders).toEqual(['claude', 'openai', 'local'])
  })

  it('renders nothing against a server without the provider catalogue', async () => {
    providers.getProviderCatalog.mockRejectedValue(new ApiError('not found', 404))
    const {container} = renderSection()
    await waitFor(() => expect(providers.getProviderCatalog).toHaveBeenCalled())
    await waitFor(() => expect(container.querySelector('section')).toBeNull())
  })

  describe('the section write', () => {
    const aiSection = (allowedProviders: string[], revision: string) => ({
      section: 'ai',
      values: {allowedProviders, systemPromptSuffix: 'Be brief.'},
      revision,
    })

    it('names only the list, with the revision it loaded, and carries the next revision forward', async () => {
      settings.getOrgSettingsSection.mockResolvedValue(aiSection(['openai'], 'r1'))
      settings.patchOrgSettingsSection
        .mockResolvedValueOnce(aiSection(['openai', 'gemini'], 'r2'))
        .mockResolvedValueOnce(aiSection(['gemini'], 'r3'))
      renderSection()
      expect(await screen.findByRole('checkbox', {name: 'OpenAI'})).toHaveAttribute('aria-checked', 'true')
      fireEvent.click(screen.getByRole('checkbox', {name: 'Google Gemini'}))
      fireEvent.click(saveButton())

      expect(await screen.findByText('AI providers saved')).toBeInTheDocument()
      expect(settings.patchOrgSettingsSection).toHaveBeenCalledWith(
        'org-1',
        'ai',
        {allowedProviders: ['openai', 'gemini']},
        'r1',
      )
      expect(settings.updateOrgSettings).not.toHaveBeenCalled()
      expect(settings.getOrgSettings).not.toHaveBeenCalled()

      fireEvent.click(screen.getByRole('checkbox', {name: 'OpenAI'}))
      fireEvent.click(saveButton())
      await waitFor(() => expect(settings.patchOrgSettingsSection).toHaveBeenCalledTimes(2))
      expect(settings.patchOrgSettingsSection.mock.calls[1]).toEqual([
        'org-1',
        'ai',
        {allowedProviders: ['gemini']},
        'r2',
      ])
    })

    it('on a stale revision reloads the list before anything is retried, and says so', async () => {
      settings.getOrgSettingsSection
        .mockResolvedValueOnce(aiSection(['openai'], 'r1'))
        .mockResolvedValueOnce(aiSection(['claude'], 'r2'))
      settings.patchOrgSettingsSection
        .mockRejectedValueOnce(new ApiError('the settings changed since they were read', 409, 'SETTINGS_STALE'))
        .mockResolvedValueOnce(aiSection(['claude', 'gemini'], 'r3'))
      renderSection()
      fireEvent.click(await screen.findByRole('checkbox', {name: 'Google Gemini'}))
      fireEvent.click(saveButton())

      expect(
        await screen.findByText(
          'The AI settings changed after you opened them. The current allowed providers are loaded; review them and save again.',
        ),
      ).toBeInTheDocument()
      await waitFor(() =>
        expect(screen.getByRole('checkbox', {name: 'Anthropic Claude'})).toHaveAttribute('aria-checked', 'true'),
      )
      expect(screen.getByRole('checkbox', {name: 'OpenAI'})).toHaveAttribute('aria-checked', 'false')
      expect(screen.getByRole('checkbox', {name: 'Google Gemini'})).toHaveAttribute('aria-checked', 'false')
      expect(settings.patchOrgSettingsSection).toHaveBeenCalledTimes(1)
      expect(settings.updateOrgSettings).not.toHaveBeenCalled()

      fireEvent.click(screen.getByRole('checkbox', {name: 'Google Gemini'}))
      fireEvent.click(saveButton())
      await waitFor(() => expect(settings.patchOrgSettingsSection).toHaveBeenCalledTimes(2))
      expect(settings.patchOrgSettingsSection.mock.calls[1]).toEqual([
        'org-1',
        'ai',
        {allowedProviders: ['claude', 'gemini']},
        'r2',
      ])
    })

    it('keeps the selection when the reload after a stale save fails, says so truthfully and offers a retry', async () => {
      settings.getOrgSettingsSection
        .mockResolvedValueOnce(aiSection(['openai'], 'r1'))
        .mockRejectedValueOnce(new ApiError('boom', 500))
        .mockResolvedValueOnce(aiSection(['claude'], 'r2'))
      settings.patchOrgSettingsSection.mockRejectedValueOnce(
        new ApiError('the settings changed since they were read', 409, 'SETTINGS_STALE'),
      )
      renderSection()
      fireEvent.click(await screen.findByRole('checkbox', {name: 'Google Gemini'}))
      fireEvent.click(saveButton())

      const alert = await screen.findByText(
        "The AI settings changed after you opened them, and the current settings couldn't be loaded. Your selection is kept; reload them before saving again.",
      )
      expect(alert.closest('[role="alert"]')).not.toBeNull()
      // The form stays, with the admin's selection; the stale toast that claims a reload never appears.
      expect(screen.queryByText("Couldn't load the organisation's AI providers.")).toBeNull()
      expect(screen.queryByText(/The current allowed providers are loaded/)).toBeNull()
      expect(screen.getByRole('checkbox', {name: 'OpenAI'})).toHaveAttribute('aria-checked', 'true')
      expect(screen.getByRole('checkbox', {name: 'Google Gemini'})).toHaveAttribute('aria-checked', 'true')

      fireEvent.click(screen.getByRole('button', {name: 'Reload'}))
      await waitFor(() =>
        expect(screen.getByRole('checkbox', {name: 'Anthropic Claude'})).toHaveAttribute('aria-checked', 'true'),
      )
      expect(screen.getByRole('checkbox', {name: 'Google Gemini'})).toHaveAttribute('aria-checked', 'false')
      expect(screen.queryByRole('button', {name: 'Reload'})).toBeNull()
      expect(settings.getOrgSettingsSection).toHaveBeenCalledTimes(3)
      expect(settings.patchOrgSettingsSection).toHaveBeenCalledTimes(1)
    })

    it('never falls back on a 404 the section route itself answers with a code', async () => {
      settings.getOrgSettingsSection.mockRejectedValue(new ApiError('organisation not found', 404, 'NOT_FOUND'))
      renderSection()
      expect(await screen.findByText("Couldn't load the organisation's AI providers.")).toBeInTheDocument()
      expect(settings.getOrgSettings).not.toHaveBeenCalled()
    })

    it('never falls back to the whole-document write on a coded 404 from the section write', async () => {
      settings.getOrgSettingsSection.mockResolvedValue(aiSection([], 'r1'))
      settings.patchOrgSettingsSection.mockRejectedValueOnce(new ApiError('organisation not found', 404, 'NOT_FOUND'))
      renderSection()
      fireEvent.click(await limitSwitch())
      fireEvent.click(screen.getByRole('checkbox', {name: 'OpenAI'}))
      fireEvent.click(saveButton())

      expect(await screen.findByText('Failed to save the AI providers')).toBeInTheDocument()
      expect(settings.updateOrgSettings).not.toHaveBeenCalled()
      expect(settings.getOrgSettings).not.toHaveBeenCalled()
    })

    it('falls back to the whole-document write on a 405 (a proxy that knows the path, not the method)', async () => {
      settings.getOrgSettingsSection.mockResolvedValue(aiSection([], 'r1'))
      settings.patchOrgSettingsSection.mockRejectedValueOnce(new ApiError('Request failed', 405))
      renderSection()
      fireEvent.click(await limitSwitch())
      fireEvent.click(screen.getByRole('checkbox', {name: 'OpenAI'}))
      fireEvent.click(saveButton())

      await waitFor(() => expect(settings.updateOrgSettings).toHaveBeenCalledTimes(1))
      expect(sentDocument().ai.allowedProviders).toEqual(['openai'])
    })

    it('falls back to the whole-document write when the replica that answers has no section route', async () => {
      settings.getOrgSettingsSection.mockResolvedValue(aiSection([], 'r1'))
      settings.getOrgSettings.mockResolvedValue(orgDoc({systemPromptSuffix: 'Be brief.'}))
      renderSection()
      fireEvent.click(await limitSwitch())
      fireEvent.click(screen.getByRole('checkbox', {name: 'OpenAI'}))
      fireEvent.click(saveButton())

      await waitFor(() => expect(settings.updateOrgSettings).toHaveBeenCalledTimes(1))
      expect(settings.patchOrgSettingsSection).toHaveBeenCalledTimes(1)
      expect(settings.getOrgSettings).toHaveBeenCalledWith('org-1', {noDedup: true})
      expect(sentDocument().ai).toEqual(
        expect.objectContaining({allowedProviders: ['openai'], systemPromptSuffix: 'Be brief.'}),
      )
      expect(await screen.findByText('AI providers saved')).toBeInTheDocument()
    })
  })

  it('says so when the list cannot be loaded', async () => {
    settings.getOrgSettings.mockRejectedValue(new ApiError('boom', 500))
    renderSection()
    expect(await screen.findByText("Couldn't load the organisation's AI providers.")).toBeInTheDocument()
  })
})
