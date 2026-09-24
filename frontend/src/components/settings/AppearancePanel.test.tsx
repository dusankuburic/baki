import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import AppearancePanel from './AppearancePanel'
import {useSettingsStore} from '@/stores/settingsStore'

const originalUpdateAppearance = useSettingsStore.getState().updateAppearance

describe('AppearancePanel chat text size', () => {
  beforeEach(() => {
    useSettingsStore.setState({updateAppearance: vi.fn().mockResolvedValue(undefined)} as never)
  })
  afterEach(() => {
    useSettingsStore.setState({updateAppearance: originalUpdateAppearance} as never)
  })

  it('offers three sizes and saves the chosen one', () => {
    render(<AppearancePanel />)
    expect(screen.getByRole('button', {name: 'Small'})).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Medium'})).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Large'}))
    expect(useSettingsStore.getState().updateAppearance).toHaveBeenCalledWith({chatTextSize: 'lg'})
  })
})
