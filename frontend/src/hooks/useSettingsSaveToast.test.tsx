import {describe, it, expect, beforeEach, vi} from 'vitest'
import {act, render, screen} from '@testing-library/react'
import {ToastProvider} from '@/components/shared'
import {__resetPersistQueueForTest, useSettingsStore} from '@/stores/settingsStore'
import {settingsApi} from '@/api'
import {ApiError} from '@/api/client'
import {useSettingsSaveToast} from './useSettingsSaveToast'

function Harness() {
  useSettingsSaveToast()
  return null
}

beforeEach(() => useSettingsStore.setState({saveError: null}))

// R-frontend-10: a refused settings save is never silent. Whichever section it carried, a toast says so in the
// server's words, once per refusal.
describe('useSettingsSaveToast', () => {
  it('shows a toast for each refused save', () => {
    render(
      <ToastProvider>
        <Harness />
      </ToastProvider>,
    )
    expect(screen.queryByText(/were not saved/)).toBeNull()

    act(() => {
      useSettingsStore.setState({
        saveError: {
          message: 'invalid settings: profile.bio is 600 characters, at most 500',
          code: 'BAD_REQUEST',
          sections: ['profile'],
          at: 1,
        },
      })
    })
    expect(screen.getByText('Your settings were not saved')).toBeInTheDocument()
    expect(screen.getByText(/profile\.bio is 600 characters/)).toBeInTheDocument()

    act(() => useSettingsStore.setState({saveError: null}))
    expect(screen.getAllByText('Your settings were not saved')).toHaveLength(1)
  })
})

// End to end: five keystrokes coalesced into one refused write raise one toast.
describe('useSettingsSaveToast with the store', () => {
  it('raises one toast for one refused write, however many changes it carried', async () => {
    vi.useFakeTimers()
    try {
      vi.spyOn(settingsApi, 'updateSettings').mockRejectedValue(new ApiError('nope', 400, 'BAD_REQUEST'))
      vi.spyOn(settingsApi, 'getSettings').mockReturnValue(new Promise(() => {}))
      __resetPersistQueueForTest()
      render(
        <ToastProvider>
          <Harness />
        </ToastProvider>,
      )
      const store = useSettingsStore.getState()
      await act(async () => {
        const writes = ['a', 'ab', 'abc', 'abcd', 'abcde'].map(v => store.updateAI({systemPromptSuffix: v}))
        await vi.advanceTimersByTimeAsync(1000)
        await Promise.all(writes)
      })
      expect(screen.getAllByText('Your settings were not saved')).toHaveLength(1)
    } finally {
      vi.useRealTimers()
      vi.restoreAllMocks()
    }
  })
})
