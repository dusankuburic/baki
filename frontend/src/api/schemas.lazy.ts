import {z} from 'zod'
import type {AppSettings, AnalysisReport} from '@/types'

// This module — and therefore zod — is ONLY reachable through the dynamic
// import('./schemas.lazy') in ./schemas.ts. Keeping zod's `z` binding inside
// one chunk lets rollup's deep tree-shaking prune unused members (passing `z`
// across a chunk boundary, or as a function parameter, ships the entire
// library ~4× larger).

export function buildSchemas() {
  // ── Auth ─────────────────────────────────────────────────────────────────
  const AuthUserSchema = z.object({
    id: z.string(),
    email: z.string(),
    role: z.enum(['admin', 'member', 'viewer', 'guest']),
    displayName: z.string().optional(),
    avatarUrl: z.string().optional(),
    createdAt: z.string().optional(),
  })

  const LoginResponseSchema = z.object({
    accessToken: z.string().min(1),
    refreshToken: z.string().min(1),
    expiresAt: z.string(),
    user: AuthUserSchema,
  })

  const RefreshResponseSchema = z.object({
    accessToken: z.string().min(1),
    expiresAt: z.string(),
    refreshToken: z.string().optional(),
  })

  // ── Analysis ──────────────────────────────────────────────────────────────
  // AnalysisReport is the core product output; an empty/missing findings array
  // would silently render a "clean" flow when the backend actually errored.
  //
  // Both schemas use .passthrough() so backend fields NOT explicitly listed
  // here (autoFix, fingerprint, confidence, autoFixHint, metadata on Finding;
  // metrics, groups, ruleProfiles on AnalysisReport) survive validation.
  // Without passthrough, zod's default .strip() would silently delete them and
  // break the Apply-fix button, health-score badge, per-rule timing,
  // confidence badges, and triage/suppression keying.

  // Envelope check for `flow:loaded` SSE payloads entering the editor store
  // (useAppEvents). Same philosophy as above: validate the top-level shape so
  // a malformed payload (HTML error page, truncated JSON, a different event's
  // data) is rejected before it can hijack the editor, while passthrough keeps
  // the deep block tree untouched.
  const FlowDocumentEnvelopeSchema = z
    .object({
      id: z.string().min(1),
      name: z.string(),
      subflows: z.array(z.object({id: z.string(), name: z.string()}).passthrough()).min(1),
    })
    .passthrough()

  const FindingSchema = z
    .object({
      id: z.string(),
      ruleId: z.string(),
      severity: z.enum(['error', 'warning', 'info']),
      title: z.string(),
      description: z.string(),
      blockId: z.string().optional(),
      subflowId: z.string().optional(),
      suggestion: z.string().optional(),
      category: z.string().optional(),
    })
    .passthrough()

  const AnalysisReportSchema = z
    .object({
      flowId: z.string(),
      generatedAt: z.string(),
      durationMs: z.number(),
      findings: z.array(FindingSchema),
      // stats REQUIRED (F1.9): the analyzer unconditionally stamps it
      // (core/analyzer/engine.go computeStats), and the TS type requires it —
      // optionality here only hid the drift behind the cast below.
      stats: z
        .object({
          errors: z.number(),
          warnings: z.number(),
          info: z.number(),
          blocksAnalyzed: z.number(),
          rulesRun: z.number(),
        })
        .passthrough(),
    })
    .passthrough() as unknown as z.ZodType<AnalysisReport>

  // ── Settings ──────────────────────────────────────────────────────────────
  // AppSettings is deeply nested; rather than duplicate the entire type tree
  // in zod, validate the top-level shape (version + the known section keys)
  // and let zod passthrough the rest. This still catches the real failure
  // modes: an HTML proxy body, null, or a fundamentally wrong shape.
  // Annotated as ZodType<AppSettings> so requestValidated<AppSettings>
  // accepts it.
  const AppSettingsSchema = z
    .object({
      version: z.number(),
      general: z.object({}).passthrough(),
      appearance: z.object({}).passthrough(),
      layout: z.object({}).passthrough(),
      ai: z.object({}).passthrough(),
      parser: z.object({}).passthrough(),
      analysis: z.object({}).passthrough(),
    })
    .passthrough() as unknown as z.ZodType<AppSettings>

  return {
    AuthUserSchema,
    LoginResponseSchema,
    RefreshResponseSchema,
    FlowDocumentEnvelopeSchema,
    FindingSchema,
    AnalysisReportSchema,
    AppSettingsSchema,
  }
}
