import {describe, it, expect} from 'vitest'
import {render, screen, within} from '@testing-library/react'
import {BatchLibraryFindings} from './BatchLibraryFindings'
import type {LibraryFinding, LibraryReport} from '@/types'

const finding = (over: Partial<LibraryFinding> = {}): LibraryFinding => ({
  id: '',
  ruleId: 'duplicate-subflow-across-flows',
  severity: 'info',
  title: 'Subflow duplicated across flows',
  description: 'Same steps as a subflow of another flow.',
  blockId: '',
  subflowId: '',
  flowId: 'a',
  flowName: 'invoices',
  subflowName: 'Login',
  ...over,
})

const report = (over: Partial<LibraryReport> = {}): LibraryReport => ({
  findings: [],
  flows: 3,
  rulesRun: 1,
  generatedAt: '2026-10-14T00:00:00Z',
  durationMs: 1,
  ...over,
})

const region = () => screen.getByRole('region', {name: 'Across flows'})

describe('BatchLibraryFindings', () => {
  it('groups the findings by rule, with the count and severity of each group', () => {
    render(
      <BatchLibraryFindings
        report={report({
          findings: [
            finding(),
            finding({ruleId: 'shared-credential', title: 'Credential reused across flows', severity: 'warning'}),
            finding({flowId: 'b', flowName: 'orders', subflowName: 'SignIn'}),
          ],
        })}
      />,
    )
    const groups = within(region())
      .getAllByRole('listitem')
      .filter(li => li.parentElement?.parentElement === region())
    expect(groups.map(g => within(g).getAllByRole('listitem').length)).toEqual([2, 1])
    expect(within(groups[0]!).getByText('2×')).toBeInTheDocument()
    expect(within(groups[0]!).getByText('Info')).toBeInTheDocument()
    expect(within(groups[1]!).getByText('Credential reused across flows')).toBeInTheDocument()
    expect(within(groups[1]!).getByText('Warn')).toBeInTheDocument()
    expect(screen.getByText('3 findings')).toBeInTheDocument()
    expect(screen.getByText('3 flows compared')).toBeInTheDocument()
  })

  it('names a whole-flow finding by its flow, a nameless flow by its id, and every related place', () => {
    render(
      <BatchLibraryFindings
        report={report({
          findings: [
            finding({subflowName: undefined, flowName: undefined, flowId: 'C:/flows/x.txt', related: []}),
            finding({
              related: [
                {flowId: 'b', flowName: 'orders', subflowName: 'SignIn'},
                {flowId: 'c', flowName: 'refunds'},
              ],
            }),
          ],
        })}
      />,
    )
    expect(screen.getByText('C:/flows/x.txt')).toBeInTheDocument()
    expect(screen.getByText('invoices › Login')).toBeInTheDocument()
    expect(screen.getByText('Also in orders › SignIn and refunds')).toBeInTheDocument()
    expect(screen.getAllByText(/^Also in/)).toHaveLength(1)
  })

  it('says when the pass found nothing, left flows out or skipped a rule', () => {
    render(<BatchLibraryFindings report={report({findings: null, flows: 1, truncated: true, rulesSkipped: 1})} />)
    expect(within(region()).getByText('Nothing found across these flows.')).toBeInTheDocument()
    expect(screen.getByText('0 findings')).toBeInTheDocument()
    expect(screen.getByText('1 flow compared')).toBeInTheDocument()
    expect(screen.getByText(/some flows or findings were left out/)).toBeInTheDocument()
    expect(screen.getByText("1 rule couldn't run.")).toBeInTheDocument()
  })

  it('marks a preview rule', () => {
    render(<BatchLibraryFindings report={report({findings: [finding({stage: 'preview'})]})} />)
    expect(within(region()).getByText('Preview')).toBeInTheDocument()
  })
})
