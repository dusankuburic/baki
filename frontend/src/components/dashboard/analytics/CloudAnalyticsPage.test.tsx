import {describe, it, expect, vi, beforeEach, beforeAll} from 'vitest'
import {render, screen, waitFor, fireEvent, within, act} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useAnalyticsStore} from '@/stores/analyticsStore'
import {useOrgStore} from '@/stores/orgStore'
import {mockDashboardAnalytics} from '@/testing/fixtures/dashboardAnalytics'
import CloudAnalyticsPage from './CloudAnalyticsPage'

const getAnalytics = vi.fn()

vi.mock('@/api', () => ({
  dashboardApi: {
    getAnalytics: (...a: unknown[]) => getAnalytics(...a),
  },
  libraryApi: {
    getContent: vi.fn(),
  },
}))

vi.mock('recharts', async importOriginal => {
  const actual = await importOriginal<typeof import('recharts')>()
  return {
    ...actual,
    ResponsiveContainer: ({children}: {children: React.ReactNode}) => (
      <div style={{width: 600, height: 240}}>{children}</div>
    ),
  }
})

function renderPage() {
  return render(
    <ToastProvider>
      <CloudAnalyticsPage />
    </ToastProvider>,
  )
}

function lastQuery(): URLSearchParams {
  const call = getAnalytics.mock.calls.at(-1)
  return new URLSearchParams(String(call?.[0] ?? ''))
}

beforeAll(() => {
  // jsdom has no scrollIntoView; the section nav and Home's "Details" use it.
  Element.prototype.scrollIntoView = vi.fn()
})

beforeEach(() => {
  vi.clearAllMocks()
  getAnalytics.mockResolvedValue(mockDashboardAnalytics())
  useAnalysisStore.getState().reset()
  useAnalyticsStore.getState().reset()
  useAnalyticsStore.setState({nonce: 0})
  useOrgStore.setState({
    organisations: [{id: 'org-1', name: 'Acme', ownerId: 'u', members: [], createdAt: '', updatedAt: ''}],
    activeOrgId: null,
  })
})

