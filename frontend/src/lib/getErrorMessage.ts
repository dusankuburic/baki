import {ApiError} from '@/api/client'

export function getErrorMessage(err: unknown, fallback?: string): string {
  if (err instanceof Error) return err.message
  if (fallback !== undefined) return fallback
  return String(err)
}

export function getErrorCode(err: unknown): string | null {
  return err instanceof ApiError ? err.code : null
}
