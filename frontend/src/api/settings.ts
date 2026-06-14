import {request} from './client'
import type {AppSettings} from '@/types'

export const settingsApi = {
  getSettings: (): Promise<AppSettings> =>
    request('/api/system/settings', undefined, 'GET'),

  updateSettings: (settings: AppSettings): Promise<void> =>
    request('/api/system/settings', settings),

  getUserSettings: (): Promise<AppSettings> =>
    request('/api/system/settings/user', undefined, 'GET'),

  updateUserSettings: (settings: AppSettings): Promise<void> =>
    request('/api/system/settings/user', settings),

  getOrgSettings: (orgId: string): Promise<AppSettings> =>
    request(`/api/system/settings/org/${orgId}`, undefined, 'GET'),

  updateOrgSettings: (orgId: string, settings: AppSettings): Promise<void> =>
    request(`/api/system/settings/org/${orgId}`, settings),
}
