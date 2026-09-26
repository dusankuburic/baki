import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import PolicyGatePanel from './PolicyGatePanel'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared'
import {useOrgStore} from '@/stores/orgStore'

const getRules = vi.fn()
const listPolicies = vi.fn()
const savePolicy = vi.fn()
const deletePolicy = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    getRules: (...a: unknown[]) => getRules(...a),
    listPolicies: (...a: unknown[]) => listPolicies(...a),
    savePolicy: (...a: unknown[]) => savePolicy(...a),
    deletePolicy: (...a: unknown[]) => deletePolicy(...a),
    evaluatePolicyById: vi.fn(),
  },
}))

const policy = {id: 'p1', orgId: 'org-1', name: 'Release gate', description: '', rules: [], gateSeverity: 'warning'}

function renderPanel() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <PolicyGatePanel />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  getRules.mockResolvedValue([])
  listPolicies.mockResolvedValue([policy])
  useOrgStore.setState({activeOrgId: 'org-1'})
})

describe('PolicyGatePanel', () => {
  // Regression: the delete button used to be nested INSIDE the expand button
  // (invalid markup; the click had to stop propagation to avoid toggling).
  it('keeps the delete control outside the expand button', async () => {
    renderPanel()
    const expand = await screen.findByRole('button', {name: /Release gate/})
    const remove = screen.getByRole('button', {name: 'Delete policy'})
    expect(expand).not.toContainElement(remove)
    expect(expand).toHaveAttribute('aria-expanded', 'false')
    fireEvent.click(expand)
    expect(expand).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByRole('button', {name: 'Delete policy'})).toBeInTheDocument()
  })

  it('offers exactly one New Policy action, in the body toolbar', async () => {
    renderPanel()
    await screen.findByRole('button', {name: /Release gate/})
    expect(screen.getAllByRole('button', {name: 'New Policy'})).toHaveLength(1)
  })
})
