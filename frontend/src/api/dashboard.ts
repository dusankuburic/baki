import {requestValidated} from './client'
import {getDashboardAnalyticsSchema, getDashboardHomeSchema} from './schemas'
import type {DashboardHomeData, DashboardAnalyticsData} from '@/types'

export const dashboardApi = {
  // The backend assembles this per-section and never hard-fails: sections with
  // no data come back with availability flags false rather than as an error.
  // The schema validates the load-bearing scalars per section and reads nil
  // (null-marshalled) collections as empty — see schemas.lazy.ts.
  getHome: async (): Promise<DashboardHomeData> =>
    requestValidated('/api/dashboard/home', await getDashboardHomeSchema(), {method: 'GET'}),
  // The filterable analytics page. `query` is the canonical string from
  // analyticsStore.toQuery, so the GET micro-cache keys on the exact filters;
  // the signal lets a superseded filter change abort the request it replaced.
  getAnalytics: async (query: string, signal?: AbortSignal): Promise<DashboardAnalyticsData> =>
    requestValidated(`/api/dashboard/analytics?${query}`, await getDashboardAnalyticsSchema(), {
      method: 'GET',
      signal,
    }),
}
