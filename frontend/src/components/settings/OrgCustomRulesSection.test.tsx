import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import OrgCustomRulesSection from './OrgCustomRulesSection'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'

const list = vi.fn()
const save = vi.fn()
const remove = vi.fn()
const validateCustomRules = vi.fn()
const testCustomRule = vi.fn()
const libraryList = vi.fn()

vi.mock('@/api/governance', () => ({
  orgRulesApi: {
    list: (...a: unknown[]) => list(...a),
    save: (...a: unknown[]) => save(...a),
    remove: (...a: unknown[]) => remove(...a),
  },
}))

// The full surface the component imports. An incomplete factory throws at CALL
// time, not import time, so a missing export can leave a test passing while the
// path it claims to cover never runs.
vi.mock('@/api', () => ({
  analysisApi: {
    validateCustomRules: (...a: unknown[]) => validateCustomRules(...a),
    testCustomRule: (...a: unknown[]) => testCustomRule(...a),
  },
  libraryApi: {list: (...a: unknown[]) => libraryList(...a)},
}))

function renderSection(isAdmin = true) {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <OrgCustomRulesSection orgId="org-1" isAdmin={isAdmin} />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  list.mockResolvedValue([])
  save.mockResolvedValue({})
  libraryList.mockResolvedValue({items: [{id: 'flow-1', name: 'Main'}]})
  validateCustomRules.mockResolvedValue({
    entries: [{index: 0, id: 'r', valid: true, loaded: true}],
    valid: 1,
    invalid: 0,
  })
})

describe('OrgCustomRulesSection', () => {
  it('hides authoring controls from non-admin members', async () => {
    list.mockResolvedValue([
      {id: 'row-1', ruleId: 'house', enabled: true, config: {id: 'house', name: 'House', severity: 'warning'}},
    ])
    renderSection(false)
    expect(await screen.findByText('House')).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /new rule/i})).not.toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /delete rule/i})).not.toBeInTheDocument()
  })

  it('reports when a rule compiles but matches nothing', async () => {
    // The failure an author cannot otherwise see: validate says "valid", and
    // the rule would never fire. Distinguishing the two is the whole reason the
    // try-it action exists.
    testCustomRule.mockResolvedValue({matches: 0, flowName: 'Main', findings: []})
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /new rule/i}))
    fireEvent.change(screen.getByPlaceholderText('no-http-without-retry'), {target: {value: 'my-rule'}})
    fireEvent.change(await screen.findByRole('combobox'), {target: {value: 'flow-1'}})
    fireEvent.click(screen.getByRole('button', {name: /try it/i}))

    expect(await screen.findByText(/would not fire here/i)).toBeInTheDocument()
  })

  it('reports a match count when the rule does fire', async () => {
    testCustomRule.mockResolvedValue({matches: 3, flowName: 'Main', findings: []})
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /new rule/i}))
    fireEvent.change(screen.getByPlaceholderText('no-http-without-retry'), {target: {value: 'my-rule'}})
    fireEvent.change(await screen.findByRole('combobox'), {target: {value: 'flow-1'}})
    fireEvent.click(screen.getByRole('button', {name: /try it/i}))

    expect(await screen.findByText(/matches 3 block/i)).toBeInTheDocument()
  })

  it('clears a stale try-it result when the rule is edited', async () => {
    // A "matches 3" left next to a changed regex is worse than no result: it
    // reports on a rule that no longer exists.
    testCustomRule.mockResolvedValue({matches: 3, flowName: 'Main', findings: []})
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /new rule/i}))
    fireEvent.change(screen.getByPlaceholderText('no-http-without-retry'), {target: {value: 'my-rule'}})
    fireEvent.change(await screen.findByRole('combobox'), {target: {value: 'flow-1'}})
    fireEvent.click(screen.getByRole('button', {name: /try it/i}))
    expect(await screen.findByText(/matches 3 block/i)).toBeInTheDocument()

    fireEvent.change(screen.getByPlaceholderText('^WebAutomation'), {target: {value: '^SET$'}})
    await waitFor(() => expect(screen.queryByText(/matches 3 block/i)).not.toBeInTheDocument())
  })

  it('will not save a rule the server rejects', async () => {
    validateCustomRules.mockResolvedValue({
      entries: [{index: 0, id: 'bad', valid: false, error: 'invalid regex', loaded: false}],
      valid: 0,
      invalid: 1,
    })
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /new rule/i}))
    fireEvent.change(screen.getByPlaceholderText('no-http-without-retry'), {target: {value: 'bad'}})
    fireEvent.click(screen.getByRole('button', {name: /save rule/i}))

    expect(await screen.findByText(/invalid regex/i)).toBeInTheDocument()
    await waitFor(() => expect(save).not.toHaveBeenCalled())
  })
})
