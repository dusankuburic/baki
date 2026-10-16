import {request, requestValidated} from './client'
import {getAppSettingsSchema} from './schemas'
import type {AppSettings} from '@/types'

export const settingsApi = {
  getSettings: async (): Promise<AppSettings> =>
    requestValidated('/api/system/settings', await getAppSettingsSchema(), {method: 'GET'}),

  updateSettings: (settings: AppSettings): Promise<void> => request('/api/system/settings', {body: settings}),

  // noDedup reads past the client's GET cache, for a read-modify-write of the document.
  getOrgSettings: async (orgId: string, opts?: {noDedup?: boolean}): Promise<AppSettings> =>
    requestValidated(`/api/system/settings/org/${orgId}`, await getAppSettingsSchema(), {
      method: 'GET',
      ...(opts?.noDedup ? {noDedup: true} : {}),
    }),

  updateOrgSettings: (orgId: string, settings: AppSettings): Promise<void> =>
    request(`/api/system/settings/org/${orgId}`, {body: settings}),
}
