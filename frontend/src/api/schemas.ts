import type {buildSchemas} from './schemas.lazy'

// Schemas for the highest-risk API responses — those whose shape drives auth
// state or core analysis output. A backend shape change or a misconfigured
// proxy returning non-JSON surfaces as a clear ResponseValidationError here,
// not an opaque downstream TypeError.
//
// zod (~20 KB gzip tree-shaken) is deferred out of the entry chunk: these
// factories dynamic-import the schema builder (which statically imports zod
// and therefore lives with it in an async chunk) on the first validated
// response, and memoize the result. Consumers await getXSchema() before
// calling requestValidated — after the first call the memoized schema
// resolves in a microtask.
//
// The builders live in schemas.lazy.ts (NOT here) so zod's `z` binding never
// crosses a chunk boundary — cross-chunk namespace objects defeat deep
// tree-shaking and would ship the whole library.

export type Schemas = ReturnType<typeof buildSchemas>

let built: Promise<Schemas> | null = null

function loadSchemas(): Promise<Schemas> {
  built ??= import('./schemas.lazy')
    .then(m => m.buildSchemas())
    .catch(err => {
      // Do NOT memoize failure: a transient chunk-load error (classic case: a
      // deploy invalidates old hashed chunk names while a tab stays open)
      // must not permanently disable every schema factory for the session —
      // the next call retries the import instead of rejecting forever.
      built = null
      throw err
    })
  return built
}

export const getAuthUserSchema = (): Promise<Schemas['AuthUserSchema']> => loadSchemas().then(s => s.AuthUserSchema)

export const getLoginResponseSchema = (): Promise<Schemas['LoginResponseSchema']> =>
  loadSchemas().then(s => s.LoginResponseSchema)

export const getRefreshResponseSchema = (): Promise<Schemas['RefreshResponseSchema']> =>
  loadSchemas().then(s => s.RefreshResponseSchema)

export const getFlowDocumentEnvelopeSchema = (): Promise<Schemas['FlowDocumentEnvelopeSchema']> =>
  loadSchemas().then(s => s.FlowDocumentEnvelopeSchema)

export const getFindingSchema = (): Promise<Schemas['FindingSchema']> => loadSchemas().then(s => s.FindingSchema)

export const getAnalysisReportSchema = (): Promise<Schemas['AnalysisReportSchema']> =>
  loadSchemas().then(s => s.AnalysisReportSchema)

export const getAppSettingsSchema = (): Promise<Schemas['AppSettingsSchema']> =>
  loadSchemas().then(s => s.AppSettingsSchema)
