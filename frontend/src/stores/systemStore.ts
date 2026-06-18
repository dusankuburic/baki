import {create} from 'zustand'
import {systemApi} from '@/api'
import type {AppInfo} from '@/types'
import {logger} from '@/lib/logger'

interface SystemState {
  info: AppInfo | null
  isLoaded: boolean
  loadInfo: () => Promise<void>
}

export const useSystemStore = create<SystemState>((set) => ({
  info: null,
  isLoaded: false,

  loadInfo: async () => {
    try {
      const info = await systemApi.appInfo()
      set({info, isLoaded: true})
    } catch (err) {
      logger.warn('Failed to load system info', err)
      set({isLoaded: true})
    }
  },
}))
