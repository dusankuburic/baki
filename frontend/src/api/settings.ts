import {request} from './client'
import type {AppSettings} from '@/types/domain'

export const settingsApi = {
  getSettings: (): Promise<AppSettings> =>
    request('/api/system/settings', undefined, 'GET'),

  updateSettings: (settings: AppSettings): Promise<void> =>
    request('/api/system/settings', settings),
}
