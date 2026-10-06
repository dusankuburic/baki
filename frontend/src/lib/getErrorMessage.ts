import {ApiError} from '@/api/client'

// getErrorMessage extracts a user-displayable message from a caught value —
// the shared home for the `err instanceof Error ? err.message : <fallback>`
// idiom that used to be pasted at every call site. Behavior-preserving with
// that idiom: an Error (ApiError included) yields its message; anything else
// yields the site's fallback when one is given, else String(value).
//
// ApiError.message is already the backend envelope copy, so it needs no
// special casing here. Branching on a machine-readable code for different UX
// (busy toast, budget notice, …) is the caller's job — use getErrorCode or
// see lib/chatErrors.ts for the chat precedent.
export function getErrorMessage(err: unknown, fallback?: string): string {
  if (err instanceof Error) return err.message
  if (fallback !== undefined) return fallback
  return String(err)
}

// getErrorCode returns the backend envelope's machine-readable code when the
// value is an ApiError that carries one, else null — the read-side half of the
// `e instanceof ApiError && e.code === '…'` pattern.
export function getErrorCode(err: unknown): string | null {
  return err instanceof ApiError ? err.code : null
}
