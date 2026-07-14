import {request} from './client'
import type {DashboardHomeData} from '@/types'

export const dashboardApi = {
  // The backend assembles this per-section and never hard-fails: sections with
  // no data come back with availability flags false rather than as an error.
  getHome: (): Promise<DashboardHomeData> => request('/api/dashboard/home', undefined, 'GET'),
}
