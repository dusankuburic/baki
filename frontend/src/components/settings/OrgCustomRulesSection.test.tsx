import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import OrgCustomRulesSection from './OrgCustomRulesSection'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {ApiError} from '@/api/client'

const list = vi.fn()
const save = vi.fn()
const setEnabled = vi.fn()
const remove = vi.fn()
const validateCustomRules = vi.fn()
const testCustomRule = vi.fn()
const libraryList = vi.fn()

// The real module with the API object replaced: the component also uses its
// pure helpers (isNewerRule).
vi.mock('@/api/governance', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/governance')>()),
  orgRulesApi: {
    list: (...a: unknown[]) => list(...a),
    save: (...a: unknown[]) => save(...a),
    setEnabled: (...a: unknown[]) => setEnabled(...a),
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

const stored = (over: Record<string, unknown> = {}, config: Record<string, unknown> = {}) => ({
  id: 'row-1',
  ruleId: 'house',
  enabled: true,
  createdAt: '',
  updatedAt: '',
  config: {
    kind: 'block',
    schemaVersion: 1,
    id: 'house',
    name: 'House',
    severity: 'warning',
    rawTypeMatch: '^SET$',
    ...config,
  },
  ...over,
})

beforeEach(() => {
  vi.clearAllMocks()
  list.mockResolvedValue([])
  save.mockResolvedValue({})
  setEnabled.mockResolvedValue({})
  libraryList.mockResolvedValue({items: [{id: 'flow-1', name: 'Main'}]})
  validateCustomRules.mockResolvedValue({
    entries: [{index: 0, id: 'r', valid: true, loaded: false}],
    valid: 1,
    invalid: 0,
  })
})

describe('OrgCustomRulesSection', () => {
  it('hides authoring controls from non-admin members', async () => {
    list.mockResolvedValue([stored()])
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

  it('creates a rule with kind and schemaVersion spelled out', async () => {
    renderSection()
    fireEvent.click(await screen.findByRole('button', {name: /new rule/i}))
    fireEvent.change(screen.getByPlaceholderText('no-http-without-retry'), {target: {value: 'my-rule'}})
    fireEvent.click(screen.getByRole('button', {name: /save rule/i}))

    await waitFor(() => expect(save).toHaveBeenCalled())
    const [orgId, config, enabled, rowId] = save.mock.lastCall ?? []
    expect([orgId, enabled, rowId]).toEqual(['org-1', true, undefined])
    expect(config).toMatchObject({kind: 'block', schemaVersion: 1, id: 'my-rule'})
  })

  it('lists a rule the analysis skips with its problem, and a newer version’s rule as not editable', async () => {
    list.mockResolvedValue([
      stored({problem: 'custom rule "house": invalid severity "critical"', problemReason: 'INVALID_SEVERITY'}),
      stored(
        {id: 'row-2', ruleId: 'future', problem: 'unknown kind "flow"', problemReason: 'UNKNOWN_KIND'},
        {kind: 'flow', id: 'future', name: 'Future'},
      ),
    ])
    renderSection()

    expect(
      await screen.findByText(/not applied to analyses: custom rule "house": invalid severity/i),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/saved by a newer version of baki\. this version cannot run or edit it/i),
    ).toBeInTheDocument()
    const edits = screen.getAllByRole('button', {name: /edit rule/i})
    expect(edits[0]).toBeEnabled()
    expect(edits[1]).toBeDisabled()
  })

  it('pauses a rule without sending its config', async () => {
    // The config may no longer validate (a severity from before severities were
    // checked); pausing it must not depend on that.
    list.mockResolvedValue([stored({problemReason: 'INVALID_SEVERITY', problem: 'bad severity'})])
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /pause rule/i}))
    // The config rides along only for a server from before rule schemas (orgRulesApi.setEnabled).
    await waitFor(() =>
      expect(setEnabled).toHaveBeenCalledWith('org-1', 'row-1', false, expect.objectContaining({id: 'house'})),
    )
    expect(save).not.toHaveBeenCalled()
  })

  it('will not create a rule under an id the org already uses', async () => {
    // The save upserts by id: a new rule named "house" would silently replace the stored one.
    list.mockResolvedValue([stored({enabled: false})])
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /new rule/i}))
    const id = screen.getByPlaceholderText('no-http-without-retry')
    fireEvent.change(id, {target: {value: ' house'}})
    fireEvent.click(screen.getByRole('button', {name: /save rule/i}))

    await waitFor(() => expect(id).toHaveAttribute('aria-invalid', 'true'))
    expect(id).toHaveAccessibleDescription(/already has a rule with this id/i)
    expect(validateCustomRules).not.toHaveBeenCalled()
    expect(save).not.toHaveBeenCalled()
  })

  it('locks the id of a saved rule and keeps a paused rule paused on save', async () => {
    list.mockResolvedValue([stored({enabled: false})])
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /edit rule/i}))
    const id = screen.getByPlaceholderText('no-http-without-retry')
    expect(id).toHaveAttribute('readonly')
    expect(screen.getByText(/fixed once saved/i)).toBeInTheDocument()

    fireEvent.change(screen.getByPlaceholderText('password|secret'), {target: {value: 'secret'}})
    fireEvent.click(screen.getByRole('button', {name: /save rule/i}))
    await waitFor(() => expect(save).toHaveBeenCalled())
    const [, config, enabled, rowId] = save.mock.lastCall ?? []
    expect([config.id, config.nameMatch, enabled, rowId]).toEqual(['house', 'secret', false, 'row-1'])
  })

  it('puts each validation problem next to its field', async () => {
    validateCustomRules.mockResolvedValue({
      entries: [
        {
          index: 0,
          id: 'house',
          valid: false,
          loaded: false,
          problems: [
            {field: 'nameMatch', reason: 'INVALID_PATTERN', message: 'missing closing )'},
            {field: 'autoFix', reason: 'UNKNOWN_AUTOFIX', message: 'unknown autoFix "x"'},
            {reason: 'NO_MATCHER', message: 'it matches every block', warning: true},
          ],
        },
      ],
      valid: 0,
      invalid: 1,
    })
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /new rule/i}))
    fireEvent.change(screen.getByPlaceholderText('no-http-without-retry'), {target: {value: 'house'}})
    fireEvent.click(screen.getByRole('button', {name: /^check$/i}))

    const nameMatch = screen.getByPlaceholderText('password|secret')
    await waitFor(() => expect(nameMatch).toHaveAttribute('aria-invalid', 'true'))
    expect(nameMatch).toHaveAccessibleDescription('missing closing )')
    expect(screen.getByRole('alert')).toHaveTextContent('unknown autoFix "x"')
    expect(screen.getByText(/note: it matches every block/i)).toBeInTheDocument()
  })

  it('accepts the built-in id a saved rule already has', async () => {
    // A rule saved before ids were checked may carry a built-in rule's id; the
    // server keeps accepting it for that rule, so the editor must not block it.
    list.mockResolvedValue([stored({ruleId: 'dead-code'}, {id: 'dead-code'})])
    validateCustomRules.mockResolvedValue({
      entries: [
        {
          index: 0,
          id: 'dead-code',
          valid: false,
          error: 'the id is a built-in rule id',
          field: 'id',
          reason: 'RESERVED_ID',
          loaded: false,
          problems: [{field: 'id', reason: 'RESERVED_ID', message: 'the id is a built-in rule id'}],
        },
      ],
      valid: 0,
      invalid: 1,
    })
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /edit rule/i}))
    fireEvent.click(screen.getByRole('button', {name: /^check$/i}))
    expect(await screen.findByText(/note: the id is a built-in rule id/i)).toBeInTheDocument()
    expect(screen.getByText(/rule is valid/i)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: /save rule/i}))
    await waitFor(() => expect(save).toHaveBeenCalled())
    expect(save.mock.lastCall?.[3]).toBe('row-1')
  })

  it('shows a save the server refuses at the field it names', async () => {
    save.mockRejectedValue(
      new ApiError('invalid rule: unknown field', 400, 'BAD_REQUEST', null, {
        field: 'config.nameMatch',
        reason: 'INVALID_PATTERN',
      }),
    )
    renderSection()

    fireEvent.click(await screen.findByRole('button', {name: /new rule/i}))
    fireEvent.change(screen.getByPlaceholderText('no-http-without-retry'), {target: {value: 'my-rule'}})
    fireEvent.click(screen.getByRole('button', {name: /save rule/i}))

    const nameMatch = screen.getByPlaceholderText('password|secret')
    await waitFor(() => expect(nameMatch).toHaveAttribute('aria-invalid', 'true'))
    expect(nameMatch).toHaveAccessibleDescription('invalid rule: unknown field')
  })
})
