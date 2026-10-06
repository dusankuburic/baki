import {request, requestValidated} from './client'
import {getAppInfoSchema} from './schemas'
import type {AppInfo as AppInfoType} from '@/types'

export interface FrontendError {
  message: string
  stack: string
  componentStack: string
  url: string
}

export const systemApi = {
  logError: (err: FrontendError): Promise<void> => request('/api/system/log-error', {body: err}),

  appInfo: async (): Promise<AppInfoType> =>
    requestValidated('/api/system/info', await getAppInfoSchema(), {method: 'GET'}),

  // Public (pre-auth) deployment flags; the login page reads them before any
  // session exists.
  features: (): Promise<SystemFeatures> => request('/api/system/features', {method: 'GET'}),
}

export interface SystemFeatures {
  /** Self-registration is closed; the server rejects /api/auth/register. */
  disableSignUp: boolean
}
