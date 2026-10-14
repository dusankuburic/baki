import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act} from '@testing-library/react'
import {ProfileDetailsSection} from './ProfileDetailsSection'
import {ToastProvider} from '@/components/shared'
import {useSettingsStore, defaultSettings, __resetPersistQueueForTest} from '@/stores/settingsStore'
import {settingsApi} from '@/api/settings'

vi.mock('@/api/settings', () => ({
  settingsApi: {
    getSettings: vi.fn(),
    updateSettings: vi.fn(),
  },
}))

beforeEach(() => {
  vi.clearAllMocks()
  __resetPersistQueueForTest()
  vi.mocked(settingsApi.updateSettings).mockResolvedValue(undefined)
  useSettingsStore.setState({
    settings: {...defaultSettings, profile: {bio: '', jobTitle: '', timezone: ''}},
    isLoaded: true,
  })
})

afterEach(() => {
  vi.useRealTimers()
})

function renderSection() {
  return render(
    <ToastProvider>
      <ProfileDetailsSection />
    </ToastProvider>,
  )
}

describe('ProfileDetailsSection', () => {
  it('shows the stored identity-card fields', () => {
    useSettingsStore.setState({
      settings: {...defaultSettings, profile: {bio: 'RPA developer', jobTitle: 'Analyst', timezone: 'Europe/Belgrade'}},
      isLoaded: true,
    })
    renderSection()
    expect(screen.getByText('RPA developer')).toBeInTheDocument()
    expect(screen.getByText('Analyst')).toBeInTheDocument()
    expect(screen.getByText('Europe/Belgrade')).toBeInTheDocument()
  })

  it('saves the fields through the settings profile section', async () => {
    vi.useFakeTimers()
    renderSection()
    fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
    fireEvent.change(screen.getByLabelText('Bio'), {target: {value: 'Automating the boring parts.'}})
    fireEvent.change(screen.getByLabelText('Job title'), {target: {value: 'RPA Developer'}})
    fireEvent.change(screen.getByLabelText('Timezone'), {target: {value: 'Europe/Belgrade'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save changes'}))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100)
    })
    vi.useRealTimers()
    await waitFor(() => expect(screen.queryByLabelText('Bio')).not.toBeInTheDocument())
    expect(settingsApi.updateSettings).toHaveBeenCalled()
    const saved = useSettingsStore.getState().settings.profile
    expect(saved).toEqual({bio: 'Automating the boring parts.', jobTitle: 'RPA Developer', timezone: 'Europe/Belgrade'})
  })

  it('refuses a bio over the cap and keeps the form open', async () => {
    renderSection()
    fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
    fireEvent.change(screen.getByLabelText('Bio'), {target: {value: 'x'.repeat(501)}})
    fireEvent.blur(screen.getByLabelText('Bio'))
    fireEvent.click(screen.getByRole('button', {name: 'Save changes'}))
    expect(await screen.findByText('Bio must be at most 500 characters')).toBeInTheDocument()
    expect(settingsApi.updateSettings).not.toHaveBeenCalled()
    expect(screen.getByLabelText('Bio')).toBeInTheDocument()
  })
})