describe('CloudAnalyticsPage', () => {
  it('requests the default window and renders every section', async () => {
    renderPage()
    expect(getAnalytics).toHaveBeenCalledTimes(1)
    const q = lastQuery()
    expect(q.get('range')).toBe('30d')
    expect(q.get('scope')).toBe('all')
    expect(q.get('compare')).toBe('1')
    expect(q.get('tz')).toBeTruthy()

    expect(await screen.findByRole('heading', {name: 'Overview'})).toBeInTheDocument()
    for (const name of ['Trends', 'Findings', 'Flows', 'Triage', 'AI usage', 'Activity', 'Security']) {
      expect(screen.getByRole('heading', {name})).toBeInTheDocument()
    }
    // Headline tiles carry the value and the comparison.
    expect(screen.getByText('72')).toBeInTheDocument()
    expect(screen.getAllByText(/vs previous 30 days/).length).toBeGreaterThan(0)
    // The rule chips carry facet counts.
    expect(screen.getByRole('button', {name: 'Missing error handling, 12 findings'})).toHaveAttribute(
      'aria-pressed',
      'false',
    )
  })

  it('toggling a severity chip refetches with the filter and keeps the page up meanwhile', async () => {
    renderPage()
    await screen.findByRole('heading', {name: 'Overview'})

    let resolveSecond: (v: unknown) => void = () => {}
    getAnalytics.mockImplementationOnce(() => new Promise(r => (resolveSecond = r)))
    fireEvent.click(screen.getByRole('button', {name: 'Errors, 6 findings'}))

    await waitFor(() => expect(getAnalytics).toHaveBeenCalledTimes(2))
    expect(lastQuery().get('severity')).toBe('error')
    expect(screen.getByRole('button', {name: 'Errors, 6 findings'})).toHaveAttribute('aria-pressed', 'true')
    // Previous charts stay on screen, marked busy, while the new payload loads.
    expect(screen.getByRole('heading', {name: 'Overview'})).toBeInTheDocument()
    expect(screen.getByRole('progressbar', {name: 'Updating analytics'})).toHaveClass('is-active')
    expect(document.querySelectorAll('.chart-card[aria-busy="true"]').length).toBeGreaterThan(0)

    await act(async () => {
      resolveSecond(mockDashboardAnalytics({derived: true}))
    })
    // Once idle the bar is aria-hidden (and so nameless to a role query).
    await waitFor(() => expect(document.querySelector('.analytics-progress')).not.toHaveClass('is-active'))
    expect(screen.getByText(/Showing Last 30 days · All my flows · Errors · vs previous period/)).toBeInTheDocument()
    expect(screen.getByText(/map each flow’s per-rule counts/)).toBeInTheDocument()
  })

  it('applies a custom range only when it is valid', async () => {
    renderPage()
    await screen.findByRole('heading', {name: 'Overview'})

    fireEvent.click(screen.getByRole('radio', {name: 'Custom'}))
    fireEvent.change(screen.getByLabelText('From'), {target: {value: '2026-09-10'}})
    fireEvent.change(screen.getByLabelText('To'), {target: {value: '2026-09-01'}})
    fireEvent.click(screen.getByRole('button', {name: 'Apply'}))
    expect(screen.getByRole('alert')).toHaveTextContent('The start must not be after the end.')
    expect(getAnalytics).toHaveBeenCalledTimes(1)

    fireEvent.change(screen.getByLabelText('To'), {target: {value: '2026-09-12'}})
    fireEvent.click(screen.getByRole('button', {name: 'Apply'}))
    await waitFor(() => expect(getAnalytics).toHaveBeenCalledTimes(2))
    const q = lastQuery()
    expect(q.get('range')).toBe('custom')
    expect(q.get('from')).toBe('2026-09-10')
    expect(q.get('to')).toBe('2026-09-12')
  })

  it('scopes to an organisation and picks flows', async () => {
    renderPage()
    await screen.findByRole('heading', {name: 'Overview'})

    fireEvent.click(screen.getByRole('button', {name: /^Scope:/}))
    fireEvent.click(await screen.findByRole('menuitem', {name: 'Acme'}))
    await waitFor(() => expect(lastQuery().get('scope')).toBe('org'))
    expect(lastQuery().get('orgId')).toBe('org-1')

    fireEvent.click(screen.getByRole('button', {name: 'Flows: All flows'}))
    const list = screen.getByRole('listbox', {name: 'Flows in scope'})
    fireEvent.click(within(list).getByRole('option', {name: /Invoice sync/}))
    await waitFor(() => expect(lastQuery().get('flows')).toBe('flow-1'))
    expect(screen.getByRole('button', {name: 'Flows: 1 flow'})).toHaveTextContent('1 flow')
  })

  it('shows the table twin of a chart with every row', async () => {
    renderPage()
    const card = (await screen.findByRole('heading', {name: 'Findings by rule'})).closest('section') as HTMLElement
    fireEvent.click(within(card).getByRole('radio', {name: 'Table'}))
    expect(within(card).getAllByRole('row')).toHaveLength(1 + 4)
    expect(within(card).getAllByText('custom-x').length).toBeGreaterThan(0)
  })

  it('shows an error state with retry when the first load fails', async () => {
    getAnalytics.mockRejectedValueOnce(new Error('boom'))
    renderPage()
    expect(await screen.findByText("Couldn't load analytics")).toBeInTheDocument()
    expect(screen.getByText('boom')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    await waitFor(() => expect(getAnalytics).toHaveBeenCalledTimes(2))
    expect(await screen.findByRole('heading', {name: 'Overview'})).toBeInTheDocument()
  })

  it('keeps the charts and shows a notice when a refetch fails', async () => {
    renderPage()
    await screen.findByRole('heading', {name: 'Overview'})
    getAnalytics.mockRejectedValueOnce(new Error('later boom'))
    fireEvent.click(screen.getByRole('radio', {name: '7 days'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('later boom')
    expect(screen.getByRole('heading', {name: 'Overview'})).toBeInTheDocument()
  })

  it('names the sections that could not be loaded', async () => {
    getAnalytics.mockResolvedValue(mockDashboardAnalytics({partial: ['ai', 'activity']}))
    renderPage()
    expect(await screen.findByText(/Some sections could not be loaded/)).toHaveTextContent(
      'Some sections could not be loaded: AI usage, Activity.',
    )
  })

  it('shows an empty state when the scope has no flows', async () => {
    getAnalytics.mockResolvedValue(mockDashboardAnalytics({flowsInScope: 0}))
    renderPage()
    expect(await screen.findByText('No flows in this scope')).toBeInTheDocument()
    expect(screen.queryByRole('heading', {name: 'Overview'})).toBeNull()
  })

  it('scrolls to the section Home asked for, once', async () => {
    useAnalyticsStore.getState().requestSection('analytics-findings')
    renderPage()
    await screen.findByRole('heading', {name: 'Overview'})
    await waitFor(() => expect(useAnalyticsStore.getState().pendingSection).toBeNull())
    await waitFor(() => expect(Element.prototype.scrollIntoView).toHaveBeenCalled())
  })

  it('refetches when an analysis finishes', async () => {
    renderPage()
    await screen.findByRole('heading', {name: 'Overview'})
    useAnalysisStore.setState({isAnalyzing: true})
    await new Promise(r => setTimeout(r, 10))
    useAnalysisStore.setState({isAnalyzing: false})
    await waitFor(() => expect(getAnalytics).toHaveBeenCalledTimes(2))
  })
})
