/**
 * A stored card as a finished message shows it: a card still pending when the answer was interrupted was never
 * decided (cancelled), and one stored mid-apply has no known outcome — never the live "Applying…" (R-approvals-5).
 */
export function settledCard<T extends {status: string}>(snap: T, interrupted: boolean): T {
  if (interrupted && snap.status === 'pending') return {...snap, status: 'cancelled'}
  if (snap.status === 'applying') return {...snap, status: 'unknown'}
  return snap
}
