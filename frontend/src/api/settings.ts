import {request, requestValidated} from './client'
import {getAppSettingsSchema} from './schemas'
import type {AppSettings} from '@/types'

export const settingsApi = {
  getSettings: async (): Promise<AppSettings> =>
    requestValidated('/api/system/settings', await getAppSettingsSchema(), {method: 'GET'}),

  updateSettings: (settings: AppSettings): Promise<void> => request('/api/system/settings', {body: settings}),

  getUserSettings: async (): Promise<AppSettings> =>
    requestValidated('/api/system/settings/user', await getAppSettingsSchema(), {method: 'GET'}),

  updateUserSettings: (settings: AppSettings): Promise<void> => request('/api/system/settings/user', {body: settings}),

  getOrgSettings: async (orgId: string): Promise<AppSettings> =>
    requestValidated(`/api/system/settings/org/${orgId}`, await getAppSettingsSchema(), {method: 'GET'}),

  updateOrgSettings: (orgId: string, settings: AppSettings): Promise<void> =>
    request(`/api/system/settings/org/${orgId}`, {body: settings}),
}
