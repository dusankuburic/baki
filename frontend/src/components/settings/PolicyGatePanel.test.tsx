import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act} from '@testing-library/react'
import PolicyGatePanel from './PolicyGatePanel'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared'
import {useOrgStore} from '@/stores/orgStore'
import {ResponseValidationError} from '@/api/client'
import {z} from 'zod'

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
  it('says the list could not be loaded (not that there are none) and retries', async () => {
    const malformed = z.array(z.object({id: z.string()})).safeParse([{}]).error!
    listPolicies.mockRejectedValueOnce(new ResponseValidationError('/api/analysis/policy/list', malformed))
    renderPanel()
    expect(await screen.findByText("Couldn't load the policies.")).toBeInTheDocument()
    expect(screen.queryByText(/No policies yet/)).toBeNull()

    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(await screen.findByRole('button', {name: /Release gate/})).toBeInTheDocument()
    expect(screen.queryByText("Couldn't load the policies.")).toBeNull()
  })

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

  // The panel edits names, the gate and rule toggles only; a save replaces the stored policy wholesale, so every
  // edit must send back the waivers, the budgets and the per-rule caps it does not show.
  it('sends the waivers and budgets back with every edit', async () => {
    const waivers = [{findingKey: 'fp-1', reason: 'accepted risk', expiresAt: '2027-01-01T00:00:00Z'}]
    const budgeted = {
      ...policy,
      rules: [{ruleId: 'todo-comment', enabled: true, maxCount: 3}],
      waivers,
      maxErrors: 0,
      maxWarnings: 5,
      maxInfo: 10,
    }
    getRules.mockResolvedValue([
      {
        id: 'todo-comment',
        name: 'TODO comment',
        description: '',
        defaultSeverity: 'info',
        category: 'x',
        enabled: true,
      },
      {
        id: 'unhandled-error',
        name: 'Unhandled error',
        description: '',
        defaultSeverity: 'warning',
        category: 'x',
        enabled: true,
      },
    ])
    listPolicies.mockResolvedValue([budgeted])
    savePolicy.mockImplementation((p: unknown) => Promise.resolve(p))
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: /Release gate/}))

    fireEvent.click(await screen.findByRole('switch', {name: 'Toggle Unhandled error'}))
    await waitFor(() => expect(savePolicy).toHaveBeenCalledTimes(1))
    expect(savePolicy.mock.calls[0]![0]).toEqual({
      ...budgeted,
      rules: [...budgeted.rules, {ruleId: 'unhandled-error', enabled: true}],
    })

    fireEvent.change(screen.getByRole('textbox', {name: 'Name'}), {target: {value: 'Release gate 2'}})
    await waitFor(() => expect(savePolicy).toHaveBeenCalledTimes(2))
    expect(savePolicy.mock.calls[1]![0]).toMatchObject({waivers, maxErrors: 0, maxWarnings: 5, maxInfo: 10})
    expect(savePolicy.mock.calls[1]![0].rules[0]).toEqual({ruleId: 'todo-comment', enabled: true, maxCount: 3})
  })

  // A policy a server before v48 stored under the empty id (one of its replicas may still, mid-deploy): the server
  // gives it an id on its first edit. The panel replaces the entry it edited, so it shows the policy once, and edits
  // it under the new id from then on (each edit under the empty id would be a new policy).
  it('takes the id the server gives a policy listed without one', async () => {
    listPolicies.mockResolvedValue([{...policy, id: ''}])
    getRules.mockResolvedValue([
      {id: 'r1', name: 'Rule one', description: '', defaultSeverity: 'info', category: 'x', enabled: true},
      {id: 'r2', name: 'Rule two', description: '', defaultSeverity: 'info', category: 'x', enabled: true},
    ])
    savePolicy.mockImplementation((p: {id: string}) => Promise.resolve({...p, id: p.id || 'p-new'}))
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: /Release gate/}))
    fireEvent.click(await screen.findByRole('switch', {name: 'Toggle Rule one'}))
    await waitFor(() => expect(savePolicy).toHaveBeenCalledTimes(1))
    expect(savePolicy.mock.calls[0]![0]).toMatchObject({id: ''})
    await waitFor(() => expect(screen.getAllByRole('button', {name: /Release gate/})).toHaveLength(1))

    fireEvent.click(await screen.findByRole('switch', {name: 'Toggle Rule two'}))
    await waitFor(() => expect(savePolicy).toHaveBeenCalledTimes(2))
    expect(savePolicy.mock.calls[1]![0]).toMatchObject({id: 'p-new'})
    expect(savePolicy.mock.calls[1]![0].rules).toEqual([
      {ruleId: 'r1', enabled: true},
      {ruleId: 'r2', enabled: true},
    ])
  })

  // Two edits of a policy listed without an id, the second before the first is answered: the server gives the
  // policy an id on the first save only, so the second must wait for it and go out under that id (sent without one
  // it is a second policy, a gate the panel never shows). Both edits land on the one policy, the panel shows it.
  it('sends a quick second edit of a policy listed without an id under the id the first save gives it', async () => {
    type Saved = {id: string; rules: {ruleId: string; enabled: boolean}[]}
    listPolicies.mockResolvedValue([{...policy, id: ''}])
    getRules.mockResolvedValue([
      {id: 'r1', name: 'Rule one', description: '', defaultSeverity: 'info', category: 'x', enabled: true},
      {id: 'r2', name: 'Rule two', description: '', defaultSeverity: 'info', category: 'x', enabled: true},
    ])
    // The server: the organisation's id-less policy takes the id 'p-a' on the first save without one; a later save
    // without one is a new policy.
    const stored = new Map<string, Saved>([['', {...policy, id: ''}]])
    const answers: (() => void)[] = []
    let fresh = 0
    savePolicy.mockImplementation(
      (p: Saved) =>
        new Promise(resolve =>
          answers.push(() => {
            let id = p.id
            if (id === '') {
              id = stored.delete('') ? 'p-a' : `p-new-${++fresh}`
            }
            stored.set(id, {...p, id})
            resolve({...p, id})
          }),
        ),
    )
    const answerNext = async () => {
      await act(async () => {
        answers.shift()!()
      })
    }
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: /Release gate/}))
    fireEvent.click(await screen.findByRole('switch', {name: 'Toggle Rule one'}))
    fireEvent.click(screen.getByRole('switch', {name: 'Toggle Rule two'}))
    await waitFor(() => expect(answers).toHaveLength(1))
    await answerNext()
    await waitFor(() => expect(savePolicy).toHaveBeenCalledTimes(2))
    await answerNext()

    expect(savePolicy.mock.calls.map(c => (c[0] as Saved).id)).toEqual(['', 'p-a'])
    expect([...stored.keys()]).toEqual(['p-a'])
    expect(stored.get('p-a')!.rules).toEqual([
      {ruleId: 'r1', enabled: true},
      {ruleId: 'r2', enabled: true},
    ])
    const rows = screen.getAllByRole('button', {name: /Release gate/})
    expect(rows).toHaveLength(1)
    expect(rows[0]).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByRole('switch', {name: 'Toggle Rule one'})).toHaveAttribute('aria-checked', 'true')
    expect(screen.getByRole('switch', {name: 'Toggle Rule two'})).toHaveAttribute('aria-checked', 'true')
  })

  // Each organisation may list its own id-less policy. A save answered after the user switched organisation belongs
  // to the one it was made in: it neither replaces the new organisation's policy nor holds back or renames its edits,
  // and a failed one does not reload the old organisation's list into the panel.
  describe('after an organisation switch', () => {
    type Saved = {id: string; orgId: string}
    const audit = {id: '', orgId: 'org-2', name: 'Audit gate', description: '', rules: [], gateSeverity: 'error'}
    let answers: ((ok: boolean) => void)[]
    let deleteAnswers: (() => void)[]
    beforeEach(() => {
      answers = []
      deleteAnswers = []
      deletePolicy.mockImplementation(
        () => new Promise(resolve => deleteAnswers.push(() => resolve(undefined))),
      )
      listPolicies.mockImplementation((org: string) =>
        Promise.resolve(org === 'org-2' ? [audit] : [{...policy, id: ''}]),
      )
      getRules.mockResolvedValue([
        {id: 'r1', name: 'Rule one', description: '', defaultSeverity: 'info', category: 'x', enabled: true},
      ])
      savePolicy.mockImplementation(
        (p: Saved) =>
          new Promise((resolve, reject) =>
            answers.push(ok => (ok ? resolve({...p, id: p.id || `id-${p.orgId}`}) : reject(new Error('boom')))),
          ),
      )
    })
    const editThenSwitch = async () => {
      renderPanel()
      fireEvent.click(await screen.findByRole('button', {name: /Release gate/}))
      fireEvent.click(await screen.findByRole('switch', {name: 'Toggle Rule one'}))
      await waitFor(() => expect(answers).toHaveLength(1))
      await act(async () => useOrgStore.setState({activeOrgId: 'org-2'}))
      const row = await screen.findByRole('button', {name: /Audit gate/})
      if (row.getAttribute('aria-expanded') !== 'true') fireEvent.click(row)
    }
    const sent = () => savePolicy.mock.calls.map(c => [(c[0] as Saved).id, (c[0] as Saved).orgId])

    it('leaves the new organisation’s policy shown and edited under its own id', async () => {
      await editThenSwitch()
      await act(async () => answers.shift()!(true))
      expect(screen.getByRole('button', {name: /Audit gate/})).toBeInTheDocument()
      expect(screen.queryByRole('button', {name: /Release gate/})).toBeNull()
      fireEvent.click(screen.getByRole('switch', {name: 'Toggle Rule one'}))
      await waitFor(() => expect(savePolicy).toHaveBeenCalledTimes(2))
      expect(sent()).toEqual([
        ['', 'org-1'],
        ['', 'org-2'],
      ])
    })

    it('sends the new organisation’s edit at once while the old one’s save is in flight', async () => {
      await editThenSwitch()
      fireEvent.click(screen.getByRole('switch', {name: 'Toggle Rule one'}))
      await waitFor(() => expect(savePolicy).toHaveBeenCalledTimes(2))
      await act(async () => answers.shift()!(true))
      await act(async () => answers.shift()!(true))
      expect(sent()).toEqual([
        ['', 'org-1'],
        ['', 'org-2'],
      ])
      expect(screen.getAllByRole('button', {name: /gate/})).toHaveLength(1)
      expect(screen.getByRole('button', {name: /Audit gate/})).toBeInTheDocument()
    })

    it('does not reload the old organisation’s list when its save fails', async () => {
      await editThenSwitch()
      await act(async () => answers.shift()!(false))
      expect(listPolicies.mock.calls.map(c => c[0])).toEqual(['org-1', 'org-2'])
      expect(screen.getByRole('button', {name: /Audit gate/})).toBeInTheDocument()
      expect(screen.queryByRole('button', {name: /Release gate/})).toBeNull()
    })

    it('does not append a created policy to the new organisation after switch', async () => {
      renderPanel()
      await screen.findByRole('button', {name: /Release gate/})
      fireEvent.click(screen.getByRole('button', {name: 'New Policy'}))
      await waitFor(() => expect(answers).toHaveLength(1))
      await act(async () => useOrgStore.setState({activeOrgId: 'org-2'}))
      await screen.findByRole('button', {name: /Audit gate/})
      await act(async () => answers.shift()!(true))
      expect(screen.getAllByRole('button', {name: /gate/})).toHaveLength(1)
      expect(screen.getByRole('button', {name: /Audit gate/})).toBeInTheDocument()
      expect(screen.queryByRole('button', {name: /Policy 2/})).toBeNull()
    })

    it('does not filter the new organisation’s list when a delete answers after switch', async () => {
      renderPanel()
      await screen.findByRole('button', {name: /Release gate/})
      fireEvent.click(screen.getByRole('button', {name: 'Delete policy'}))
      const confirmBtn = await screen.findByRole('button', {name: 'Delete'})
      fireEvent.click(confirmBtn)
      await waitFor(() => expect(deleteAnswers).toHaveLength(1))
      await act(async () => useOrgStore.setState({activeOrgId: 'org-2'}))
      await screen.findByRole('button', {name: /Audit gate/})
      await act(async () => deleteAnswers.shift()!())
      expect(screen.getByRole('button', {name: /Audit gate/})).toBeInTheDocument()
      expect(screen.queryByText(/No policies yet/)).toBeNull()
    })
  })

  // An edit shows at once; when its save fails the panel shows what is stored again.
  it('shows the stored policy again when a save fails', async () => {
    getRules.mockResolvedValue([
      {id: 'r1', name: 'Rule one', description: '', defaultSeverity: 'info', category: 'x', enabled: true},
    ])
    savePolicy.mockRejectedValue(new Error('boom'))
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: /Release gate/}))
    fireEvent.click(await screen.findByRole('switch', {name: 'Toggle Rule one'}))
    await waitFor(() => expect(listPolicies).toHaveBeenCalledTimes(2))
    await waitFor(() =>
      expect(screen.getByRole('switch', {name: 'Toggle Rule one'})).toHaveAttribute('aria-checked', 'false'),
    )
  })

  // A new policy has its id from the start, so a create never lands on an id-less policy the server still holds.
  it('creates a policy with an id of its own', async () => {
    listPolicies.mockResolvedValue([])
    savePolicy.mockImplementation((p: unknown) => Promise.resolve(p))
    renderPanel()
    fireEvent.click(await screen.findByRole('button', {name: 'New Policy'}))
    await waitFor(() => expect(savePolicy).toHaveBeenCalledTimes(1))
    expect(savePolicy.mock.calls[0]![0].id).toMatch(/^[0-9a-f-]{36}$/)
  })

  // The server evaluates a gate it does not know as report-only (analyzer.EvaluatePolicy); the badge says the same.
  it('shows a gate severity it does not know as report only', async () => {
    listPolicies.mockResolvedValue([{...policy, gateSeverity: 'critical'}])
    renderPanel()
    const expand = await screen.findByRole('button', {name: /Release gate/})
    expect(expand).toHaveTextContent('Report only')
    expect(expand).not.toHaveTextContent('Info+')
  })

  it('reads the gate as the server does, trimmed and in any case', async () => {
    listPolicies.mockResolvedValue([{...policy, gateSeverity: ' Error '}])
    renderPanel()
    expect(await screen.findByRole('button', {name: /Release gate/})).toHaveTextContent('Error+')
  })
})
