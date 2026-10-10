# 19. Organisations mint their own CI credentials; the deployment secret is deprecated

Date: 2026-10-10. Status: accepted.

## Context

`POST /api/integrations/ci` lets a CI runner (`bakicli report`) have the server analyse a stored flow and record the
run on the dashboard. It authenticated one deployment-wide HMAC key, `PAD_CI_WEBHOOK_SECRET`, and then loaded the
flow by id on the pool, outside row-level security. Row-level security cannot express an organisation's scope (every
policy keys on the calling user), so whoever held the secret could analyse any tenant's flow and record runs on its
dashboard (R8). Rotating the secret broke every runner at once, and nothing could be revoked short of disabling the
endpoint.

The route had smaller faults too:

- It was mounted on the desktop.
- In local mode it analysed whatever document the process had open, whatever `flowId` said.
- It rode the `general` rate-limit bucket although each call is a full analysis.
- It rewrote the dashboard on every call, cache hits included.
- It wrote no audit row.
- It read `PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP` with `os.Getenv` from the handler.

bakicli ships as release binaries and `action.yml` pins a version, so clients that send only `X-Baki-Timestamp` and
`X-Baki-Signature` stay in use for a while.

## Decision

1. **Organisation CI credentials.** An org admin mints credentials in table `org_ci_credentials` (migration 0038,
   `expand`). Each row has the key id, the organisation, a name, the secret, a hint (the last four characters), who
   minted it, when, an optional expiry (1 to 365 days) and the last use. The table has the org-member row-level
   policy of `org_channels`, and the foreign key cascades on organisation delete. An organisation holds at most 10
   unexpired credentials.
2. **Format.** A credential is `baki_ci_<keyId>_<secret>`: a 16-character base32 key id (public, the primary key)
   and 32 random bytes in base64url. The HMAC key is the whole string, so the signature format is unchanged
   (`"sha256=" + hex(HMAC-SHA256(key, ts + "." + body))`, `internal/platform/hmacsig`). An old bakicli given the
   credential as its secret signs correctly.
3. **Sealed, shown once.** The secret must be recoverable to verify, so it cannot be hashed. It is sealed with the
   field encryption of ADR 0005 (`enc:v1:`, associated data table/column/id). The store refuses to write one without
   the key (503 `CI_CREDENTIALS_UNAVAILABLE`), so no plaintext row exists and no backfill is needed. Only the create
   answer carries the credential. An `Idempotency-Key` retry replays it from the sealed response record (ADR 0007),
   as it does for API tokens.
4. **Identification, in order.**
   - The credential named by `X-Baki-Key-Id`. New bakicli derives the header from the credential.
   - The deployment secret, if configured.
   - For a call without a key id (an old bakicli), the unexpired credentials of the organisation owning the flow
     the bounded body names, at most 10.

   Every failure before authentication is the same 401: an unknown key, an unknown or personal flow, an organisation
   without credentials, an expired or revoked credential, a bad signature or a stale timestamp. The only exception is
   a call without `X-Baki-Timestamp`, whose 401 says so. Credentials never accept the untimed signature.
   `pad_ci_webhook_auth_total{method="key_id|global|flow_org"}` counts which way each call came in. The flow
   fallback goes away once old clients are gone.
5. **Scoped resolution.** A credential reaches its organisation's flows only. `LoadFlowHeaderInOrg` puts the
   organisation in the SQL predicate (ADR 0013: routes outside the request transaction rest on explicit
   predicates), and the storage contract tests it. A flow outside the organisation answers the same 404 as an
   unknown one, after authentication. The deployment secret keeps the unscoped lookup by id. In local mode the flow
   is now read from the store by id, no longer taken from the open document.
6. **An integration actor.** The run carries `auth.ActorIntegration` (organisation, credential id as token id, no
   role, no scope). Authorization refuses it everything. Audit records it, with one `ci.webhook_run` row per call
   (method, format, gate, verdict).
7. **The route.**
   - `route.Needs(ServerStore)`: 503 on the desktop.
   - The `analysis` rate-limit bucket (`PAD_RATE_LIMIT_EXPENSIVE_*`, per client address). Its 429 carries
     `Retry-After`, and `bakicli report` resends a rate-limited call up to four times within its `-timeout`.
   - `AnalyzeFlowFresh`, with the dashboard recorded through the bounded detached recorder only for a fresh report.
   - An unknown `format` is a 400. bakicli gates every value but `sarif`, so no release relies on the old fallback.
   - The last use is written off the response path, at most once per five minutes.
