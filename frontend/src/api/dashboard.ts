import {requestValidated} from './client'
import {getDashboardAnalyticsSchema, getDashboardHomeSchema} from './schemas'
import type {DashboardHomeData, DashboardAnalyticsData} from '@/types'

export const dashboardApi = {
  getHome: async (): Promise<DashboardHomeData> =>
    requestValidated('/api/dashboard/home', await getDashboardHomeSchema(), {method: 'GET'}),
  getAnalytics: async (query: string, signal?: AbortSignal): Promise<DashboardAnalyticsData> =>
    requestValidated(`/api/dashboard/analytics?${query}`, await getDashboardAnalyticsSchema(), {
      method: 'GET',
      signal,
    }),
}
