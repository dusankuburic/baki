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
    expect(screen.getAllByText('Error').length).toBeGreaterThan(0)
  })

  it('toggling a rule uses setRuleEnabled (preserves severity/options), not a config replace', async () => {
    setRuleEnabled.mockResolvedValue(undefined)
    renderPanel()
    await screen.findByText('Deeply nested logic')

    const switches = screen.getAllByRole('switch')
    fireEvent.click(switches[switches.length - 1]!)

    await waitFor(() => {
      expect(setRuleEnabled).toHaveBeenCalledWith('deep-nesting', false)
    })
    expect(updateRuleConfig).not.toHaveBeenCalled()
  })

  it('shows an error toast when the toggle fails', async () => {
    setRuleEnabled.mockRejectedValue(new Error('boom'))
    renderPanel()
    await screen.findByText('Deeply nested logic')

    const switches = screen.getAllByRole('switch')
    fireEvent.click(switches[switches.length - 1]!)

    expect(await screen.findByText(/Failed to update rule/)).toBeInTheDocument()
  })
})

import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'

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
    renderPanel()
    const switches = await screen.findAllByRole('switch')
    fireEvent.click(switches[switches.length - 1]!)
    await waitFor(() => expect(setRuleEnabled).toHaveBeenCalled())
    expect(screen.queryByText(/Applies to/)).not.toBeInTheDocument()
  })

  it('a non-admin member of no org cannot edit deployment rules', async () => {
    signIn({id: 'u1', role: 'member'})
    renderPanel()
    const switches = await screen.findAllByRole('switch')
    expect(switches[switches.length - 1]!).toBeDisabled()
    fireEvent.click(switches[switches.length - 1]!)
    await waitFor(() => expect(setRuleEnabled).not.toHaveBeenCalled())
  })

  it('an org admin edits THEIR org profile, not the deployment singleton', async () => {
    signIn({id: 'u1', role: 'member'}, [
      {id: 'org-1', name: 'Acme', ownerId: 'u1', members: [{userId: 'u1', role: 'admin'}]},
    ])
    renderPanel()
    await screen.findByText(/Applies to/)

    const switches = await screen.findAllByRole('switch')
    fireEvent.click(switches[switches.length - 1]!)

    await waitFor(() => expect(updateOrgSettings).toHaveBeenCalled())
    expect(setRuleEnabled).not.toHaveBeenCalled()
    expect(updateOrgSettings.mock.calls[0]![0]).toBe('org-1')
  })

  it('merges into the org profile rather than replacing it', async () => {
    getOrgSettings.mockResolvedValue({analysis: {rules: {'other-rule': {enabled: false, severity: 'info'}}}})
    signIn({id: 'u1', role: 'member'}, [
      {id: 'org-1', name: 'Acme', ownerId: 'u1', members: [{userId: 'u1', role: 'admin'}]},
    ])
    renderPanel()
    await screen.findByText(/Applies to/)

    const switches = await screen.findAllByRole('switch')
    fireEvent.click(switches[switches.length - 1]!)

    await waitFor(() => expect(updateOrgSettings).toHaveBeenCalled())
    const sent = updateOrgSettings.mock.calls[0]![1] as {analysis: {rules: Record<string, unknown>}}
    expect(Object.keys(sent.analysis.rules)).toContain('other-rule')
    expect(Object.keys(sent.analysis.rules).length).toBeGreaterThan(1)
  })
})

describe('RulesPanel thresholds', () => {
  beforeEach(() => {
    useAuthStore.setState({user: null, isAuthenticated: false})
    useOrgStore.setState({organisations: []})
  })

  it('renders a numeric input per threshold and writes it back merged with severity', async () => {
    getRules.mockResolvedValue([
      {
        id: 'deep-nesting',
        name: 'Deeply nested logic',
        description: 'Detects deep nesting.',
        defaultSeverity: 'info',
        category: 'Style',
        enabled: true,
        options: {maxDepth: 6},
      },
    ])
    updateRuleConfig.mockResolvedValue(undefined)
    renderPanel()
    const box = await screen.findByTestId('thresholds-deep-nesting')
    expect(box).toHaveTextContent('maxDepth')
    const input = screen.getByDisplayValue('6')
    fireEvent.change(input, {target: {value: '3'}})
    fireEvent.blur(input)
    await waitFor(() =>
      expect(updateRuleConfig).toHaveBeenCalledWith('deep-nesting', {
        enabled: true,
        severity: 'info',
        options: {maxDepth: 3},
      }),
    )
  })

  it('shows no threshold block for a rule without options', async () => {
    renderPanel()
    await screen.findByText('Deeply nested logic')
    expect(screen.queryByTestId('thresholds-deep-nesting')).toBeNull()
  })

  it('marks a preview library rule, which toggles like any other', async () => {
    getRules.mockResolvedValue([
      {
        id: 'deep-nesting',
        name: 'Deeply nested logic',
        description: 'Detects deep nesting.',
        defaultSeverity: 'info',
        category: 'Style',
        enabled: true,
        stage: 'stable',
        kind: 'block',
      },
      {
        id: 'duplicate-subflow-across-flows',
        name: 'Duplicate subflow across flows',
        description: 'Same subflow body in two flows.',
        defaultSeverity: 'info',
        category: 'Style',
        enabled: true,
        stage: 'preview',
        kind: 'library',
      },
    ])
    setRuleEnabled.mockResolvedValue(undefined)
    renderPanel()
    await screen.findByText('Duplicate subflow across flows')
    expect(screen.getAllByText('Preview')).toHaveLength(1)
    expect(screen.getAllByText('Across flows')).toHaveLength(1)
    const row = screen.getByText('Duplicate subflow across flows').closest('div.p-4') as HTMLElement
    expect(row).toHaveTextContent('Preview')
    expect(row).toHaveTextContent('Across flows')

    const switches = screen.getAllByRole('switch')
    fireEvent.click(switches[switches.length - 1]!)
    await waitFor(() => expect(setRuleEnabled).toHaveBeenCalledWith('duplicate-subflow-across-flows', false))
  })
})
