import {request, requestValidated} from './client'
import {AppSettingsSchema} from './schemas'
import type {AppSettings} from '@/types'

export const settingsApi = {
  getSettings: (): Promise<AppSettings> =>
    requestValidated('/api/system/settings', AppSettingsSchema, undefined, 'GET'),

  updateSettings: (settings: AppSettings): Promise<void> => request('/api/system/settings', settings),

  getUserSettings: (): Promise<AppSettings> =>
    requestValidated('/api/system/settings/user', AppSettingsSchema, undefined, 'GET'),

  updateUserSettings: (settings: AppSettings): Promise<void> => request('/api/system/settings/user', settings),

  getOrgSettings: (orgId: string): Promise<AppSettings> =>
    requestValidated(`/api/system/settings/org/${orgId}`, AppSettingsSchema, undefined, 'GET'),

  updateOrgSettings: (orgId: string, settings: AppSettings): Promise<void> =>
    request(`/api/system/settings/org/${orgId}`, settings),
}
