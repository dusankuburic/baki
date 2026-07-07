import {describe, it, expect, vi, beforeEach} from 'vitest'
import {useSystemStore} from './systemStore'
import type {AppInfo} from '@/types'

vi.mock('@/api', () => ({systemApi: {appInfo: vi.fn()}}))

import {systemApi} from '@/api'

const mockAppInfo = systemApi.appInfo as ReturnType<typeof vi.fn>
const initialState = useSystemStore.getState()

beforeEach(() => {
  useSystemStore.setState(initialState, true)
  vi.resetAllMocks()
})

describe('loadInfo', () => {
  it('populates info and marks isLoaded on success', async () => {
    const info = {version: '1.0.0'} as AppInfo
    mockAppInfo.mockResolvedValue(info)

    await useSystemStore.getState().loadInfo()

    const s = useSystemStore.getState()
    expect(s.info).toEqual(info)
    expect(s.isLoaded).toBe(true)
  })

  it('marks isLoaded true even when the request fails, leaving info null', async () => {
    mockAppInfo.mockRejectedValue(new Error('network down'))

    await useSystemStore.getState().loadInfo()

    const s = useSystemStore.getState()
    expect(s.info).toBeNull()
    expect(s.isLoaded).toBe(true)
  })
})
