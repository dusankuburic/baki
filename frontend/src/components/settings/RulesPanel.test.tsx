import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import RulesPanel from './RulesPanel'
import {ToastProvider} from '@/components/shared/Toast'

const getRules = vi.fn()
const setRuleEnabled = vi.fn()
const updateRuleConfig = vi.fn()

const getOrgSettings = vi.fn()
const updateOrgSettings = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    getRules: (...a: unknown[]) => getRules(...a),
    setRuleEnabled: (...a: unknown[]) => setRuleEnabled(...a),
    updateRuleConfig: (...a: unknown[]) => updateRuleConfig(...a),
  },
  // settingsApi is mocked even though the deployment-scope tests never reach it:
  // RulesPanel imports it unconditionally, and a factory that omits an imported
  // export throws at call time rather than at import — which is how a test can
  // pass while the code path it claims to cover never runs.
  settingsApi: {
    getOrgSettings: (...a: unknown[]) => getOrgSettings(...a),
    updateOrgSettings: (...a: unknown[]) => updateOrgSettings(...a),
  },
}))

function renderPanel() {
  return render(
    <ToastProvider>
      <RulesPanel />
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  getRules.mockResolvedValue([
    {
      id: 'deep-nesting',
      name: 'Deeply nested logic',
      description: 'Detects deep nesting.',
      // Simulates a configured severity override surfaced by the backend (B1)
      defaultSeverity: 'error',
      category: 'Style',
      enabled: true,
    },
  ])
})

describe('RulesPanel', () => {
  it('renders rules with the backend-provided (configured) severity selected', async () => {
    renderPanel()
    expect(await screen.findByText('Deeply nested logic')).toBeInTheDocument()
    // The severity SegmentedControl reflects the configured override
    expect(screen.getAllByText('Error').length).toBeGreaterThan(0)
  })

  it('toggling a rule uses setRuleEnabled (preserves severity/options), not a config replace', async () => {
    setRuleEnabled.mockResolvedValue(undefined)
    renderPanel()
    await screen.findByText('Deeply nested logic')

    // The rule row Switch is the second switch (first is auto-analyze)
    const switches = screen.getAllByRole('switch')
    fireEvent.click(switches[switches.length - 1])

    await waitFor(() => {
      expect(setRuleEnabled).toHaveBeenCalledWith('deep-nesting', false)
    })
    // Regression for the B1 wipe chain: toggle must NOT full-replace the config
    expect(updateRuleConfig).not.toHaveBeenCalled()
  })

  it('shows an error toast when the toggle fails', async () => {
    setRuleEnabled.mockRejectedValue(new Error('boom'))
    renderPanel()
    await screen.findByText('Deeply nested logic')

    const switches = screen.getAllByRole('switch')
    fireEvent.click(switches[switches.length - 1])

    expect(await screen.findByText(/Failed to update rule/)).toBeInTheDocument()
  })
})

// --- R4: scoped rule configuration -----------------------------------------

import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'

/** Puts the app into cloud mode as `user`, a member/admin of `orgs`. */
function signIn(user: {id: string; role: 'admin' | 'member'}, orgs: unknown[] = []) {
  useAuthStore.setState({
    user: {id: user.id, email: `${user.id}@x.io`, role: user.role} as never,
    isAuthenticated: true,
  })
  useOrgStore.setState({organisations: orgs as never})
}

describe('RulesPanel scoping (R4)', () => {
  beforeEach(() => {
    useAuthStore.setState({user: null, isAuthenticated: false})
    useOrgStore.setState({organisations: []})
    getOrgSettings.mockResolvedValue({analysis: {rules: {}}})
    updateOrgSettings.mockResolvedValue(undefined)
  })

  it('stays editable in local/desktop mode, where there are no tenants', async () => {
    // No authenticated user: the backend's RequireRole short-circuits when JWT
    // auth is off, and the panel must match — otherwise every desktop user gets
    // a read-only rules screen.
    renderPanel()
    const switches = await screen.findAllByRole('switch')
    fireEvent.click(switches[switches.length - 1])
    await waitFor(() => expect(setRuleEnabled).toHaveBeenCalled())
    expect(screen.queryByText(/Applies to/)).not.toBeInTheDocument()
  })

  it('a non-admin member of no org cannot edit deployment rules', async () => {
    signIn({id: 'u1', role: 'member'})
    renderPanel()
    const switches = await screen.findAllByRole('switch')
    expect(switches[switches.length - 1]).toBeDisabled()
    fireEvent.click(switches[switches.length - 1])
    await waitFor(() => expect(setRuleEnabled).not.toHaveBeenCalled())
  })

  it('an org admin edits THEIR org profile, not the deployment singleton', async () => {
    signIn({id: 'u1', role: 'member'}, [
      {id: 'org-1', name: 'Acme', ownerId: 'u1', members: [{userId: 'u1', role: 'admin'}]},
    ])
    renderPanel()
    await screen.findByText(/Applies to/)

    const switches = await screen.findAllByRole('switch')
    fireEvent.click(switches[switches.length - 1])

    // The org profile is written; the deployment endpoint is NOT touched. That
    // separation is the whole point — before R4 this panel wrote the singleton
    // that every tenant shares.
    await waitFor(() => expect(updateOrgSettings).toHaveBeenCalled())
    expect(setRuleEnabled).not.toHaveBeenCalled()
    expect(updateOrgSettings.mock.calls[0][0]).toBe('org-1')
  })

  it('merges into the org profile rather than replacing it', async () => {
    // An org that already overrides another rule must keep that override when a
    // second one is set — a replace would silently drop it.
    getOrgSettings.mockResolvedValue({analysis: {rules: {'other-rule': {enabled: false, severity: 'info'}}}})
    signIn({id: 'u1', role: 'member'}, [
      {id: 'org-1', name: 'Acme', ownerId: 'u1', members: [{userId: 'u1', role: 'admin'}]},
    ])
    renderPanel()
    await screen.findByText(/Applies to/)

    const switches = await screen.findAllByRole('switch')
    fireEvent.click(switches[switches.length - 1])

    await waitFor(() => expect(updateOrgSettings).toHaveBeenCalled())
    const sent = updateOrgSettings.mock.calls[0][1] as {analysis: {rules: Record<string, unknown>}}
    expect(Object.keys(sent.analysis.rules)).toContain('other-rule')
    expect(Object.keys(sent.analysis.rules).length).toBeGreaterThan(1)
  })
})
