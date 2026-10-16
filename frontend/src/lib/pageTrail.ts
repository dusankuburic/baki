/** The deepest offset the legacy paged lists take without a cursor: the server refuses a larger one. */
export const MAX_LIST_OFFSET = 10_000

/**
 * The next-page cursors one keyset-paged list answered, by the page each leads to, under one set of filters (key): a
 * page reached by its cursor stays reachable past MAX_LIST_OFFSET. end is the first page found empty; cursors says the
 * server has sent a cursor (one that predates them never does).
 */
export interface PageTrail {
  readonly key: string
  readonly byPage: Readonly<Record<number, string>>
  readonly end?: number
  readonly cursors?: boolean
}

export const EMPTY_TRAIL: PageTrail = {key: '', byPage: {}}

/** The cursor leading to page under key, when the page before it answered one. */
export function trailCursor(trail: PageTrail, key: string, page: number): string | undefined {
  return trail.key === key ? trail.byPage[page] : undefined
}

/**
 * The trail once page answered under key, next being the cursor of the page after it (none: there is none, or none
 * of use). sentCursor: the answer carried a cursor, of use or not.
 */
export function recordPage(
  trail: PageTrail,
  key: string,
  page: number,
  next: string | undefined,
  sentCursor = Boolean(next),
): PageTrail {
  const same = trail.key === key
  const byPage = {...(same ? trail.byPage : {})}
  if (next) byPage[page + 1] = next
  else delete byPage[page + 1]
  // A cursor at or past the recorded end: the list grew.
  const end = same && trail.end !== undefined && !(next && page + 1 >= trail.end) ? trail.end : undefined
  return {key, byPage, end, cursors: trail.cursors === true || sentCursor}
}

/** The trail once page answered empty under key. */
export function recordEnd(trail: PageTrail, key: string, page: number): PageTrail {
  const same = trail.key === key
  const end = same && trail.end !== undefined ? Math.min(trail.end, page) : page
  return {key, byPage: same ? trail.byPage : {}, end, cursors: trail.cursors}
}

/** Whether page is known to be empty under key. */
export function pastEnd(trail: PageTrail, key: string, page: number): boolean {
  return trail.key === key && trail.end !== undefined && page >= trail.end
}
