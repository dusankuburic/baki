// Immutable Set/Map helpers that produce new instances for React/zustand
// change-detection, eliminating the copy-mutate-replace boilerplate.

export function toggleSetMember<T>(set: Set<T>, value: T): Set<T> {
  const next = new Set(set)
  if (next.has(value)) next.delete(value)
  else next.add(value)
  return next
}
