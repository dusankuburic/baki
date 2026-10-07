// Wire enums the backend may grow. A response field typed Tolerant<T> holds a
// value this bundle knows (T) or, when a newer server sent one it does not,
// that string as sent, typed UnknownEnumValue. Such a value never fails a
// parse: one record the bundle predates must not blank a whole listing
// (schemas.lazy.ts tolerantEnum). A new backend value therefore ships one
// release after the tolerant parse that accepts it. Narrow with isKnown before
// using a value as a known one; show the rest under a neutral label, and
// disable what would have to send it back — request fields stay closed.
//
// Why the raw string rather than an 'unknown' member: most responses are not
// validated and hand over the server's string as is, so this is the one type
// that is true of both paths; no wire value can collide with a marker; and an
// unknown value can still be shown or logged.
//
// UnknownEnumValue is a phantom: no wire value starts with NUL, so no known
// literal compares equal to it — `status === 'ready'` narrows it away, which a
// branded `string & {…}` would not — and a plain string cannot pass for one.
export type UnknownEnumValue = `\u0000${string}`

/** A value of the wire enum T, or one a newer server added (see above). */
export type Tolerant<T extends string> = T | UnknownEnumValue

/** Whether `value` is one of `values`: the guard that narrows a Tolerant<T> to T. */
export function isKnown<T extends string>(values: readonly T[], value: unknown): value is T {
  return typeof value === 'string' && (values as readonly string[]).includes(value)
}