8. **Admin API.**
   - `GET /api/orgs/{id}/ci-credentials`
   - `POST /api/orgs/{id}/ci-credentials`, `{name, expiresInDays?}`, 201 with `credential`
   - `DELETE /api/orgs/{id}/ci-credentials/{credId}`, 204

   All three are org-admin only and `Needs(ServerStore, Accounts)`. The writes refuse API tokens (`DenyTokens`, and
   the legacy scope table agrees). The create is not `Idempotent`: a replay would hand the one-time credential out
   again before the admin check runs (to an admin since removed); a retried mint mints another one. Audit actions:
   `org.ci_credential_create` and
   `org.ci_credential_revoke`. To rotate, mint a new credential, switch the runner, then revoke the old one.
9. **Lifecycle.** The data-lifecycle registry claims the table:
   - No data-subject export: it is the organisation's secret.
   - Erasure clears `created_by`. The list shows a minter whose account is gone as gone (`createdBy` and
     `createdByName` empty) even when the row kept the id.
   - Organisation delete cascades.
   - The retention sweep removes credentials 30 days after they expire. It runs on the server store with accounts
     (`cloud`, and `local+db` with `PAD_AUTH_ENABLED`; before this release it ran in cloud mode only).
10. **The deployment secret, per profile.**

    | Profile | This release |
    |---|---|
    | `desktop` | The route answers 503 (`Needs`). |
    | `local+db` without accounts | The secret is the only key and is not deprecated. Without it the route answers 503. |
    | With accounts (`cloud`, `local+db` + auth) | Organisation credentials are primary. The secret still works, with a boot WARN, `Deprecation: true` and a `Link` to this record on every answer, and `method="global"` counted. |

    `PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP` moves into `internal/config`, still exactly `"1"`. It applies to the
    deployment secret only and is deprecated.
