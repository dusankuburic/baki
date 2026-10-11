import {ApiError} from '@/api/client'

export interface ApiFieldError {
  field: string
  reason?: string
}

export function apiErrorField(err: unknown): ApiFieldError | null {
  if (!(err instanceof ApiError)) return null
  const field = err.details?.field
  if (typeof field !== 'string' || !field) return null
  const reason = err.details?.reason
  return {field, reason: typeof reason === 'string' ? reason : undefined}
}
