import {request} from './client'
import type {AppInfo as AppInfoType} from '@/types'

export interface FrontendError {
  message: string
  stack: string
  componentStack: string
  url: string
}

export const systemApi = {
  logError: (err: FrontendError): Promise<void> => request('/api/system/log-error', err),

  appInfo: (): Promise<AppInfoType> => request('/api/system/info', undefined, 'GET'),
}
