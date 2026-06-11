import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import RulesPanel from './RulesPanel'
import {ToastProvider} from '@/components/shared/Toast'

const getRules = vi.fn()
const setRuleEnabled = vi.fn()
const updateRuleConfig = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    getRules: (...a: unknown[]) => getRules(...a),
    setRuleEnabled: (...a: unknown[]) => setRuleEnabled(...a),
    updateRuleConfig: (...a: unknown[]) => updateRuleConfig(...a),
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