11. **A delivery id against replays.** A signed call used to be good for its whole five-minute window, and each
    replay ran a full analysis and recorded a run.
    - The body carries `deliveryId`, so the signature covers it; a header beside the signature would not be, and an
      attacker would change it. A release before this one decodes the body leniently and ignores the field.
    - `bakicli report` sends 128 random bits (32 hex characters), new per invocation. It keeps the id across its
      retries of a 429 from the rate limiter (code `RATE_LIMITED`, or a proxy's 429 that is no API error), which
      answers before the handler runs, so the retry is the same call that never ran. Any other 429 would come from
      the handler, which may have recorded the id: that retry gets a new one.
    - The server validates the id (16 to 128 characters of `A-Z a-z 0-9 - _`, else 400), then inserts
      `(caller, delivery_id)` into `ci_webhook_deliveries` (migration 0039, `expand`) with `ON CONFLICT DO NOTHING`:
      after authentication, so an unauthenticated caller writes nothing, and before the flow is resolved or
      analysed. A pair already present is `409 CI_REPLAYED` and nothing runs; of concurrent calls with one id
      exactly one wins. `caller` is the key that authenticated the call, never a person: `credential:<key id>` (by
      key id and through the old-client fallback alike), `secret` (the deployment secret) or `token:<API token id>`.
      Another key may use the same id.
    - A call without an id is still accepted. `pad_ci_webhook_deliveries_total{delivery="new|replayed|missing"}`
      counts the calls, so operators see when old clients are gone and the id can be required.
    - The table has row-level security that admits the pool only (`NOT app_rls_active()`): the webhook writes it
      outside any request transaction, the sweep purges it, and no user's request transaction sees or writes a row.
      A store failure while recording is a 500 and the call does not run.
    - Lifecycle: no export, nothing personal to erase (key ids and random ids), no organisation column. A record is
      kept an hour: a signed call is fresh for five minutes either side of the server's clock, so a replay can come
      up to ten minutes after the first call, and the rest is margin for skew between replicas. Each insert removes
      its own caller's older records in the same statement, so a key's record stays bounded where no retention
      sweep runs (`local+db` without accounts); the sweep removes the rest in batches. Records of a revoked
      credential, a deleted organisation or an erased account go with the next sweep; nothing can authenticate as
      them meanwhile.
12. **Personal CI access: the `ci` token scope.** Flows outside any organisation have no organisation credential.
    - API tokens get a fifth scope, `ci`. It grants the CI webhook only: every declared route needs `read`, `write`,
      `chat` or `admin`, so a `ci`-only token is refused everywhere else (403, as `chat` is outside chat), and the
      legacy scope table maps `/api/integrations/ci` to it.
    - The webhook takes `Authorization: Bearer <API token>` instead of a signature. The token is verified as on
      every other route (hash, expiry, the owner's current row). Only a scoped token carrying `ci` is admitted: an
      unscoped token, which may do everything else its user can, and a token without `ci` get the webhook's one
      401, as do a session, an unknown, revoked or expired token. A bearer header decides alone: signature headers
      beside it are not consulted. The HMAC methods are unchanged.
    - The token reaches the flows its user can read: `AuthzService` with the new action `CIRun` (viewer access,
      scope `ci`), the rule every route applies. Another user's flow is the same static 404 as an unknown one.
    - An organisation's flow also needs membership of the organisation (any role), whatever else grants the flow.
      The webhook runs outside any request transaction, so the analysis resolves the organisation's custom rules
      and records onto its dashboard; interactively a collaborator outside the organisation reads under row-level
      security, does not see those rules, and records nothing (`RuleProfile.Narrowed`). Rather than reproduce that
      narrowed run, `CIRun` refuses it: such a collaborator needs an organisation credential from an admin.
    - The run acts as the token: audit records the user and token id, `ci.webhook_run` with `method: token`, and
      `pad_ci_webhook_auth_total{method="token"}` counts it. Its delivery ids are recorded per token.
    - `bakicli report -token` (or `$PAD_CI_TOKEN`) sends the token. It excludes `-secret`; both environment
      variables without a flag are an error, and an explicit flag wins over the other's variable. bakicli refuses
      to send a token over plain HTTP to anything but a loopback address.
    - A bearer token over HTTPS authenticates without a signature, but it is not bound to the request: whoever
      captures a call can resend it with a new delivery id. The delivery id stops a verbatim replay only. That is
      inherent to bearer tokens; the defence is HTTPS, the token's expiry and revocation, and the scope, which
      limits a leaked token to running CI on its user's flows.

## Consequences

- Credentials can be revoked and rotated per organisation, and a leaked one reaches one organisation.
- A rollback to the release before keeps the table, which that release never reads (an `expand` step). Runners
  signing with an organisation credential then get 401 until the release returns. Runners on the deployment secret
  are unaffected. An account erased during the rollback keeps its id in `created_by` (that release has no erasure
  step for the table); after the roll-forward the list shows that minter as gone, and the id stays in the row.
- With accounts, a call that nothing verifies is now a 401 instead of the 503 "not configured".
- Personal (org-less) flows are reached with an API token with the `ci` scope; they no longer need the deprecated
  deployment secret.
- A captured signed call runs once. A repeat within the record's hour is a 409; after it, the timestamp has long
  expired. The untimed form (`PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP`) has no window, so its record stops a replay for
  that hour only.
- A delivery id is claimed before the analysis: a call that then fails (an unknown flow, a 500) has used its id, and
  a new `bakicli report` invocation sends a new one.
- Existing tokens keep their scopes and none reaches the webhook; a user mints a `ci` token to use it.
- Rollback of decisions 11 and 12: the release before keeps working on schema v39 (an `expand` step; it never
  reads `ci_webhook_deliveries`, and the rows age out once this release returns). It ignores `deliveryId`, so new
  bakicli keeps working against it, without replay protection. It refuses a bearer call (no signature: 401) and
  mints no `ci` token (400 unknown scope); `ci` tokens minted before the rollback stay in `api_tokens` and are
  refused by every route, as the release before checks route scopes and has none for `ci` (its Settings list shows
  their scope as the untranslated key `tokens.scope.ci`). Old bakicli against this release sends no `deliveryId`
  and is accepted. Rolling the schema back (the down step) drops the record: a
  call captured in the last hour could run once more.

Open items:

- Requiring `deliveryId` once `pad_ci_webhook_deliveries_total{delivery="missing"}` stays at zero.
- Removing the flow fallback and the untimed form once `method="flow_org"` and the deprecation warnings stay at zero.
- Ignoring the deployment secret with accounts unless an operator opts in.
- A sender-constrained token (a signature with the API token as its key) if bearer replay becomes a concern.
