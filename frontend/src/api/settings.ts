import {request, requestValidated} from './client'
import {AppSettingsSchema} from './schemas'
import type {AppSettings} from '@/types'

export const settingsApi = {
  getSettings: (): Promise<AppSettings> =>
    requestValidated('/api/system/settings', AppSettingsSchema, {method: 'GET'}),

  updateSettings: (settings: AppSettings): Promise<void> => request('/api/system/settings', {body: settings}),

  getUserSettings: (): Promise<AppSettings> =>
    requestValidated('/api/system/settings/user', AppSettingsSchema, {method: 'GET'}),

  updateUserSettings: (settings: AppSettings): Promise<void> =>
    request('/api/system/settings/user', {body: settings}),

  getOrgSettings: (orgId: string): Promise<AppSettings> =>
    requestValidated(`/api/system/settings/org/${orgId}`, AppSettingsSchema, {method: 'GET'}),

  updateOrgSettings: (orgId: string, settings: AppSettings): Promise<void> =>
    request(`/api/system/settings/org/${orgId}`, {body: settings}),
}
