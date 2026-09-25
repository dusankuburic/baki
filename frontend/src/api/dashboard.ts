import {request} from './client'
import type {DashboardHomeData, DashboardAnalyticsData} from '@/types'

export const dashboardApi = {
  // The backend assembles this per-section and never hard-fails: sections with
  // no data come back with availability flags false rather than as an error.
  getHome: (): Promise<DashboardHomeData> => request('/api/dashboard/home', {method: 'GET'}),
  // The filterable analytics page. `query` is the canonical string from
  // analyticsStore.toQuery, so the GET micro-cache keys on the exact filters;
  // the signal lets a superseded filter change abort the request it replaced.
  getAnalytics: (query: string, signal?: AbortSignal): Promise<DashboardAnalyticsData> =>
    request(`/api/dashboard/analytics?${query}`, {method: 'GET', signal}),
}
