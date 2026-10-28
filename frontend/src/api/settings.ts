import {request, requestValidated} from './client'
import {getAppSettingsSchema} from './schemas'
import type {AppSettings} from '@/types'

/** One section of an organisation's settings and its revision, the token a section write's ifRevision carries. */
export interface SettingsSection<T = Record<string, unknown>> {
  section: string
  values: T
  revision: string
}

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

  // One section and its revision, read past the GET cache: the revision is a write's precondition.
  getOrgSettingsSection: <T = Record<string, unknown>>(orgId: string, section: string): Promise<SettingsSection<T>> =>
    request(`/api/system/settings/org/${orgId}/sections/${section}`, {method: 'GET', noDedup: true}),

  // Sets the named fields of one section; the server merges them into the section as stored, so a concurrent
  // write to another field is kept. With ifRevision a write the section has moved past is refused with 409
  // SETTINGS_STALE: reload before retrying. A server from before the route answers 404.
  patchOrgSettingsSection: <T = Record<string, unknown>>(
    orgId: string,
    section: string,
    values: Partial<T>,
    ifRevision?: string,
  ): Promise<SettingsSection<T>> =>
    request(`/api/system/settings/org/${orgId}/sections/${section}`, {
      method: 'PATCH',
      body: {values, ...(ifRevision ? {ifRevision} : {})},
    }),
}
