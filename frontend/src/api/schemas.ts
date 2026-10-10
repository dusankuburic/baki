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

export const getRegisterResponseSchema = (): Promise<Schemas['RegisterResponseSchema']> =>
  loadSchemas().then(s => s.RegisterResponseSchema)

export const getRefreshResponseSchema = (): Promise<Schemas['RefreshResponseSchema']> =>
  loadSchemas().then(s => s.RefreshResponseSchema)

export const getFlowDocumentEnvelopeSchema = (): Promise<Schemas['FlowDocumentEnvelopeSchema']> =>
  loadSchemas().then(s => s.FlowDocumentEnvelopeSchema)

export const getFindingSchema = (): Promise<Schemas['FindingSchema']> => loadSchemas().then(s => s.FindingSchema)

export const getAnalysisReportSchema = (): Promise<Schemas['AnalysisReportSchema']> =>
  loadSchemas().then(s => s.AnalysisReportSchema)

export const getAppSettingsSchema = (): Promise<Schemas['AppSettingsSchema']> =>
  loadSchemas().then(s => s.AppSettingsSchema)

// ── Flow reads ──────────────────────────────────────────────────────────────
export const getLoadedFlowDocumentSchema = (): Promise<Schemas['LoadedFlowDocumentSchema']> =>
  loadSchemas().then(s => s.LoadedFlowDocumentSchema)

export const getRecentFilesSchema = (): Promise<Schemas['RecentFilesSchema']> =>
  loadSchemas().then(s => s.RecentFilesSchema)

export const getSearchResultsSchema = (): Promise<Schemas['SearchResultsSchema']> =>
  loadSchemas().then(s => s.SearchResultsSchema)

export const getSourceFilesSchema = (): Promise<Schemas['SourceFilesSchema']> =>
  loadSchemas().then(s => s.SourceFilesSchema)

export const getSourceMetaSchema = (): Promise<Schemas['SourceMetaSchema']> =>
  loadSchemas().then(s => s.SourceMetaSchema)

export const getSnapshotsListSchema = (): Promise<Schemas['SnapshotsListSchema']> =>
  loadSchemas().then(s => s.SnapshotsListSchema)

// ── Library ─────────────────────────────────────────────────────────────────
export const getLibraryFlowsPageSchema = (): Promise<Schemas['LibraryFlowsPageSchema']> =>
  loadSchemas().then(s => s.LibraryFlowsPageSchema)

export const getLibraryFlowSchema = (): Promise<Schemas['LibraryFlowSchema']> =>
  loadSchemas().then(s => s.LibraryFlowSchema)

export const getLibraryFlowVersionSchema = (): Promise<Schemas['LibraryFlowVersionsSchema']> =>
  loadSchemas().then(s => s.LibraryFlowVersionsSchema)

// ── Governance ──────────────────────────────────────────────────────────────
export const getGovernanceAlertsSchema = (): Promise<Schemas['GovernanceAlertsSchema']> =>
  loadSchemas().then(s => s.GovernanceAlertsSchema)

export const getUnreadCountSchema = (): Promise<Schemas['UnreadCountSchema']> =>
  loadSchemas().then(s => s.UnreadCountSchema)

export const getOrgCICredentialsSchema = (): Promise<Schemas['OrgCICredentialsSchema']> =>
  loadSchemas().then(s => s.OrgCICredentialsSchema)

export const getCreatedOrgCICredentialSchema = (): Promise<Schemas['CreatedOrgCICredentialSchema']> =>
  loadSchemas().then(s => s.CreatedOrgCICredentialSchema)

export const getOrgChannelsSchema = (): Promise<Schemas['OrgChannelsSchema']> =>
  loadSchemas().then(s => s.OrgChannelsSchema)

export const getChannelOptionsSchema = (): Promise<Schemas['ChannelOptionsSchema']> =>
  loadSchemas().then(s => s.ChannelOptionsSchema)

// ── Providers ───────────────────────────────────────────────────────────────
export const getProviderListSchema = (): Promise<Schemas['ProviderListSchema']> =>
  loadSchemas().then(s => s.ProviderListSchema)

export const getProviderCatalogSchema = (): Promise<Schemas['ProviderCatalogSchema']> =>
  loadSchemas().then(s => s.ProviderCatalogSchema)

export const getGitHubUserSchema = (): Promise<Schemas['GitHubUserOrNullSchema']> =>
  loadSchemas().then(s => s.GitHubUserOrNullSchema)

export const getDeviceAuthUserSchema = (): Promise<Schemas['DeviceAuthUserOrNullSchema']> =>
  loadSchemas().then(s => s.DeviceAuthUserOrNullSchema)

export const getAzureConnectionsListingSchema = (): Promise<Schemas['AzureConnectionsListingSchema']> =>
  loadSchemas().then(s => s.AzureConnectionsListingSchema)

export const getAzureConnectionResponseSchema = (): Promise<Schemas['AzureConnectionResponseSchema']> =>
  loadSchemas().then(s => s.AzureConnectionResponseSchema)

export const getAzureModelsListingSchema = (): Promise<Schemas['AzureModelsListingSchema']> =>
  loadSchemas().then(s => s.AzureModelsListingSchema)

export const getAzureDiscoverySchema = (): Promise<Schemas['AzureDiscoverySchema']> =>
  loadSchemas().then(s => s.AzureDiscoverySchema)

// ── Chat ────────────────────────────────────────────────────────────────────
export const getSuggestedPromptsSchema = (): Promise<Schemas['SuggestedPromptsSchema']> =>
  loadSchemas().then(s => s.SuggestedPromptsSchema)

// ── Admin ───────────────────────────────────────────────────────────────────
export const getAdminUsersPageSchema = (): Promise<Schemas['AdminUsersPageSchema']> =>
  loadSchemas().then(s => s.AdminUsersPageSchema)

export const getUserStatsSchema = (): Promise<Schemas['UserStatsSchema']> => loadSchemas().then(s => s.UserStatsSchema)

export const getAuditEventsSchema = (): Promise<Schemas['AuditEventsSchema']> =>
  loadSchemas().then(s => s.AuditEventsSchema)

export const getAuditActionsSchema = (): Promise<Schemas['AuditActionsSchema']> =>
  loadSchemas().then(s => s.AuditActionsSchema)

// ── Dashboard ───────────────────────────────────────────────────────────────
export const getDashboardHomeSchema = (): Promise<Schemas['DashboardHomeSchema']> =>
  loadSchemas().then(s => s.DashboardHomeSchema)

export const getDashboardAnalyticsSchema = (): Promise<Schemas['DashboardAnalyticsSchema']> =>
  loadSchemas().then(s => s.DashboardAnalyticsSchema)

// ── System ──────────────────────────────────────────────────────────────────
export const getAppInfoSchema = (): Promise<Schemas['AppInfoSchema']> => loadSchemas().then(s => s.AppInfoSchema)
