import {describe, it, expect, vi, beforeEach} from 'vitest'
import {act, render, screen, fireEvent} from '@testing-library/react'
import {__resetBuildVersionForTest, noteServerVersion} from '@/lib/buildVersion'
import {reloadPage} from '@/lib/chunkLoad'
import ReloadBanner from './ReloadBanner'

vi.mock('@/lib/chunkLoad', () => ({reloadPage: vi.fn()}))

beforeEach(() => {
  vi.clearAllMocks()
  sessionStorage.clear()
  __resetBuildVersionForTest()
})

describe('ReloadBanner', () => {
  it('shows nothing while the server runs this build', () => {
    render(<ReloadBanner />)
    act(() => noteServerVersion('v1.0.0', 'v1.0.0'))
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('says a new version is available once the server runs another build, and Reload reloads', () => {
    render(<ReloadBanner />)
    act(() => noteServerVersion('v1.1.0', 'v1.0.0'))
    expect(screen.getByRole('status')).toHaveTextContent('A new version of the app is available.')
    fireEvent.click(screen.getByRole('button', {name: 'Reload'}))
    expect(reloadPage).toHaveBeenCalledTimes(1)
  })

  it('can be dismissed: the same build does not bring it back, a later deploy does', () => {
    render(<ReloadBanner />)
    act(() => noteServerVersion('v1.1.0', 'v1.0.0'))
    fireEvent.click(screen.getByRole('button', {name: 'Dismiss'}))
    expect(screen.queryByRole('status')).toBeNull()
    act(() => noteServerVersion('v1.1.0', 'v1.0.0'))
    expect(screen.queryByRole('status')).toBeNull()
    act(() => noteServerVersion('v1.2.0', 'v1.0.0'))
    expect(screen.getByRole('status')).toBeTruthy()
    expect(reloadPage).not.toHaveBeenCalled()
  })
})
