# Backend refactoring proposal: making Baki extensible (revision 3, two review passes applied)

## Status (2026-10-10)

**Fix first:** R1–R8 and S1–S3 are done (R7 and R8 with Phase 4c).

**Phase 4b: done** (2026-10-10, uncommitted). Migration 0042, `expand`.

*Delivered:*
- **Provider specs** (ADR 0025, `docs/architecture/ai-providers.md`): `ai.ProviderSpec` (id, name, auth type,
  caps, modes as capability profiles, egress policy, credential schema, keystore keys, catalog, default model,
  pricing mode, wire family, resolver, constructor, listing hook, colour, icon, order), one `spec_<id>.go` per
  provider, registered at init and pinned by `testdata/providers.golden`; every provider list is derived from
  `ai.Specs()` (the available and embedding-capable sets, the fallback order, the constructors, the keystore key
  names, the alias set, the valid-id check, the listing filter by profile). Copilot and github-models are spec
  fields (keys, resolver, no org sharing); Azure AI Foundry stays a family behind a `Listing` hook, its registry
  untouched; the nine per-provider pricing constants are gone (the catalog row of the default model). A
  go/packages ratchet pins the provider-id literals per file (95 in 21 files before; 22 in 15 now). The wire adds
  `GET /api/providers/catalog` (every registered provider of the running profile, configured or not, with its
  credential schema and endpoints; same policy as `/list`) and optional `ProviderInfo` fields; `authType` is
  unchanged. `PROVIDER_UNKNOWN` (400) on the key routes, the connection test and chat create for an id the registry
  does not know.
- **Tool specs and approvals** (ADR 0026): `ai.ToolSpec` declares `RequiresApproval`, `RequiredScope` and
  `PreviewKind`; `ToolDefinitionsFor(gate)` offers a token's model only the tools its scopes allow, and
  `ExecuteTool` refuses a gated tool before its handler runs (an Azure agent's foreign `function_call` never runs
  one). One approvals component on `ChatService` (request, keepalive, timeout, resolve with ownership and scope
  checks) serves both fix paths; `fix_proposal` carries `tool`, `previewKind` (`fix-patch`), `requiredScope` and
  always `items[]`, `fix_decision` gains `cancelled` (emitted at shutdown or a stop before the error event), the
  endpoint and its policy are unchanged; `testdata/tools.golden`; the authz matrix gains a `pat:chat+write` actor.
- **Budget and metering** (ADR 0028): `ai.BudgetChecker` is consulted first in the audited wrapper's `Chat`,
  `Stream` and `Embed`, above the breaker and retries, so every billed call is covered (connection tests and the
  knowledge query embed were not); the chat turn keeps its two checks and seeds a request memo and the stream's own
  spend into the context, so the hook costs no extra usage read; `Embed` attributes usage to the org the run was
  gated on; the in-flight ledger sits behind a `spendLedger` seam whose error fails closed (Phase 5b's Redis
  ledger). `ai.ProviderPolicy` is consulted by every factory builder before any key is read: the deployment's
  `Enabled` rule and a new org-settings `ai.allowedProviders` (empty = all); refusals are `PROVIDER_NOT_ALLOWED`
  on streams, the test result and the knowledge routes. `pad_ai_budget_refusals_total{provider,call,reason}`.
- **RAG identity** (ADR 0027, migration 0042): `knowledge_chunks.embedding_identity` (per chunk, the frozen
  `label|baseURL|model|dims` string) and `knowledge_documents.chunker_version`; search filters by the current
  identity (legacy NULL rows match leniently and are stamped on the first proven same-model upload); a
  column compare replaces the billed three-sample guard for stamped corpora; the empty-result classification is
  precise (`pad_rag_stranded_search_total` vs the width metric, which over-counted); an unforced re-index walks
  only non-current chunks; `embeddingStranded` per document on the list and in the panel; a declared on-demand
  `knowledge_reembed` job (`{"background": true}` on the reindex route, 202, org credential required, never
  automatic spend); the per-org advisory lock in the page transactions (two replicas can no longer interleave an
  upload and a re-index); a contract scenario on the fake; a chunker geometry golden.
- **Frontend:** `ProviderID` is a string with a `KnownProviderID` set (an unknown provider is listed as sent,
  not dropped; a saved unknown `activeProvider` still selects); the settings panel renders from the catalogue
  (a `CredentialForm` by `credential.kind`, bespoke panels only for Azure, local and Copilot, a
  `GenericProviderCard` for an unknown provider, kind or field type, the old panel when the catalogue answers
  404); the embedding list and the picker glyphs come from the catalogue; `ApprovalCard` renders by `previewKind`
  with a generic fallback, parses the generic `approval_request`/`approval_decision` events of a later server, and
  the persisted strip does the same; the device-login button draws the provider's icon.
- **Safety nets:** providers, tools, chunker goldens; the literal ratchet; `TestProviderSpecs_Complete`,
  `TestToolSpecs_Complete`, `TestEmbeddingIdentity_FormatFrozen`, `TestFactory_PolicyRefusesEveryBuilderBeforeTheKeyRead`,
  `TestApprovals_RoundTripThroughRouter`, the knowledge contract scenario; frontend tolerance tests for the
  catalogue, the card and the codes; an e2e spec per feature.

*Bugs fixed on the way:*
- The default Gemini model id `gemini-2-5-pro` does not exist (catalog `gemini-2.5-pro`); three names for the
  local provider; an unknown provider id in a chat turn was logged as an operator incident ("provider setup
  failed") and told the user to retry.
- A chat-only token's model was offered `apply_fix`, called it, burned an iteration and got a refusal string; a
  pending approval at shutdown was left hanging (nothing written, no event) until the slot was torn down.
- Connection tests ran billed calls that no budget gated (the org variant billed the org); a member's key
  embedding into an org corpus was never counted against the org; a disabled provider was refused only in chat,
  after the provider was built, without a code; the usage-runner refusal at the 1024 cap counted as "shutdown".
- A same-width embedding model switch was invisible until the next write, every search returned nothing and the
  width metric counted it; every upload into a non-empty corpus bought a three-sample embed; a forced re-index
  re-bought chunks already converted; two replicas could interleave an upload and a re-index (process-local lock).
- An unknown saved `activeProvider` broke the chat with a codeless error; the chat-stream e2e spec was red on the
  base tree (its provider stub lacked fields the schema requires).

*Behaviour changes users will notice:*
- **Providers:** `/api/keys/has` answers 400 `PROVIDER_UNKNOWN` for an unknown id (was 200 `{has:false}`); a
  provider disabled in the deployment settings is refused everywhere with `PROVIDER_NOT_ALLOWED`; `ai.allowedProviders`
  in the org settings restricts an org's flows and corpus (no UI yet); the settings panel lists every provider of
  the profile from the catalogue in the server's order.
- **Budget:** a connection test on a spent daily budget answers `{ok:false, code:"AI_BUDGET_EXCEEDED"}` without a
  call; an org corpus embedded through a member's key now shows in the org's daily usage.
- **Approvals:** a stop or deploy during a pending card shows "Cancelled — nothing was changed"; the card for a
  single fix also carries `items[]`.
- **Knowledge:** the list and the panel say how many chunks were indexed with another embedding model; a
  background re-index answers 202 and runs as a job (`pad_job_*`, `job_runs`); the search over a stranded corpus
  counts as stranded, not as a width mismatch.
- **Operations:** schema v42; new metrics `pad_ai_budget_refusals_total`, `pad_chat_tool_refused_total`,
  `pad_rag_stranded_search_total`; `pad_usage_dropped_total{reason="runner"}` (was `shutdown` for the 1024 cap: an
  alert on `shutdown` goes quiet); new code `PROVIDER_POLICY_UNAVAILABLE`; the org settings route refuses an
  unknown provider id in `ai.allowedProviders`; new goldens
  (`-update-provider-specs`, `-update-tools`).

*Found in review and fixed:* the org allowlist was wiped by this release's own partial org-settings writes (a
rule toggle posts only the rules section, and an absent list meant "all providers"): a write that does not name
`ai.allowedProviders` now keeps it, `[]` clears it, and an unknown id is refused with `PROVIDER_UNKNOWN`; the
background re-embed job checked the org credential only at its start and could finish on the starting admin's
personal key (now every page re-checks and the run fails instead); a failed org-settings read inside the policy
reached admins as `EMBEDDING_NOT_CONFIGURED` with internal text (now 503 `PROVIDER_POLICY_UNAVAILABLE`, fixed copy);
a batch card matched outcomes by rule id, so two fixes of one rule on different blocks shared a row (items carry
`id = blockId:ruleId`, the client matches by id, else by position); the settings panel read `/api/keys/has`'s
`{has}` envelope as truthy and showed every api-key provider as configured; Azure probes, the preview context and
the knowledge list's embedder resolution bypassed the policy or the budget scope; identity stamping wrote an
RLS-forced table through the pool (now under the caller's scope, pinned across two orgs); the refusal metric
always said `unavailable`; lookups keyed by server strings could hit prototype keys; the catalogue's api-key field
was named `apiKey` while the save body takes `key`; the literal ratchet's provider list was hand-kept (now derived
from `ai.Specs()`); swagger lacked the new codes.

*Plan corrections:* the proposal's Findings row 11 was partly stale (usage writes were already drained by Phase
0b; the frontend `authType` was already tolerant; there was no `/api/config` route and no org allowlist to
enforce); the "41 branch lines in 11 files" were 95 literals in 21 files by the time the phase started; the fix
flow was batch + journal + resume, so the generic approval kept the fix event names and endpoint and added fields
instead of renaming (an old client never misses a card); the catalogue is its own route because the listing hides
unconfigured providers; the phase ran as three waves of parallel agents plus two follow-ups and a review round,
about the estimated four to five weeks of work.

*Open:* the org allowlist has no settings UI; the policy reads the org settings once per provider build (a
declared cache if the listing shows it); `message.approvals[]` is read by the client but the server stores only
fix proposals; `POST /api/providers/{id}/config` for document-kind providers is deferred (bespoke panels); the
`approval_request`/`approval_decision` names and `/api/chat/approval-decision` exist only on the client; the
pending-approval map stays in process memory (Phase 5b); the catalog's select option labels equal their values;
`ModelPicker` and the device-login button keep two glyph maps; a re-pointed Azure deployment behind the same name
is undetectable by identity (force a re-index); the stale `ReservationPricer` has no caller; a re-index run on the previous release after a rollback rewrites
vectors but keeps the stamped identity (migrate down 42 or force a re-index); the background re-embed is one job
per deployment, so one org's run makes another's wait (409) for up to its 2 h budget; generic org
credential routes exist only for Azure (the factory and listing support every `CapOrgShared` provider).

**Phase 5a: done** (2026-10-10, uncommitted). Migration 0041, `expand`.

*Delivered:*
- **`main.go` split** (ADR 0010 updated, `di-wiring.md`): 994 lines became 74 (build stamps, the healthcheck, the
  swag annotations); the fx graph lives in `internal/app`, one file per concern (`app.go`, `config.go`,
  `observability.go`, `storage.go`, `keystore.go`, `auth.go`, `governance.go`, `jobs_*.go`, `server.go`), a pure
  move pinned by an AST comparison. `TestApp_StopOrder` runs the real graph in a child process and pins the stop
  hooks (desktop and cloud profiles); `TestDesktopBoot_Smoke` builds the binary and checks the `CONFIG:` line, the
  session key file, `/healthz` and a clean stop that never touches the OS keychain; `TestMainPackageIsMainGoAlone`
  keeps `go build main.go` working. The archtest ratchets now parse every root file (R12).
- **`lifecycle.Singleton`** (ADR 0023, migration 0041 `job_runs`): every advisory-lock user (scanner, retention,
  audit backfill, field-encryption backfill, secret re-seal, legacy migration, token-blacklist cleanup, HNSW build)
  is a declared job (`lifecycle.DeclareJob`, `testdata/jobs.golden`, keys derived from the name with the legacy keys
  kept, uniqueness over every `*LockKey` constant in the source). Lock errors fail closed (skip, retry sooner);
  a panic ends the run, not the loop (R6); one run per process; no locker (desktop) runs alone. The Postgres lease
  heartbeats on its pinned session every 15 s and cancels the run when the lock is gone (R5); `pg_advisory_unlock`
  answering `false` is a lost lease and discards the session (R4). `job_runs` (no RLS, listed with its reason)
  feeds `pad_job_last_success_timestamp_seconds` and the cross-replica `pad_job_overdue` gauge with a `job overdue`
  WARN (default twice the interval); `pad_job_runs_total{outcome}`, `pad_job_skipped_total{reason}`,
  `pad_job_duration_seconds`, `pad_advisory_lease_lost_total`, `pad_advisory_unlock_not_held_total`. `DrainAll`
  also stops jobs their owners did not; the migration run and admin scan-now are cancelled at shutdown (R16, R17).
- **Config** (R6 completed, R8, R13, R14): `config.Duration` (JSON string or nanoseconds, env text) for the eight
  duration fields, parsed once at load; `config.Load` owns the bootstrap (`PAD_CONFIG` file or env), validates
  ranges (request timeout > 0, intervals and pool durations ≥ 0, breaker and retry > 0, drain delay ≤ 10 s) on both
  paths, resolves the instance id and copies every slice; `TestArch_ConfigNotMutatedOutsideConfig` (go/types)
  forbids assigning, appending to or taking the address of any config field outside `internal/config`; the AI
  factory takes a parsed `ResilienceConfig` instead of `&cfg.Runtime`; the healthcheck reads a config file's port.
  New knobs `PAD_INSTANCE_ID` (else the ACA replica name, the host name, a UUID), `PAD_SHUTDOWN_DRAIN_DELAY`,
  `PAD_MAX_REPLICAS`, `PAD_DB_SERVER_MAX_CONNS`; the effective log-level default is `info` in every mode (R10).
- **Secrets in configuration** (ADR 0005 §"Secrets in configuration"): 18 credential-bearing fields are
  `secret.Value` (database and Redis URLs, the blob connection string, the auth secret, the encryption keys, the
  SSO client secret, the webhook URLs, the signing secrets, the Jira token, the SMTP password, the metrics token):
  `[redacted]` under every format verb, JSON, slog and error wrapping, read only through `Reveal()` at the
  boundary, compared with `Equal`; `TestConfig_FormatAndJSONNeverShowSecrets` proves it; `SecretEphemeral` can no
  longer be set from a config file; a malformed `PAD_REDIS_URL` no longer echoes its password (R9).
- **Observability** (R15): modules register their own metrics (`metrics.MustRegister` at package init, 47 of 56
  collectors moved, the central list pinned at 9 shared ones and only allowed to shrink) and
  `testdata/metrics.golden` lists every family with its labels, help and registering package; the OTel resource
  carries `service.instance.id` and honours `OTEL_RESOURCE_ATTRIBUTES`; every log line carries `instance_id`; the
  logger and telemetry are installed before any module runs (migration lines are JSON now) and telemetry flushes
  last; `errreport` events carry the instance id; `application_name=baki-backend/<instance>` on every Postgres
  session. KQL alerts in Bicep for `job overdue`, `job lease lost`, the two advisory-lock lines and `chat workers
  still running at shutdown`; the readiness alert counts per replica, above what a deploy's drain can produce.
- **Graceful shutdown** (ADR 0024, `internal/platform/drain`): the drain hook runs first (`/readyz` and
  `/api/health` 503 `draining`, new chat streams, event streams and `/ws` refused with 503 `SERVER_RESTARTING`,
  `Retry-After` and `details.retryAfterSeconds`, a pause of `PAD_SHUTDOWN_DRAIN_DELAY`), then `ChatService.Shutdown`
  ends every stream with `SERVER_RESTARTING` and waits up to 2 s for the workers, final snapshots and persists
  (R1, R2); the SSE handler writes what is queued before it closes; a resume of an unknown stream says
  `STREAM_NOT_FOUND`. The client resends the turn once automatically (status line, 2–3 s jittered, the server's
  hint capped at 10 s; not for turns with fix cards, reattached streams or unsynced history) and reports a stream
  whose reconnect resume answers 403/404/410 as interrupted at once instead of "stalled" 90 s later. `StopTimeout`
  28 s with the budget table in the ADR (the scanner's stop capped at 5 s), `terminationGracePeriodSeconds` 35,
  compose `stop_grace_period: 35s` (R7), pinned by `TestApp_StopEndsChatStreamsBeforeSSECloses` and
  `deploy_pool_test.go`.
- **Pool size per replica** (`operations.md`): Bicep derives `PAD_DB_MAX_OPEN_CONNS` from `dbServerMaxConnections`
  minus the reserved connections over `maxReplicas` plus the deploy surge, capped at the app default; the app warns
  at boot when the pools could exceed `max_connections` and when the DSN points at PgBouncer's 6432 (session
  advisory locks); PgBouncer transaction mode is documented as unsupported; the stale
  `idle_in_transaction_session_timeout` comment is corrected (R19).
- **Safety nets:** metrics and jobs goldens; `TestApp_StopOrder`, `TestDesktopBoot_Smoke`,
  `TestApp_LogsAreJSONFromTheFirstLine`, `TestApp_StopEndsChatStreamsBeforeSSECloses`; the config immutability and
  secret-redaction tests; `deploy_pool_test.go` (pool formula, grace periods, alert log strings still in the
  source); `TestMetrics_NoUnboundedLabelNames` over every registered family.

*Bugs fixed on the way:*
- The image build and CI's go-build job compiled `main.go` alone and had been broken since Phase 4c's
  `secret_reseal.go` (both build `.` now).
- Retention and the scanner ran without the lock when the lock query failed (R3); a scanner panic ended the loop
  (R6); a job kept running after its session died (R5); a lost lease leaked the lock silently (R4).
- `PAD_REQUEST_TIMEOUT=0s` timed out every request at once, and a malformed duration in a config file silently
  disabled scanning or retention (R8); the log level was ignored on desktop and local+db (R10); a malformed Redis
  URL printed its password (R9); the healthcheck probed `PAD_PORT` even with a config file (R14).
- A deploy with an open chat lost the terminal event: the client showed "stalled" 90–120 s later (R1); a finished
  answer's persist raced the pool close (R2); compose killed the app after 10 s (R7); a refused `/ws` upgrade burned
  its single-use ticket; migration logs went to the text logger in the cloud.
- The migration runner's lock error answered 409 "already running" (R16); admin scan-now and the migration run were
  not cancelled at shutdown (R17); local+db with accounts had no scan-now (R20).

*Behaviour changes users will notice:*
- **Deploys:** a cut-short answer shows "The server restarted — sending your message again…" and is answered again;
  a message sent while the server drains waits and goes through; `/readyz` is 503 for the drain delay; the stop
  takes `PAD_SHUTDOWN_DRAIN_DELAY` (3 s) longer.
- **Boot refusals:** a malformed duration in a config file, a request timeout ≤ 0, `0s` for the breaker or retry
  durations, negative intervals or pool durations, a drain delay over 10 s, a `[redacted]` marker in any secret.
  `"RetentionPurgeInterval": ""` in a file means the 24 h default now (write `"0s"` for off).
- **Logs:** JSON from the first line in the cloud, `instance_id` on every line; a cloud config file without a log
  level logs at info (was debug); the re-seal and backfill lines are the job's (`job run failed`, `interrupted`).
- **Operations:** schema v41; `pad_background_task_*{field_encryption_backfill|secret_reseal}` become `pad_job_*`;
  `postgres` sessions named `baki-backend/<instance>`; `OTEL_SERVICE_NAME`/`OTEL_RESOURCE_ATTRIBUTES` now override
  the service name; retention skips on a lock error and logs `job overdue` after twice its interval; a partial purge
  is a failed run; the audit backfill retries a lock error after 5 minutes instead of waiting for the next boot.

*Plan corrections:* "modules register their own metrics" was not in the survey's agent plan and was added as its own
item; the Singleton's heartbeat reached the scanner, audit backfill and migration only once the leaser was wired
in wave 2; `fx.Supply(app.Build)` carries the version (one leaf value, no closure in the Invoke list); the phase
ran as three waves of parallel agents plus an integrator, about the estimated two weeks of work.

*Open:* admin scan-now answers `{started: true}` even when the sweep then skips (needs `ScanNow(ctx) error` and a
409/503); the safety-net waits at stop (cancelled jobs ≤ 5 s each, telemetry 5 s, blob cleaner 15 s) have fixed
budgets outside the table, so two of them at once overrun the 28 s (context-aware stops are the structural fix);
`maxShutdownDrainDelay` 10 s exceeds what the budget absorbs (about 5 s); the stall probe outside a reconnect still
takes about 90 s to report a gone stream; `ApiError` carries no response headers (the client reads
`details.retryAfterSeconds`); a stuck chat worker past the 2 s budget can emit a second terminal event (the client
acts on the first); the `ReplicaName_s` column, ACA's probe logging during termination and the Workflows/Jira
payloads are unverified against live services; the desktop boot smoke test's CTRL_BREAK on GitHub's Windows runner
is unverified; `cmd/bakicli` has two load-sensitive tests (`TestWatch_ConfigDiscovery`,
`TestCLI_InvalidBaselineExits2`).

**Phase 4c: done** (2026-10-10, uncommitted). Migrations 0038–0040, all `expand`.

*Delivered:*
- **Organisation CI credentials** (R8, ADR 0019): `org_ci_credentials` (0038) holds server-minted keys
  (`baki_ci_<keyId>_<secret>`), sealed at rest, members-only under row-level security, at most 10 active per
  organisation, expiring after 1 to 365 days or never. Org admins list, mint (shown once) and revoke them
  (`/api/orgs/{id}/ci-credentials`, no API tokens), in Settings > Organisation > CI credentials with the `bakicli
  report` command to copy. `CIService` authenticates the webhook by `X-Baki-Key-Id`, then the deployment secret, then
  (old bakicli, no key id) the credentials of the organisation owning the flow; a credential reaches only its
  organisation's flows (`LoadFlowHeaderInOrg`, contract-tested). The run carries an integration actor, is audited
  (`ci.webhook_run`) and counted (`pad_ci_webhook_auth_total{method}`). With accounts the deployment secret is
  deprecated (boot WARN, `Deprecation` and `Link` headers). `internal/platform/hmacsig` is the one signing
  implementation (bakicli, the webhook, the channel envelope).
- **Socket actors from the ticket** (R7, ADR 0020): a ws-ticket carries the token's scopes and a version (`wsv`; a
  ticket without it is refused); the socket's actor is the PAT, not its user. Every inbound frame type declares what
  it needs (a completeness test walks the event constants; an undeclared type is refused), `block.*` needs `write`
  on the flow, rechecked every 2 minutes; refusals are counted (`pad_ws_inbound_refused_total`) and answered at most
  once per 5 s. `POST /api/ws-ticket` adds `expiresAt`. The first end-to-end WebSocket test runs through the router.
- **Channel-kind registry** (ADR 0021, `docs/webhooks.md`): one file per kind (webhook, Slack, Teams, Jira, email)
  declares its fields (secret or not), egress and signing; validation, building and the public view go through the
  registry. `ChannelService` owns the organisation channels: write-only secrets kept when omitted and removed with
  `clear`, the URL redacted in every list (`hasSecret` added), a stale tab's redacted URL read as unchanged, a test
  delivery on its own rate-limit bucket (`channel_test`) with a coarse answer. Typed channel routes. A versioned
  webhook envelope: `schemaVersion`, `id`, `deliveryId`, `orgId` in the body; `X-Baki-Event`, `X-Baki-Delivery`,
  `X-Baki-Timestamp` and a timed `X-Baki-Signature-V2` on every HTTP kind (the body-only signature stays);
  `User-Agent: baki/<version>`. One egress answer for tenant channels (`egress.ForDeployment`: accounts decide) and
  one for the operator's (`egress.ForOperator`). The scanner hands org-channel deliveries to the shared bounded
  runner (`GoWait`), and `new_rules` notices reach the channels once each. Payload goldens per kind and event.
- **Mail template registry** (ADR 0022): typed templates registered at init (each executed once against zero data, so
  a typo fails the boot), `html/template` for the HTML part; one bounded `mail` runner (32 slots, 8 kept for password
  reset, verification and invitations), so no request waits on SMTP; a MIME writer with a random boundary,
  quoted-printable parts, RFC 2047 subjects, `Date` and `Message-ID`; addresses parsed, never stripped; every SMTP
  phase bounded by the context. The governance alert email (notify's email kind, Jira's description) renders through
  the registry. Mail goldens per template and variant.
- **Safety nets:** payload goldens (26), mail goldens (48), the inbound-needs completeness test, 2 contract scenarios
  (CI credentials, org-scoped flow reads) and 3 for channels; `docs/testing.md` lists every test file again (it
  lacked about 100).

*Bugs fixed on the way:*
- **Channels:** every edit without a new secret, and every pause or resume, wiped the webhook or Slack signing secret
  (receivers then rejected or accepted unsigned); editing a paused channel re-enabled it; the save answered "now" as
  the creation time, and the fake overwrote it; org-channel events carried a zero time (`0001-01-01`, Slack `ts`
  −62135596800); Slack and Teams URLs, the credential itself, went into WARN logs and the error sink; the Jira payload
  used a plain-string description that Jira Cloud refuses; three egress predicates disagreed in local mode with
  accounts; the channel test shared the general rate-limit bucket; the field-encryption backfill would have resealed
  an empty URL on every boot.
- **Mail:** a fixed MIME boundary let a finding comment inject a third part (phishing inside an official email);
  forgot-password answered later for existing accounts (the SMTP round trip ran inline); a `Name <addr>` sender broke
  `MAIL FROM`; a CR/LF in an address was stripped, joining the next header; the assignment email named a flow the
  assignee cannot read.
- **CI webhook:** any tenant's flow was reachable with the one deployment secret (R8); local+db analyzed the open
  document instead of the stored flow; the dashboard was written on every call.
- **Sockets:** a read-scoped token's socket relayed `block.*` frames as an editor (R7).
- **UI:** the channels and custom-rules lists showed the state before a save for 5 s (GET micro-cache); opening Edit
  on a second channel kept the first one's fields.

*Behaviour changes users will notice:*
- **CI webhook:** desktop answers 503; with accounts a call nothing verifies is 401 (was 503 "not configured"), and
  the deployment secret's answers carry `Deprecation`; an unknown `format` is 400 (an old `bakicli -format json` now
  exits 2); the webhook is on the `analysis` rate-limit bucket, whose 429 now says `Retry-After`, and `bakicli
  report` retries a 429 up to four times; a `failOn` other than error, warning, info or none gates as `error`; one
  audit row per completed run.
- **Channels:** admins see the redacted URL too (`host/…`, no scheme), the edit form's URL field is write-only, a
  row says when it is signed, a secret can be removed and is refused on Teams; the save body is strict; deliveries carry
  the envelope headers and fields; org channels get `new_rules` notices; the channel test answers 502
  `CHANNEL_DELIVERY_FAILED` with a reason, 503 `CHANNEL_DELIVERIES_BUSY`, or 429; the list needs the server store
  (503 on desktop, where the UI shows no organisations).
- **Email:** `Date` and `Message-ID` headers, quoted-printable bodies, one-line subjects of at most 200 characters;
  `PAD_EMAIL_FROM` may be `Name <addr>`, and one that does not parse fails each send; under saturation an email is
  dropped and counted (`task="mail"`) instead of holding the request; every SMTP failure reaches the error sink
  (`mail.send`) as its SMTP phase and code only (`mail: rcpt: 550`); governance alert emails wait up to 10 s for a
  mail slot instead of being dropped; forgot-password no longer answers 500 when the token write fails (the token is
  written moments later, with the email).
- **Sockets:** a ticket from the previous release is refused for its 30 s life (the client reconnects); a PAT's
  socket is logged as the token.
- **Operations:** schema v40; the retention sweep also runs on local+db with accounts (it ran in cloud mode only);
  new metrics `pad_ci_webhook_auth_total`, `pad_ws_inbound_refused_total`; runner labels `finding_email` and
  `org_invite_email` become `mail`; new settings `PAD_RATE_LIMIT_CHANNEL_TEST_{RPS,BURST}`.

*Found in review and fixed:* the old-client CI fallback read the store for any flow id an unauthenticated caller
sent (a NUL made it a 500 with an ERROR log) and HMACed bodies of any size once per credential (now: plausible ids
and bodies up to 4 KiB, and every failure is the caller's 401); the webhook echoed and audited any `failOn` text;
a replayed mint handed the one-time credential out again (the mint is no longer idempotent); a stale or pasted
URL placeholder was stored over a channel's URL (the list now shows `host/…`, which no release accepts as a URL, and
a save refuses the marker), `urlUnchanged` with a new URL silently kept the old one, and a Teams row's legacy secret
listed as `hasSecret`; governance alert emails were dropped once 24 were in flight; the error sink received SMTP
replies with the recipient's address; forgot-password still wrote the token inside the request, so the response
time still told which accounts exist (the write now runs with the email on the mail runner); the scanner
stopped after the server had drained the runners, so alerts found by a sweep running at shutdown were recorded and
never delivered; a loopback `PAD_NOTIFY_*` URL on local+db with accounts failed the boot after migrating (the
operator's channels now use `ForOperator`); an account erased during a rollback left its id as a credential's
creator name; expired CI credentials were never purged on local+db with accounts; the deployment-channel egress
was documented wrongly; the new audit actions had no labels.

*Open items closed the same day* (a follow-up round, one item at a time, each merged and tested on its own):
- **CI webhook** (ADR 0019, migration 0039): a signed `deliveryId` in the body; a repeat from the same key is
  409 `CI_REPLAYED` (`ci_webhook_deliveries`, kept an hour, pool-only under row-level security), a call without one
  is accepted and counted (`pad_ci_webhook_deliveries_total{delivery}`). Personal CI access: an API-token scope `ci`
  (refused by every other route), `Authorization: Bearer` on the webhook, flows the token's user can read (an org
  flow also needs membership: a collaborator outside the organisation gets 404); `bakicli report -token`.
- **Sockets and streams** (ADR 0020): access is re-checked when it changes (a collaborator removed, a member removed
  or demoted, an organisation or flow deleted, an account erased, a logout, a deleted API token: `AccessRevoked`),
  through the event bus after the commit, coalesced per socket, at most 4 passes at a time; the 2-minute pass stays
  the backstop. An SSE stream of a revoked token ends at once too.
- **Encryption key rotation** (ADR 0005): `PAD_ENCRYPTION_KEY_PREVIOUS`; reads try the current key, then the older
  ones; a background re-seal pass per boot (one replica, resumable, compare-and-swap writes) moves secret columns,
  stored responses and the keystore to the current key and logs when nothing is left; no format change.
- **Channels** (ADR 0021, migration 0040): organisation `jira` and `email` kinds (`config`/`secrets` columns; email
  only to chosen members with a verified address, looked up again per delivery, removed by erasure); per-channel
  event subscriptions (default: today's set); `PAD_NOTIFY_ALLOWED_HOSTS`, enforced at save and at dial for
  organisation channels, shown to admins only with `PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE`
  (`GET /api/orgs/{id}/channels/options`); a `teams-workflow` kind (Adaptive Card of `TextRun`s); tenant text shows as
  typed in Slack (`&`, `<`, `>` escaped: no links or `@channel` from a flow name) and Teams connector cards
  (`"markdown": false`); a channel no configured key decrypts is listed as `unreadable` instead of failing its
  organisation's list.
- **Mail** (ADR 0022): `PAD_SMTP_MAX_CONNECTIONS`, a cap per relay with one connection kept for password resets and
  invitations; an unparseable `PAD_EMAIL_FROM` is an ERROR at boot (not a boot failure: the previous release sent it
  as it was).
- **Smaller fixes:** deleting another user's API-token id revoked that token's sockets and wrote a revoke audit row
  (now only the owner's delete does); a failed channel test on Windows said "the connection failed" (Windows' wording
  of refused, reset and timed-out connections is recognised); the plaintext backfill held up the shutdown drain (now
  cancelled first); `infra/main.bicep` passes the Teams and webhook URLs as secrets; the auth handler's unused mail
  dependency is gone; the heartbeat, scanner-pooling and Azure-model tests that flaked under load are deterministic;
  the gosec finding in `flow_snapshots.go` is gone (golangci-lint is clean).
- **Not a defect:** a retried API-token mint replays the same token to the same user (ADR 0007's decision; no check
  that could have changed runs after the replay, unlike the CI credential mint).

*Open:* mixed-version replicas and other replicas' sockets learn of a change at their 2-minute pass (Phase 5b);
removing the old-client CI fallback, the untimed signature and the deployment secret with accounts, and requiring
`deliveryId`, once their counters stay at zero; a captured bearer CI call can be resent with a new delivery id
(inherent to bearer tokens; a sender-constrained token if it matters); a nested v2 body and retiring the body-only
signature; retiring the `teams` kind when Microsoft's connectors stop; deliveries and mail stay at most once and in
process (ADR 0009); the Jira ADF payload and the Workflows card are unverified against live services; the
deployment channels receive every tenant's alerts (documented, B12); a global role change announces nothing (the
periodic pass catches it, for ownerless legacy flows only).

*Plan corrections:* the CI webhook's "output through the exporter registry" was done in Phase 4a; the phase took
four ADRs and two waves of parallel work, more than the estimated two weeks.

**Phase 4a: done** (2026-10-10, committed as c99e3ef). Migration 0037, `expand`.

*Delivered:*
- **Rule metadata at registration** (ADR 0014): each rule file registers `RuleMeta` (confidence, auto-fix, direct
  fixes, `DisabledByDefault`, options, related rules, `IntroducedIn`, `Revision`, `Stage`); the engine's side tables,
  the related-rules table and the fix switch are gone. Fixes register beside their builders (`registerFix`;
  `LookupFix`/`FixTypes` exported, registration not: no package outside the analyzer adds a fix). `RuleEnabled`
  answers an absent settings entry with the rule's default, for the engine, the inventory, the parse-error fallback,
  the library runner and the catalog. `RuleSetVersion` 1: all 45 rules `IntroducedIn: 1`, stable; eight at Revision
  2 (the determinism fixes below; a severity-only change keeps its revision). Completeness tests replace the hand-kept copies.
- **Preview stage and one gating predicate** (ADR 0014, 0016): `analyzer.Gates` (fail closed). Preview findings stay
  in the list and in `Stats` but leave the health score, `bakicli`'s exit code, the CI webhook verdict, policies
  (`PolicyResult.preview`), drift and regression; SARIF `level: "note"` with `properties.stage`, JUnit a passing
  testcase. No built-in per-flow rule is preview yet.
- **Rule kinds and lazy facts** (`docs/architecture/analyzer.md`): `BlockRule` and `FlowRule` (the eight whole-flow
  rules no longer fake a per-block check), one dispatcher for the walk and the suppression inventory, rule-only facts
  built on first use (one `sync.Once` per group). Same findings, order and fingerprints (a differential oracle over
  the corpus, the fuzz corpus and random graphs); memory per analysis down 13–51% across the analysis benchmarks.
- **Rule sets on baselines and snapshots, new-rule notices** (ADR 0016, 0037): an analysis records its rule set
  (`{ID, Rev, Stage}` per rule, an `rs1:` digest); baselines and health snapshots store it, valid only when written
  with the row (a row the previous release rewrites reads as legacy). The scanner sends drift alerts only for rules
  the baseline knew, compares regressions like for like, and raises one `new_rules` notice per scope (org, owner, or
  the deployment without accounts) and rule set in the bell, announced once; no channel delivery until Phase 4c. The
  CLI reads a v2 baseline file (`ruleSet`) and keeps writing v1.
- **Library rules** (ADR 0014): `LibraryRule` (extract bounded JSON facts per flow, then check across flows) in a
  separate registry, never in a per-flow report, cache, baseline or gate; the reference rule
  `duplicate-subflow-across-flows` (preview). Run by `bakicli` with several paths and by the desktop folder batch
  (`BatchAnalysis.library`); no cloud persistence or route.
- **Custom-rule schema** (ADR 0015, `docs/custom-rules.md`): `kind` and `schemaVersion`, one decoder and compiler in
  `pad-core`, strict on writes (save, validate, test, `bakicli rules test`, new `bakicli rules lint`), tolerant on
  reads, a newer release's kind or version refused in both; typed errors with stable reasons (`details.field`,
  `details.reason`); `MatchDigest` as a custom rule's revision. The org rule editor maps errors to fields, keeps the
  id read-only when editing, shows each rule's problem, and pauses without re-validating.
- **Export registry and one folder walker** (ADR 0018): `core/export` formats register themselves (`json` is one);
  the CLI, analyze-raw, both export APIs, `ExportService` and the CI webhook render through it; the
  `SetToolVersion` global is gone (`Options.ToolVersion`). `bakicli` analyze, fix, watch and `-new-from-rev` share
  `parser.ParseFolderTree`/`ListFolder`; `.bakiignore` uses `path.Match` on every OS. The plan's `ServerRendered`
  flag was dropped: only the UI's findings CSV is client-rendered, and it is a different document.
- **Declared caches** (ADR 0017): `cache.Declare` + `cache.New`/`GetOrFill` with typed key parts (a tenant part or
  a `Global` reason), `pad_cache_*` metrics, a caches golden. The analyzer cache is an injected
  `*analyzer.CachedAnalyzer` keyed by tenant, flow, analyzer version, rule profile (`ProfileDigest`, which folds stage
  and revision) and content plus parse identity; 17 service caches moved onto specs (19 declared). Ratchets: raw
  cache constructions 10 → 1 (the undo ring), `singleflight.Group`s 13 → 11; memos left off the registry are listed
  with their reason. `pad-core` builds standalone.
- **Safety nets:** findings goldens (55 inputs × 6 settings variants), export byte goldens (5 reports × 7 formats),
  the caches golden, the rule-order pin, determinism tests; 9 new contract scenarios (custom-rule renames and
  isolation, rule sets, notices); typed endpoints for org rule list and save (hand-written routes in
  `handlers_orgrules.go` 4 → 2).

*Bugs fixed on the way:*
- **Determinism:** circular-subflow detection reported a different set of cycle members from run to run (one member
  in only about 40 of 300 runs); seven property rules picked the reported property in map order, so their
  fingerprints could flap between runs (and `hardcoded-credential` its finding order); the sanitize-command-vars
  patch applied its edits in map order.
- **Severities:** `bakicli` without a config graded `hardcoded-credential` `warning`, the server `error`. Settings
  flattened rule-graded severities: parse errors the parser grades `warning` became `error`, and
  `sql-injection-risk`'s taint escalation to `error` was reset.
- **Analyzer cache:** analyze-raw served one caller's report (root name, document and block ids) to another who posted
  the same files; repeated analyze-raw posts got block ids their document does not have (SARIF lost its line
  numbers); the suppression inventory showed another flow's line numbers; `"8"` and `8` options shared a report.
- **Service caches:** a non-member collaborator's resolve filled the org's rule profile (hiding the org's custom rules
  from members for 30 s) and a member's showed them to the non-member; a desktop file opened with another file's
  bytes became that flow (the parsed-document key ignored the name); admin and non-admin sessions shared an analytics
  view.
- **bakicli:** several path arguments analyzed only the first (a fail-open gate for `bakicli flows/*.txt`); status
  lines on stdout made SARIF and JSON invalid with `-baseline`, `-update-baseline` and `-new-from-rev`; an invalid
  baseline exited 1 after writing the report; `fix` and `watch` walked `.git`, followed symlinks and, on Windows,
  ignored `.bakiignore`'s `vendor/` and directory patterns, so `fix` changed files `analyze` never reported; a missing
  named custom-rules file was ignored; the diff-reports gate did not fail closed on an unknown severity; SARIF always
  said version `0.1.0`; `init` printed "(0 rules)".
- **Custom rules:** unknown fields were dropped silently, so a misspelled matcher made a rule that fires on every
  block; renaming a saved rule was a 500 on Postgres, and renaming onto another rule's id overwrote it; another org's
  row id was a 500; pausing re-validated the config, so a legacy rule could not be paused; saving an edit re-enabled a
  paused rule; the editor kept the previous rule's form when another was opened.
- **Scanner:** after a restart or failover a persisting drift or regression was sent to every channel again (a Jira
  bug each time).
- **UI and API:** the findings JUnit export always failed ("non-JSON response"); the SARIF download was re-serialized;
  the Findings tab dropped the Portability, Syntax and custom-rule categories; the catalog showed no fix for a rule
  that stamps its own (`subflow-mismatch`); the CI webhook's SARIF was not valid SARIF 2.1.0 (`logicalLocation`).
- **Suppression inventory:** a directive over an `empty-branch` finding was reported stale while it suppressed the
  finding; one panicking rule broke the inventory.

*Behaviour changes users will notice:*
- **Findings:** parse errors keep the parser's `warning` and tainted SQL stays `error` under settings, so health
  scores rise for flows with parse warnings and an `error` gate fails on tainted SQL; `circular-subflow-dependency`
  flags every subflow on a cycle and `circularDependencies` lists each once; the property rules report the first
  qualifying property in sorted key order (one wave of changed fingerprints on such blocks, a notice rather than
  drift).
- **bakicli:** hardcoded credentials fail the default `-fail-on error`; `-format markdown|pdf` (`pdf` refuses a
  terminal); several paths with `text`/`json` (one report per flow plus a library section; other formats exit 2);
  a flag after a path, an invalid or newer baseline, a newer custom rule and a missing named rules file exit 2;
  status lines go to stderr for non-text formats; SARIF carries the CLI's version (`dev` from source); custom-rule
  warnings on stderr; `rules test` is strict; `rules` shows a STAGE column, the library rule, and `kind`, `stage`,
  `introducedIn` in JSON; `fix` and `watch` act on `analyze`'s file set; `.bakiignore`'s `*` no longer crosses `/`
  on Windows, and patterns it cannot use are warned about.
- **Server:** the rule catalog gains `stage`, `introducedIn`, `kind` and the direct fixes (auto-fixable KPI +1), and
  lists the library rule without accounts; baselines return `ruleSetDigest`, drift `newRuleIds`, `newFromNewRules`
  and `preview`, policies `preview`; the CI webhook's `format=sarif` is the core SARIF as `application/sarif+json`
  (indented, compressed) and its verdict gains `preview`; analyze-raw SARIF/JUnit downloads are attachments and
  analyze-raw records no diff pair or history snapshot; org rule saves are strict (400 with `details.field` and
  `details.reason`), a rename or a newer release's rule is a 409, and pause/resume sends no config; the org rules
  list adds `problem`, and the export keeps `kind`, `schemaVersion`, unknown fields and skipped rules (with
  `problem`); validate answers per entry; the desktop folder batch adds `library`.
- **Governance:** the first sweep after the deploy re-scans every flow and reads every baseline and snapshot as
  legacy, so the findings of the eight revised rules arrive as one `new_rules` notice per scope instead of drift;
  a health snapshot from before the upgrade is not compared: the sweep records its own analysis as the flow's
  snapshot instead, so the dashboard shows the current rules' health for flows nobody opened; alerts already in the inbox are not resent after a restart.
- **Gates:** a severity configured as anything but error, warning or info counts as an error everywhere (it used to
  pass the CI webhook, policies, health and drift, and fail only `bakicli`); `POST /api/analysis/rule/config`
  refuses one. A cloud deployment that never saved its settings graded `hardcoded-credential` `warning`; it is now
  `error` there too, so its CI webhook (`failOn=error`) and error-gated policies fail on hardcoded secrets. A
  non-member collaborator can no longer set an org flow's baseline (403); a member or the owner can.
- **UI:** preview badges on rules and findings, "Across flows" on library rules; the Findings tab shows every
  category (the filter now hides categories; saved views are converted); JUnit export works and SARIF is saved as
  the server sent it; the custom-rule editor shows field errors, warnings and problems and makes a newer release's
  rule read-only; the AI's `list_findings` marks preview findings.
- **Operations:** schema v37; in-process caches start cold (`analyzerVersion` 5); `/metrics` gains `pad_cache_*`
  for 19 caches; `GET /api/analysis/dependencies` is sorted; a desktop "fix all" on `my.flow.txt` reloads it as flow
  "my.flow"; stored-rule warnings and a missing `PAD_CUSTOM_RULES` file are logged.

*Found in review and fixed:* the cache's fill could store a value read before a concurrent invalidation (the guard
was checked apart from the store); a rule-set record that carried only its digest, or rules not matching it, made
every rule "new" and silenced drift and regression for the flow; the regression check dropped revised rules from
the current score while the snapshot still counted them, hiding real regressions; a custom rule under the library
rule's id was accepted and would never gate; baseline reads sent the full rule set (org custom-rule ids) to any
flow reader; notices of flows skipped as unchanged were purged by retention and announced again; a newer release's
org rule that retyped a field read as invalid, and saving its echo replaced it with a match-everything rule;
membership changes left a member's rule profile stale for 30 s, and a pool read was cached as the caller's scoped
view; several flows shared one baseline key set, so one flow's baselined finding hid the same new finding in
another; `bakicli report` passed any `-format` spelling other than `sarif` ungated, `diff-reports` read a
several-flows report as empty, and a flag after a path was ignored; analyze-raw entries evicted stored flows'
reports and stopped counting as analysis runs; the webhook's SARIF lost compression; `Format.Bytes` bypassed a
replaced `Render`; the inventory allocated 46% more than before the dispatcher change.

*Final review* (six dimensions; engine, caches and modes re-run after the session ended): a custom rule under a
preview or library rule's id never gated (its findings are now stamped `stable`, which the gates trust over the id,
and take no built-in metadata); an unknown configured severity passed every server gate (normalised, unknown counts
as an error, the rule-config route refuses it; JUnit, HTML, Markdown and PDF follow); `bakicli report -format sarif`
cut the answer at 16 MiB and exited 0 (streamed; a cut connection exits 2); a stored org rule that does not decode
echoed an empty stub that a save turned into a match-everything rule (the fields that decode are kept); the new
bundle could not pause a rule on a rolled-back server (it retries the old way) and "New rule" silently replaced the
rule with that id (refused in the editor); a non-member collaborator's analysis on a scoped route replaced the
flow's recorded report, so members exported and diffed without the org's custom rules, and could set the org
flow's baseline from that view (a non-member's view is `Narrowed`: cached apart, recorded nowhere, and
`baseline/set` answers it 403); an unchanged desktop folder reload pushed the previous run out of the
diff; `parse-error`'s severity-only change carried a revision that turned its new findings against older baselines
into a notice instead of drift; the first sweep after the upgrade raised a health regression for unchanged flows
(a legacy snapshot is not compared; the sweep records its analysis as the new snapshot); policy severities and the
catalog's configured severity are read like the engine's (case, spacing, unknown as error); the parse identity hashed each block's subflow id again (now only where it
differs). The final test runs exposed a Windows race outside the phase: a conversation file replaced by rename while
a polling GET conversation held it open failed the save (the finished answer was lost) or the read (a 500); both
now retry the transient sharing conflict (`atomicfile`), as the other on-disk stores do. Also closed from the open
list: a newer release's rule that retypes a field reads as "needs a newer
version" in a rules file; the toolbar's "N new" counts drift only.

*Open:* the desktop UI does not show the folder batch's library findings yet (the JSON carries them); routes
without row-level security still give a non-member collaborator the org's custom rules (a request-path membership
decision, ADR 0013); a notice is per rule set, so rules still newer than old baselines are announced again at the
next rule change until the flows are re-baselined; a revised rule counts on both sides of a regression, and a rule
disabled or demoted since the snapshot lowers only the snapshot's score; a regression made between a flow's last
snapshot before the upgrade and the first sweep goes unreported; a stale SPA opens no flow for a `new_rules` entry; the
policy editor may offer library and preview rules where orgs and the folder batch coexist; the CI webhook answers an
unknown `format` with the summary; block-less parse-error findings carry no category, and subflows without blocks
are never flagged; `duplicate-action` panics (recovered) on duplicate subflow ids, which the parser never emits; the
desktop's `LoadFlowFolder` keeps its own size pre-check, and a folder reload re-parses with new ids, so it analyzes
again (no cache hit); a pause racing a full save can restore the older config; the editor's Try it refuses a legacy
rule under a built-in id; a `.bakirc.json` whose inline newer-release rule retypes a field fails as a whole (exit 2)
instead of "needs a newer bakicli"; a symlinked root folder lists nothing; `bakicli watch` takes one path (no
library pass), and one path given twice is two flows; JUnit, CSV and Markdown downloads stay uncompressed; library
extraction builds a second rule context per flow; `docs/testing.md` still lacks about 80 test files of earlier
phases.

**Phase 3: done** (2026-10-08, committed as 6b87f4e). Migrations 0034–0036, all `expand`.

*Delivered:*
- **The test fake meets the storage contract** with no known divergences, strict mode included: real users (email
  uniqueness, the bootstrap-admin rule), collaborators, versions, comments, policies, refresh tokens, sessions and
  identity links, invitations and `MutateOrg`, per-user and per-org settings, the flow filter and its sort and limits,
  optimistic versions, foreign keys, and the lifecycle semantics below. 35 new contract scenarios (`semantics.go`,
  `lookups.go`, `lifecycle.go`, `bugs.go`, `comments.go`) pass on Postgres and the fake.
- **The filesystem backend is gone as a store.** 66 test files moved onto the fake (API, DI and service tests), and
  `internal/storage/filesystem` is only `OpenLegacySource`: read-only (never creates, writes or renames), no index,
  with its own contract (`contract.RunLegacySourceSuite`). The legacy migrator runs on it through a declared step list
  and copies flows with their original timestamps (`FlowStore.ImportFlow`).
- **Storage capabilities** (ADR 0010): the domain capabilities are methods of the role interfaces (finding status,
  API token owner, org role and memberships, the library page, audit batches, the export stream, refresh tokens,
  identity links, `MutateOrg`, invitations); Postgres-only ones are fx values, nil without a database (one
  `lock.GlobalLocker`, `reqtx.Opener` for the request transaction, the keystore's org and family capabilities, field
  encryption, the audit backfiller, the blob probe, the Azure registry). Type assertions on stores: 30 → 0, pinned by
  `TestArch_StorageTypeAssertionsRatchet`; `internal/api` no longer imports the database package.
- **Tenant scoping** (ADR 0013): row-level security on `flow_analysis` and `share_tokens` (0034); collaborator
  statements reach their grants through the RLS-protected `flows`; an empty author can no longer delete a comment
  (moderation is its own method); the cross-tenant system reads are pinned to the scanner; every table without RLS is
  listed with its reason.
- **Data-lifecycle registry** (ADR 0011, `docs/architecture/data-lifecycle.md`): each domain claims its tables and
  declares what export, account erasure, organisation delete, retention and legacy migration do with them, in
  explicit phases; a coverage test fails on an unclaimed table or a cascade claim no deleting parent backs; an erasure
  sweep scans every column of every table for the erased user.
- **Settings in versioned sections** (ADR 0012, 0035): one row per scope and section with its own version, upgraded
  on read, never downgraded or truncated on write (unknown fields survive); written alongside the whole-document blob
  for one release, with a digest so a blob changed by the previous release after a rollback wins.
- **Per-domain files**: the storage interfaces (24 files), the contract scenarios, the knowledge store; an SQL argument
  builder (`sqlArgs`) replaces the hand-kept `$n` counters with identical SQL text; transactions opened outside
  `withTx`/`withTxAs`/`withOwnTx`: 12 → 6 (`docs/architecture/transactions.md`; no public `Tx`, since requests share one
  transaction and `reqtx.Opener` serves the rest).

*Bugs fixed on the way:*
- **Account erasure (GDPR):** left the conversations of the user's own flows (no foreign key); ran inside the
  departing user's own RLS scope, so it missed their rows on flows they could no longer open; kept their identity on
  other people's rows (comment author, assignee, share-link creator, alert target, inviter, org owner, credential
  editor), on failed logins for their address, and on the audit rows the deletion request itself wrote.
- **Export:** carried flows shared with the user (it claims the user's own data), other members of each organisation,
  and ignored a settings read error.
- **Organisation delete:** left its knowledge base, policies, channels and custom rules, and its flows pointing at it.
- **Policies** saved on Postgres lost their waivers and severity budgets (0036). A library page past the end
  reported 0 flows. Local mode on a database answered 500 to a settings save. `perUserDailyBudget` was dropped by
  every cloud settings save. A tag filter treated `_` as a wildcard. A rejected migration start changed who adopts
  the running migration's flows. Comments with the same timestamp came back in no fixed order.
- **Tests touching the developer's machine:** the API test router used the real desktop settings file and the OS
  keychain (a test deleted the stored Copilot token); two DI tests opened the real settings file. Both packages now run
  on a temporary config home and go-keyring's mock.

*Found in review and fixed:* the erasure's alert rewrite matched substrings and display names across every tenant
(it now matches whole messages on flows the user reached, including ones they have since lost); a settings save and
an account or org delete could deadlock (the save now locks the owner row, the blob, then the sections, the
delete's order); the next-release settings plan would have brought back stale sections after a rollback (ADR 0012
amended); the coverage test accepted cascades through tables the operation only updates; the DeleteFlow and
comment-delete scenarios passed on broken stores; a burst of sign-ups failed about a quarter of the time on
SERIALIZABLE conflicts (registration now runs at READ COMMITTED, with an advisory lock only while the table may be
empty).

*Open:* orgs left without an admin by an erasure keep no owner (product decision); the user's flows inside an
organisation are deleted with the account, as before; several personal tables are not exported yet (identity links,
usage, share links the user created, version history); an anonymised comment shows no author in the UI; the frontend
`Policy` type lacks waivers and budgets; local mode on a database keeps deployment settings in the desktop settings
file (the provider checks the mode, not the server store); SSO sign-up writes the user and the identity link in two
transactions; a knowledge upload can leave a document without chunks after a crash; removing a non-member or changing
a non-member's role answers 500 instead of 404; the contract `KnownDivergence` machinery has no user left.

**Phase 2b: done** (2026-10-07).

*Delivered:*
- **Domain events** (ADR 0009): typed events in `internal/events` (imports nothing of the module) and an in-process
  bus in `internal/platform/eventbus`. Sync tier for cache invalidation: runs immediately and again after the
  transaction commits or rolls back (`txhook.OnRollback` is new). Async tier for side effects: after the commit only,
  on a bounded runner drained at shutdown, with a timeout and panic recovery per delivery. Metrics per event and
  subscriber; a test recorder.
- **The four hook setters are gone:** `SetCloudFlowMutationHook`, `OnInvalidateFlow` (now `FlowChanged`),
  `OnApiKeyChanged` (`CredentialChanged`), `OnUserDeleted` (`AccountErased`). Also published: `SettingsChanged`,
  `OrgMemberChanged`, `KnowledgeDocChanged`, `FindingStatusChanged`, `FindingAssigned`, `FindingCommented`.
- **Generation-checked fills** (`eventbus.Gen`): the dashboard home and analytics, chat scrubbed context, deployment
  settings, rule profiles and the knowledge corpus count refuse a fill read before a concurrent invalidation, or
  inside a transaction that published (it may be reading its own uncommitted writes).
- **Stale windows closed:** pre-commit invalidations followed by a refill of the old row (up to 30 s in the
  dashboard, rule profile and settings caches); the chat context's LRU-of-256 generation resetting on eviction;
  deployment rule toggles never invalidating rule profiles; org delete leaving its rule profile, corpus count and
  model scope cached; other members' org analytics views staying stale after an org flow changed.
- **After the commit only:** WebSocket `flow.changed`, the comment and assignment bell rows and emails, and disabling a
  departed lender's shared credential (before, a rolled-back membership change still lost the credential). The last
  two run inline if the bus runner is full, so they are never dropped.
- **SSE audience is mandatory:** `EmitTo`, `EmitToOrg` (roster read at delivery) or the explicit `EmitToAll`; an
  empty user no longer broadcasts to every client.

*Found in review and fixed:* `flow.changed` carried the stored version, which a viewer could already hold over stale
content and then skip (it now sends a stamp, as before); the bell rows and the lent-credential revocation could be
dropped by a saturated bus runner.

*Open:* `settings:changed` is still emitted directly (driving it from `SettingsChanged` would need payload reloads
and add pushes for rule toggles); the frontend's flow reload records the metadata version for content read in
parallel (a stamp in `flow.changed` makes it harmless now); `OrgService.Create` publishes no `OrgMemberAdded` for the
owner; the legacy migrator publishes nothing; audit rows are still written for rolled-back requests; delivery is at
most once (no outbox).

**Phase 2a: done** (2026-10-07, uncommitted).

*Delivered:*
- **Services own the storage work.** Handlers make no direct storage calls any more: the `backend.X` ratchet is 0
  (from 85), and a second ratchet (`internal/archtest/storehandles_test.go`, go/types) pins the remaining
  infrastructure store handles per file (PAT auth, the Idempotency-Key store, the migration lock, the Redis health
  probe). New services take narrow role interfaces: `TriageService`, `CommentService`, `ShareTokenService`,
  `PolicyService`, `GovernanceAlertService`, `OrgRuleService`, `ChannelService`, `UserDirectory`,
  `CollaboratorService`, `SessionService`, `IdentityService`, `APITokenService`, `UserAdminService`,
  `HealthService`, and `AccountLifecycleService`, which runs erasure as prepare → delete → finalize in the old order.
- **`internal/audit`** (ADR 0008): an fx-managed `*audit.Recorder` replaces the package-global pool in `api`;
  `Record(ctx, audit.Entry{…})` takes the user, actor, token, IP and user agent from the context. Every handler
  records through its injected recorder (83 sites; `logAudit` and the global default are gone) and passes the
  organisation where it is already in hand. Migration v33 adds `org_id`, `actor_kind`, `token_id`, `actor_job` and
  `user_agent`; a boot-time job backfills `org_id` under an advisory lock, in statements of at most 1,000 rows.
  The existing columns are byte-identical to before (tested across actors, proxies and address forms).
- **DI:** `AIBudget` and `FlowScopeResolver` are extracted, the other setters became constructor parameters, and
  `internal/di/setters_test.go` pins the remaining ones, each with its reason (`docs/architecture/di-wiring.md`).
- `apperr` moved to `internal/platform/apperr`, so services may return typed errors.

*Found in review and fixed:* the flow phase of the backfill could exceed the pool's 30 s statement timeout on a
large table and then fail at the same page on every boot; erasure left the user agent on the user's pre-auth rows.

*Open:* audit rows of personal flows and of flows deleted before v33 keep a NULL organisation (by design);
`FlowService.SetRuleProfileResolver` stays a setter (55 test call sites); the four event-hook setters are replaced
by Phase 2b's event bus.

**Phase 1b: done** (2026-10-07, uncommitted).

*Delivered:*
- **Typed endpoints** (`internal/api/endpoint.go`, `page.go`, `apperr`): `JSON`/`Query` adapters run the strict
  decode, URL parameters, page bounds, `Validate()`, `AuthorizeFlow` and the function in a fixed order, and refuse a
  misdeclared request when the handler is built. `render` honours `*apperr.Error` (byte-identical envelope for every
  other error). `Page[T]` and `PageRequest`; conventions in `docs/architecture/api-conventions.md`. No existing
  handler was migrated; `TestArch_RoutesUseTheTypedAdapter` pins the hand-written routes per file, so new routes use
  the adapter.
- **`Idempotency-Key`** for 14 create routes (`route.Idempotent()`, ADR 0007): migration 0032 (the first `.sql`
  step, `expand`), the claim inside the request transaction under an advisory lock, a concurrent duplicate answers
  409 `IDEMPOTENCY_IN_PROGRESS`, another body under the same key 422, bodies sealed at rest, 24 h retention purge.
  Postgres store, fake and a contract suite. CORS allows the header and exposes `Idempotent-Replayed`.
- **Frontend:** tolerant enums (`tolerantEnum`, `Tolerant<T>`) for provider, model, Azure, alert, channel and
  triage values; roles and finding severity stay closed. `X-Baki-Version` on every response and a reload banner,
  offered once per server build; the image stamps the version into `index.html`, not a chunk.

*Deferred:* the frontend does not send `Idempotency-Key` yet (one key per user action, and `fetchWithRetry` could
then retry those POSTs); cursor/sort helpers until the first endpoint needs them.

**Phase 1a: done** (2026-10-07, uncommitted; the rollout flag defaults to `shadow`).

*Delivered:*
- **Modules and declared route policy** (ADR 0006): every handler is an `api.Module` contributed through the fx
  `routes` group and declares its routes and policy in `Mount(rb)`. The `Handlers` struct, `ProvideHandlers` and the
  central route list are gone. The request is resolved once in the outer layer; each endpoint's guard checks chi's
  match against it (500 on a mismatch in declarative mode); a route not registered through the builder fails the
  boot. `PAD_ROUTE_POLICY=legacy|shadow|declarative` (Bicep `routePolicy`, compose), shadow counting mismatches in
  `pad_route_policy_mismatch_total`. The metrics label is the matched pattern (dashboards: `:id` becomes `{param}`).
- **Request info and actor:** `reqinfo` and `auth.Actor` (user, token, system); the access log's `user_id` is
  filled and app logs carry the request ID (`logger.FromContext`).
- **Authorization through `AuthzService.Can`**, token scopes included (closes the S2 class): flows (27 sites, route
  middleware for the path-parameter routes), orgs, `/api/admin` as one group middleware, the WebSocket flow checker.
  Four sites stay on their role checks because `Can`'s scope rule would tighten what a token may do today (rule
  enable/config, GET snapshots, GET invites, chat save/clear).
- **Mode gating:** `route.Needs(capability.ServerStore)` on 52 routes replaces their handler 503s.
- **CI:** the previous release boots against the new schema (`previous-release-boot`).
- **Security fixes found while building:** the SSO flow cookie is signed and verified (an attacker could link their
  IdP identity to a victim's account); `/swagger/*` accepted any `Authorization` header.

*Deviations from the plan, and why:*
- Org admin writes that also run on the desktop's in-memory org store (rename, delete, invite revoke, role change)
  use `AuthorizeOrg`, which reads the org first, because `Can` allows every org action without storage; one roster
  read in cloud.
- On the desktop, a few routes now answer 503 before validating their body or session (account delete/export,
  admin role PUT, collaborator add/update), because `Needs` is decided before the handler.

*Open:* flip to `declarative` after a soak with zero shadow mismatches, then delete the legacy tables one release
later. Tightening the four role-check sites to `Can` is a product decision (it changes what existing tokens may do).
`securityAuthz` builds an `AuthzService` per call where the handlers have no injected one.

**Phase 0b: done** (uncommitted).

*Delivered:*
- **`internal/platform/txhook`:** a post-commit registry that is pending, committed or aborted.
  - Aborted hooks never run, including ones registered after the rollback.
  - Late registrations after a commit run at once.
  - A panicking hook is contained.
  - All 12 call sites moved to it, so API and service code no longer import the storage package for it.
- **`withTx(ctx, func(txCtx, tx))`:** when it opens its own transaction, `txCtx` carries a registry committed
  after that transaction commits. The three hand-rolled copies (SaveFlow, SaveFlowVersion, the Azure owner lock)
  now use it.
- **`storagedb.RLSHookRunner`:** each hook gets a fresh lazy RLS scope for the same user.
- **R5:** the RLS middleware commits before the first response byte (`txResponseWriter`). A failed commit becomes
  a 503 with the entry headers restored, and a panic after the commit still runs the post-commit work
  (ADR 0004).
- **`lifecycle.Bounded`:**
  - in-flight and duration metrics;
  - panics reported at ERROR through the error sink;
  - every runner registered with a group that `lifecycle.DrainAll` drains at shutdown. It waits for all runners
    to go idle before closing any, so a chained hand-off is not dropped.
  - `Router.DrainBackground` is gone.
  - New runners: the invite email (R4), admin jobs (`runDetached`, now 429 when saturated) and AI usage writes.
- **S3:** channel URLs and secrets are encrypted at rest with the keystore key, plus a one-shot backfill per boot
  under a cross-replica lock (ADR 0005).
- **Ratchets lowered:** API files importing the storage package, 4 → 1; transaction sites outside `withTx`, −3.

*Deviations from the plan, and why:*
- **The RLS context key stays** in the storage package. Hook contexts come from a `txhook.Runner` that the
  transaction's owner supplies, which needs no move.
- **`txhook.AsSystem` is deferred** until a consumer needs a system scope (Phase 2b).

*Found while building:*
- Two audit EXPLAIN tests failed on every fresh database, CI's included. They passed only after earlier runs had
  scrambled the shared table. Now each gets its own database and a seed in random physical order.
- A stray trailing space in `postgres_lock.go` broke gofmt; removed.

**Phase 0a: done** (2026-10-06, uncommitted).

*Delivered:*
- **Migrations:**
  - compat markers recorded in `schema_migrations`;
  - a boot guard that accepts expand-only newer schemas (ADR 0002);
  - an embedded `.sql` loader for v32+ that strips the BOM and CR, with `.gitattributes`;
  - a per-step `lock_timeout` with retry;
  - a checksum golden.
- **Deploy:** a startup probe plus a longer smoke-test window (ADR 0003), and the schema-aware rollback guard.
- **Test harness:** `internal/testutil/pgtest`, isolated databases cloned from a migrated template; CI grants
  `CREATEDB` and creates `baki_template`.
- **Safety nets:**
  - `TestAppGraph` (`fx.ValidateApp` over main's graph);
  - the route-policy golden (1,249 requests);
  - the authz matrix golden;
  - the storage contract run against FakeBackend (9 known divergences listed);
  - architecture ratchets in `internal/archtest`.
- **Benchmark:** `BenchmarkBuildHandler`, plus a nightly same-runner benchstat job.
- **Conventions:** `internal/capability` with the profile matrix, `docs/architecture/*`, `docs/adr/*`, and a root
  `CLAUDE.md`.

*Deviations from the plan, and why:*
- **Boot ordering.** A startup probe instead of binding the listener before migrating. The goal (liveness never
  kills a long migration) is reached with configuration, not a restructured boot; see ADR 0003.
- **Test routers from the real fx modules** moved to Phase 1a, because it needs the `api.Module` seam. Phase 0a
  validates main's full graph once: deployment modes change values, not the graph's types.
- **The "previous release boots on the new schema" CI job** waits until a release containing the compat guard
  exists, since older binaries refuse every newer schema anyway. `TestMigrate_AcceptsNewerExpandOnlySchema`
  covers the guard.
- **Ratchets as a Go test** instead of golangci `depguard`/`forbidigo`: pinned per-file counts that must also
  shrink are not expressible in golangci.

*Found while building the nets:*
- A knowledge-documents plan test passed 2 of 3 query arguments. It failed whenever `DATABASE_URL` was set;
  now fixed.
- The benchmark's save case timed `409` responses until it sent the stored version.
- Recorded for Phase 1a to keep or fix deliberately:
  - `GET /x%2F..%2Fshared` is charged to the general rate-limit bucket while the `/shared` fallback serves it;
  - read-style POST endpoints need the `write` scope for API tokens.

## Context

Baki's backend is about 100k lines of Go: the root module (`internal/*`, `main.go`) plus `./core`, which is the separate `pad-core` module (parser, analyzer, export).

The base is solid:
- fx dependency injection
- per-domain services
- storage role interfaces
- RLS-aware `withTx`
- advisory-lock-guarded jobs
- Redis backplanes for rate limiting, WebSocket and chat resume
- self-registering analyzer rules
- size-adaptive compression and byte-weighted caches

The problem is the cost of change. One feature touches **6–10 central files** (commits `9cc25c0`, `7218f55`, `b613b02`). Correctness depends on many hand-synced lists, and some have already drifted into bugs.

**Goal:** a feature is one self-contained module. Cross-cutting policy is declared once, next to the feature:
- auth and scopes
- rate limits, RLS mode, timeouts and metrics labels
- audit
- cache invalidation and realtime
- data lifecycle
- migrations

Invariants are enforced by tests and linters, not comments.

**User decisions:**
- All four extension areas matter: product features, AI providers and tools, analyzer rules and exports, and scale-out.
- **Desktop/local mode stays as it is** (nil storage backend, cloud features off), but becomes explicit through a mode capability matrix.
- The filesystem backend is frozen as a read-only legacy migration source.
- The test fake stays and gets contract-tested.

**Constraints:**
- URLs and JSON stay unchanged, except for the flagged frontend work.
- Each phase ships on its own.
- Never commit; the user commits or tags at phase boundaries, and those are the rollback points.

**Effort for one developer:**

| Tier | Contents | Time | What it unlocks |
|---|---|---|---|
| Tier 1 | Fix-first + Phases 0–2 | about 5 months | Cheap new product features |
| Tier 2 | Phases 3, 4a, 4c, 5a | about 3 months | |
| Tier 3, on demand | 4b when a new provider is scheduled; 5b when more than one replica is needed | about 2.5 months | |

### Review log

- **Pass 1 (revision 1 → 2):**
  - Desktop mode has a nil backend (filesystem is legacy only).
  - Jobs already hold advisory-lock leases; the real scale-out blocker is the in-process SSE hub plus per-process chat state.
  - Per-route `r.With` would reorder middleware.
  - Policy tables are about 15, not 4; past bypasses came from path normalization.
  - Post-commit and event-bus semantics needed rework; only 4 of 20 setters are hooks.
  - Counts corrected; the frontend is affected.
  - The no-transaction migration mode was unsafe.
  - Bugs found: B1–B8.
- **Pass 2 (revision 2 → 3):**
  1. Auto-rollback after a migrating deploy **crash-loops** (verified). Migration discipline and boot ordering move to Phase 0a.
  2. New security bugs, both verified: the org channel list exposes webhook URLs to any member, and a chat-scoped PAT can mutate flows.
  3. B1 is latent (frames are inert), and B4 and B6 are rarer than stated. Re-ranked.
  4. Post-commit registry needs **three states**: aborted hooks must never run.
  5. Hooks need a user-scoped RLS context, because stripping it fails open.
  6. `withTx` gets a context-carrying callback.
  7. Route resolution design hardened: mount stubs, 404 vs 405, the decoded-path no-route case, a guard wrapper, and shadow-mode rollout.
  8. Phase 2b is not race-free as written; fills are now generation-checked.
  9. Narrowing the filesystem backend breaks **55 test files**; the order is now fixed.
  10. Migrations are not frozen at v31: B3 and audit `org_id` need new ones, so the loader moves to Phase 0a.
  11. Phase 0 split into 0a and 0b; RequestInfo moved to Phase 1a.
  12. Added: Postgres test harness, observability, Redis payload versioning, graceful stream drain, Redis failure modes, new-rule alert floods, rule types, AI budget and metering, generic approvals, RAG embedding identity, performance baseline, conventions docs.
  13. Corrected:
      - Flow-ID sites are 27: 21 body, 3 query, 3 path.
      - Only the scanner and retention sweep fail open on lock errors.
      - The provider `id` is already a plain string in zod; only `authType` is closed.

---

## Fix first (verified bugs, each small and independent)

| ID | Severity | Bug | Evidence | Fix |
|---|---|---|---|---|
| R1 | **High, ops** | After a deploy that ran a migration, the smoke-test auto-rollback restarts the old image. The old binary refuses a newer schema, so the app **crash-loops**. | `internal/storage/database/postgres_migrations.go:343-349`, `.github/workflows/deploy.yml:329-338` | Now: make the rollback step schema-aware (if `schema_migrations` advanced, alert instead of swapping the image). Full fix: Phase 0a migration discipline |
| S1 | Medium, security | `GET /api/orgs/{id}/channels` returns the full webhook **URL** to any org member. For Slack/Teams the URL is the credential. | `internal/api/org_channels.go:38-56` (`requireMember`) | Admin-only list, or redact the URL to host + masked path for non-admins |
| S2 | Medium, security | A PAT scoped only to `chat` can **mutate flows**: `/api/chat/fix-decision` maps to the chat scope, and `applyApprovedFix` checks only the user's flow role. | `internal/auth/apitoken.go:89`, `internal/api/handlers_chat.go:431`, `internal/service/chat_fixes.go:362,515` | Require `write` for fix-decision now; Phase 1a makes `authz.Can` check actor scopes for every mutation |
| S3 | Medium, security | Channel `url` and `secret` are plaintext `TEXT` columns. Provider keys, by contrast, use the encrypted keystore. | `postgres_migrations.go:1350-1359`, `org_channels.go:104` | Encrypt via the keystore with a resumable backfill job (after Phase 0a's loader). Local+DB mode needs R6 first |
| R2 | Medium | `releaseAdvisoryLock` ignores an unlock failure and returns the connection to the pool. The leaked lock stalls that job on every replica until the connection is recycled. | `database/postgres_lock.go:40-45` | Copy `releaseMigrationLock` (`postgres_migrations.go:406-413`): `conn.Raw(… driver.ErrBadConn)` |
| R3 | Low | `DeleteLibraryFlow`, and Library create/update, skip `notifyCloudFlowMutation`, so the dashboard is up to 30 s stale. | `service/library.go:441-513` | Call the hook now; it becomes an event in Phase 2b |
| R4 | Low | The org-invite email (up to 30 s of SMTP) runs in a post-commit hook before the buffered response is sent. | `api/org_members.go:202-206`, `middleware/compress.go:211` | Phase 0b async runner |
| R5 | Low (rare) | A failed commit after the handler wrote 2xx still sends 2xx. Triggers: the connection dies at COMMIT, or a swallowed statement error. | `api/rls_middleware.go:203-226` | Phase 0b commit-on-first-output |
| R6 | Low | In `PAD_MODE=local` + `PAD_STORAGE=database` + auth off + no `PAD_ENCRYPTION_KEY`, the DB keystore is keyed by a **per-boot random** secret, so stored keys are unreadable after a restart. | `main.go:381-389,617-624` | Resolve secrets at config load, and keep the other uses (`ProvideSecurityConfig.Token`, `writeSessionSecret` `main.go:680`) |
| R7 | Low, latent | A read-scoped PAT gets a WS ticket without scopes and can relay `block.*` frames as an editor. The frames are not persisted, and the shipped frontend ignores them. | `auth/apitoken.go:96-98`, `auth/jwt.go:188-193`, `websocket/client.go:504-519` | Carry scopes in the ticket before any live-editing feature ships (Phase 4c) |
| R8 | Medium, design | The CI webhook uses one deployment-wide HMAC secret and loads **any tenant's** flow. | `api/handlers_integrations.go:173-185` | Phase 4c per-org credentials; never hand the global secret to tenants |

---

## Findings: extensibility blockers (verified)

| # | Blocker | Evidence |
|---|---|---|
| 1 | A new handler means editing central wiring | `api.Handlers` (`internal/api/router.go:33`), `di.ProvideHandlers` with 12 params (`internal/di/api.go:90`), the `routes_chi.go:89-124` list, typed couplings (`router.go:114-119,150-155`) |
| 2 | About 15 hand-kept path tables and predicates decide policy | `publicRoutes` (`router.go:452`); `middleware_chain.go:108-236` (`rateLimitGroup`, `authRateLimitPaths`, `expensiveGetPaths`, `azureOutboundRoute`, `isOrgProviderTestPath`); `rls_middleware.go:15-149` (34 exact paths, 2 prefixes, 5 predicates); `middleware/timeout.go:23-35`; `middleware/metrics.go` `normalizeRoute`; `auth.RequiredScope` (`apitoken.go:85-105`). Group rate limits run **outside chi** (`middleware_chain.go:56-69`; the doc comment there is stale) |
| 3 | Authorization lives in handlers in about six styles | `RequireRole` ×8 inside `handlers_admin.go`; org helpers; 27 flow checks (21 read the flow ID from the body, 3 from the query, 3 from the path, `handlers_sharing.go:38,75,159`) |
| 4 | Handlers bypass services | 85 `backend.X` calls in 20 api files (plus about 23 through other store handles). Triage, comments, share tokens, policies, channels and alerts have no service |
| 5 | Storage interface | `StorageBackend` = 21 role interfaces, 115 methods; Postgres is the only live implementation; FakeBackend (1,191 lines) is not contract-tested; about 24 runtime capability assertions mix domain and Postgres-only capabilities (`database/capabilities.go:19-41`) |
| 6 | Hard-wired side effects | 4 hook setters (`di/services.go:325,360,385,427`); account-deletion fan-out in a handler (`api/auth_account.go:52-79`); cache invalidation runs **before** commit (`flow.go:534,1393`) |
| 7 | Audit is HTTP-bound and package-global | `logAudit(r, …)` (`api/audit.go:482`) at 82 sites; pool and spill are globals (`audit.go:127-180`); no `org_id`; AccessLog `user_id` is always empty |
| 8 | Implicit desktop handling | About 91 nil-backend checks and about 56–61 handler-level "cloud required" 503s |
| 9 | Data lifecycle uses central lists | Export struct (`storage.go:75-90`), retention (`postgres_retention.go:24,121,490`), migrator (`migration/migrator.go:152-380`) |
| 10 | Provider identity is spread over about 10 lists | 41 branch lines in 11 files; the frontend's closed `authType` enum (`schemas.lazy.ts:361`) and `ProviderID` union (`frontend/src/types/providers.ts:3`) |
| 11 | AI plumbing | 4 SSE parsers and 4 tool converters; hard-coded tools (`ai/tools.go:178`); a fix-only approval flow (`ai.FixProposal`, `FixProposalCard.tsx`); budget checked only at `chat.go:147` and `rag/service.go:121`; usage writes are bare goroutines, dropped when saturated and **not drained** at shutdown (`ai/audited.go:384-428`); RAG chunks lack embedding identity (`rag/service.go:286`) |
| 12 | Analyzer | Rule metadata in side tables (`engine.go:26,51,101`, `models/settings.go:250`, `incremental.go:15`, `fix_dispatch.go`). Rules run per block only (`engine.go:210-217`; flow-level rules fake it, `rule_circular_subflow.go:31-35`); all facts are built every run. Baselines store only keys (`storage.go:1124-1129`), so **a new rule raises drift/regression alerts for every baselined flow**. Custom-rule JSON is loosely decoded (`handlers_orgrules.go:86-104`) |
| 13 | Exporters | Separate switches in the CLI, API, `ExportService` and the CI webhook's own SARIF writer; the frontend renders CSV/SARIF/JUnit itself |
| 14 | Integrations | Channel kinds listed in 3 places (`notify/notify.go:53-62,106-115`); webhook signature has no timestamp or delivery ID; one method per email (`mail/service.go:93-175`) |
| 15 | Scale-out (pinned to 1 replica, `infra/main.bicep:18-26`, `deploy.yml:292`) | In-process SSE hub (`api/events.go`; `Emit` reaches **all** tenants' clients); chat begin/cancel/fix-decision; AI budget ledger; conversation read-modify-write mutex; undo ring; `uploadIDs` (`flow.go:40,477-488`); scanner alert dedup; KB lock; migration status; audit spill on `/tmp`; unversioned Redis payloads, with presence **deleted on decode failure** (`websocket/backplane.go:232-243`); readiness fails on a Redis blip (`handlers_system.go:390-394`); shutdown kills streams (`main.go:747`) |
| 16 | Deploy and migrations | Migrations run inside `storagedb.New` **before the listener binds**, so liveness can kill a long step mid-transaction; `lock_timeout=0` lets DDL queue behind the serving revision (`postgres_migrations.go:380-386`); the boot guard plus auto-rollback cause R1; checksums hash exact bytes (CRLF checkout, no `.gitattributes`) |
| 17 | Lifecycle | No job abstraction; bare goroutines; `DrainBackground` drains only `AnalysisHandler`; the scanner and retention sweep **fail open** on lock errors (`scanner.go:183-187`, `main.go:342-347`); `Bounded` logs panics at WARN, below the error sink and alert (`lifecycle/bounded.go:67`) |
| 18 | `main.go` and config | About 700 of 862 lines are providers; duration strings; stray `os.Getenv`; config mutated in DI (R6) |
| 19 | Observability | App logs lack `request_id`/`trace_id`; nothing scrapes the about 46 metrics (`main.bicep:705-707`); no instance ID; no full-chain benchmark, and nightly benchmarks are never compared |
| 20 | Tests and docs | API tests run on the **filesystem** backend through a fourth hand-wired DI copy (`api/helpers_test.go:155-240`, 211 router-helper uses); a destructive `MigrateDown` runs on the shared CI DB (`migration_v27_test.go:241`); 130 `time.Sleep` calls; e2e never reaches a Go server; no architecture docs, ADRs or CLAUDE.md |

---

## Target principles

1. **A feature is a module.** It owns its service, `api.Module`, role interface, migrations, data-lifecycle handler, event subscriptions and metrics, all contributed through fx groups.
2. **Policy is declared where the thing is defined.** Derived lists are computed.
3. **Middleware order stays the same.** The route is resolved once at the outer layer, its policy travels in `RequestInfo`, and a guard asserts that chi agrees.
4. **Layering and conventions are linted** (golangci `depguard`/`forbidigo`), and the rules are written down in `docs/architecture`.
5. **Explicit delivery semantics:**
   - post-commit hooks have three states
   - a sync invalidation tier with generation-checked fills
   - a drained async I/O tier
   - hooks run with **user-scoped** RLS; the system scope is explicit
6. **Every schema change is expand/contract.** Every Redis payload is versioned and additive.
7. **Each Redis-backed mechanism declares its failure mode** (fail closed or fail open with a TTL). Every piece of per-process state is either shared or declared replica-local.

---

## Phases

### Phase 0a: Safety nets and deploy safety (about 3–4 weeks)

**Migration discipline:**
- Each migration declares `compat: expand|breaking`. v1–v31 are classified conservatively; index-only ones such as v30 and v31 are `expand`.
- The boot guard refuses only an applied **breaking** step newer than the binary.
- Contract steps ship one release after the code stops using the old shape.
- CI job: the previous release image boots against the new schema.

**Migration loader for v32+:**
- Embedded `.sql` in one directory.
- Strip the BOM and `\r` before hashing, plus `.gitattributes` `*.sql text eol=lf`.
- Checksum golden for v1–v31.
- CI check that versions are unique.
- Each step uses `SET LOCAL lock_timeout` with retry; the advisory-lock wait stays unbounded.
- `CONCURRENTLY` indexes are idempotent ensure-steps outside the version table, checking `indisvalid` (the `postgres_vector_index.go:45-104` pattern).

**Boot ordering:**
- Bind the listener and serve `/healthz` before migrating.
- `/readyz` returns 503 until migrations finish.
- Data backfills run as resumable singleton jobs after boot.

**Postgres test harness:**
- `testutil.PG(t)`: a per-package database cloned from a migrated template, which also removes the shared-DB `MigrateDown` hazard.
- Test routers built from the **real fx modules**, with `fx.Replace` for fakes.
- `fx.ValidateApp` per mode (desktop / local+DB / cloud).

**Differential route-policy test and authz matrix** (on the Postgres harness, cloud + JWT config):
- Old classifiers run over concrete adversarial requests for every route: `/api/v1`, `/api/v1/v1/x`, `%2F`, dot segments, a trailing slash, the 405 case (e.g. `GET /api/auth/login`), HEAD/OPTIONS, `/x%2F..%2Fshared?token=`, `/assets/x`, unknown `/api/*`, `/ws`, `/swagger`, `/metrics`.
- The authz matrix covers anonymous / viewer / member / admin / scoped PAT → status.

**Contract suite on FakeBackend:** a `t.Run` skip-list wrapper plus a count golden and a strict mode. Stubs are declared unsupported.

**Lint ratchets:**
- `api` must not import `storage/database` (allowlist the 4 current files).
- Services must not import `api`.
- `BeginTx` (7 sites) and self-managed `BeginRLS(` (5 sites) are allowlisted.
- Handler `backend.X` count ratchets down from 85.
- `os.Getenv` only in config.

**Performance baseline:**
- `BenchmarkBuildHandler` through the full chain: library GET, flow save, dashboard, SSE fan-out.
- benchstat comparison in `bench-fuzz.yml`.
- Keep the `perf_statements_test.go` statement counts as the "no extra DB round trips" gate for later phases.

**Conventions:**
- `docs/architecture/` (module recipe, route policy, migrations, modes, metrics, events), `docs/adr/`, root `CLAUDE.md`.
- `capability` package plus the desktop / local+DB / cloud matrix.

**Fix-first items:** R1 (immediate rollback guard), S1, S2, R2, R3, R6.

### Phase 0b: Transaction and async foundations (about 2–3 weeks)

**`internal/platform/txhook`** owns the post-commit registry and the RLS context key:
- States are **pending → committed | aborted**.
- Committed: late registrations run inline.
- Aborted: late registrations are dropped and logged, so a blob purge never follows a rollback.
- `hasPostCommit` is false when aborted (`postgres_flows.go:163,1124` depend on it).

**`withTx(ctx, func(txCtx context.Context, tx *sql.Tx) error)`** (5 call sites):
- The fallback path puts a registry and `WithRLSTx` on `txCtx` and flushes after its own commit.
- The joined path passes `ctx` through unchanged.
- Fold in the 3 hand-rolled copies (`postgres_flows.go:87,1000`, `postgres_azure_registry.go:468`).

**Hook context:**
- `WithoutCancel` plus a **fresh lazy RLS scope for the same user**. Stripping the scope would fall back to RLS allow-all.
- The system scope is opt-in through `txhook.AsSystem(ctx, reason)`.

**Commit on first output (R5):**
- Commit at the first final `WriteHeader` (1xx ignored), the first `Write`, or `Flush`, or at handler return if nothing was written.
- On failure: restore the header snapshot taken at entry (keeps security/CORS/request-ID headers, drops handler-set cookies and ETag), write 503, fail later writes, abort the registry.
- Hooks run after the handler returns.
- Documented: a panic after the first byte leaves the write committed.

**Bounded async runner** (`lifecycle.Bounded` plus fx-stop drain):
- Panics go through `logger.RecoverPanic` at ERROR.
- Metrics: in-flight, dropped, duration.
- It replaces `runDetached` and the AI usage-metric goroutines, so they are drained at shutdown.

**Fix-first items:** R4, S3 (needs the loader).

### Phase 1a: Module seam and declarative route policy (about 4 weeks)

**`RequestInfo`:**
- Installed at the outermost layer: ID, client IP plus the raw peer IP (MetricsGuard and `RateLimitAddr` unchanged), UA, Policy.
- `Actor` is write-once (`atomic.Pointer`) and filled by `jwtAuth`, login, SSO and WS. `auth.WithClaims` is kept; the struct is never pooled.
- `logger.FromContext`; AccessLog `user_id` gets fixed.

**`api.Module`:**
- `Mount(rb)` via fx `group:"routes"`.
- The builder owns the shared prefix groups and sorts registrations.
- Delete the `Handlers` struct, `ProvideHandlers` and the `routes_chi.go` list; replace the typed couplings with small interfaces.

**Route resolution:**
- Merge `inboundRoutePath` and `dispatchPath` (`timeout.go:45-67`) into one function.
- Resolve once inside the outer Recovery, before RequestTimeout, with `mux.Find` on a **fresh** chi context that never enters the request.
- Look up policy by (method, canonical pattern):
  - A mount stub or miss means no-route.
  - A **405 policy** probes the other methods and inherits Public/RateLimitGroup.
  - The no-route policy is computed from `canonicalRequestPath(r.URL.Path)`, which is what NotFound serves.
- The outer layer never mutates `r.URL`.

**Guard:**
- Every endpoint, NotFound and MethodNotAllowed is wrapped. If chi's match disagrees with the pre-resolved policy pointer: log, metric, 500 before the handler.
- Boot self-test: each route's synthesized path resolves to itself.
- `Router.ServeHTTP` installs the policy if missing (211 test call sites).

**Rollout via `PAD_ROUTE_POLICY=legacy|shadow|declarative`:**
- Shadow mode: legacy decides, and mismatches are counted per route.
- Flip with a config-only revision.
- Delete the legacy tables one release later.
- Initial declarations are generated from the legacy classifiers.

**Metrics label = matched pattern.** Delete `staticRoutePatterns`/`normalizeRoute`; update `metrics_test.go` and dashboards (`:id` becomes `{id}`).

**Deny by default** at startup, including `/ws`, `/swagger`, `/metrics`, `/shared` and `/api/integrations/*`.

**Authorization:**
- Group middleware handles roles, org roles and the 3 path-parameter flow routes.
- The other 24 flow checks go through the typed request's `FlowRef()` or the service.
- Everything goes through `AuthzService.Can(ctx, actor, action, resource)`, which also checks **actor scopes** (closes the S2 class).

**Mode gating:** `route.Modes(...)` replaces the about 56–61 handler 503s, with the same error code.

### Phase 1b: Typed endpoints and API conventions (about 2 weeks)

- **`api.JSON[Req,Resp]`** with `Validate()` and `apperr.Error{Kind, Code, Message, Details}`, plus the `Page[T]` envelope and filter/sort conventions. Swag gets real types.
- **`Idempotency-Key`** for create endpoints (table via the 0a loader).
- **Frontend:**
  - Tolerant zod parsing for unknown enum values ships **one release before** any new value.
  - Build-version header and a reload banner for stale tabs.
- New endpoints must use the adapter; old ones migrate when touched.

### Phase 2a: Domain services and audit (about 4 weeks)

- **New services:** `TriageService`, `CommentService`, `ShareTokenService`, `PolicyService`, `ChannelService`, `GovernanceAlertService`, `AccountLifecycleService`.
  - The account service orchestrates prepare → delete → finalize, with behaviour unchanged.
  - The 85 handler calls (plus about 23 via other store handles) move into services, and the ratchet goes to 0.
  - Services take role interfaces.
- **`internal/audit`** owns the pool and spill moved out of `api`. `audit.Record(ctx, …)` reads `RequestInfo`/`Actor`.
  - Jobs record as `System(name)`.
  - `org_id` column plus a resumable backfill.
  - Login-failure audit stays a direct call.
- **Break real DI cycles** by extracting the budget checker, scope resolver and Azure model access. Document the remaining setters.

### Phase 2b: Domain events (about 2–3 weeks)

- **Bus** with no dependencies; subscriptions are made in `fx.Invoke`.
- **Typed snapshot events:** `FlowChanged{FlowID, StableID, OwnerID, OrgID, Kind}`, `CredentialChanged`, `OrgMemberChanged`, `SettingsChanged`, `KnowledgeDocChanged`, `FindingStatusChanged`.
- **Sync invalidation tier:**
  - Runs immediately, and again on commit **and** rollback.
  - Cache fills check a generation captured before the read and skip filling while a transaction is open. Without this, a stale entry survives until the TTL.
  - Redis fan-out (Phase 5b) is published only from the post-commit pass.
- **Async tier** (email, notify, SSE/WS, lent-credential disabling) runs on the 0b runner with timeout and recover per subscriber.
- **Replaces** the 4 hook setters.
- **SSE audience is mandatory** (`EmitTo`/`EmitToOrg`); broadcast needs an explicit `AudienceAll`.
- **Metrics per event:** published, failed, duration.
- Test-helper event recorder.
- Delivery is at most once; a durable outbox is deferred.

### Phase 3: Storage and data lifecycle (about 4–5 weeks)

- **Order:**
  1. Bring the fake to parity on its implemented methods (strict contract mode).
  2. Move the **55 filesystem-backed test files** onto the Postgres harness or the fake.
  3. Narrow the filesystem backend to a read-only `LegacySource`, with a read-only contract subset.
- **Capabilities:**
  - Domain capabilities (`FindingStatusGetter`, `APITokenOwnerLookup`, …) are folded into role interfaces.
  - Postgres-only capabilities (`SQLProvider`, `RLSBeginner`, `KeyStoreProvider`, `GlobalLocker`) become optional fx values. That removes `rls_middleware.go`'s import exception.
- **Per-domain files** for interfaces, Postgres and contract; `Tx(ctx, fn)`; an SQL args builder.
- **Scope the unscoped methods** (`storage.go:244,246,443`, `DeleteFindingStatus`).
- **Data-lifecycle registry** `group:"data_lifecycle"` (export, delete, purge, legacy-migrate), plus an `information_schema` coverage test. It replaces the central export struct, retention lists and migrator lists.
- **Settings** namespaced per module with a version check, plus one release of dual-read.

### Phase 4a: Analyzer and export registries (about 3 weeks)

- **`RuleMeta` on rules:** confidence, auto-fix, direct fix, defaults, related, **`IntroducedIn`**, **`Stage(preview|stable)`**. Add `RegisterFix` and a completeness test.
- **Baselines record a rule-set digest.**
  - Findings from rules newer than the baseline produce one "new rules" notice per org, instead of drift/regression alerts.
  - Preview rules never fail CI or policy gates.
- **Rule types:** `FlowRule` (whole flow) and `LibraryRule` (cross-flow), with facts computed lazily.
- **Custom-rule JSON** gets `kind` + `schemaVersion` and strict decoding.
- **Exporter registry** `{ID, MIME, Ext, Render, ServerRendered}`, used by the CLI, API, `ExportService` and the CI webhook. `bakicli` reuses `parser.ParseFolder`.
- **`cache.New(name, keyParts…)`:** tenant and rule-profile parts are required; the analyzer cache is injected.

### Phase 4b: AI providers, tools, approvals (about 4–5 weeks; start when a new provider is scheduled)

- **`ai.ProviderSpec` registry:** ID, auth type (wire enum unchanged), Caps, Modes, EgressPolicy, CredentialSchema, resolver, constructor.
  - It derives every provider list.
  - The 41 branch lines become caps or hooks.
  - Copilot and github-models special cases become spec fields.
  - Pricing comes only from the catalog.
- **Budget and metering:**
  - A pre-call **budget hook in the audited provider wrapper**, so it covers every caller.
  - Org provider allowlist enforced at factory selection.
  - Usage writes go through the drained runner.
- **Tools and approvals:**
  - Tool registry: each tool declares `RequiresApproval` and `RequiredScope`, checked against actor scopes.
  - Generic `ApprovalRequest{tool, previewKind, items, requiredScope}`, with one decision endpoint and one frontend card.
- **RAG:** chunks store the embedding identity (`ai/embedding_identity.go:26-29`) and a chunker version.
- **Frontend:** generic credential form, `ProviderID` as a string with a known set.
- **Deferred:** wire-codec rewrite, factory-scoped memo maps, generic connection registry.

### Phase 4c: Integrations and notifications (about 2 weeks)

- **Channel-kind registry** in one place, with:
  - keystore-backed secrets (S3 data already migrated)
  - egress policy per kind
  - a versioned webhook envelope with timestamp and delivery ID
- **Mail template registry.**
- **CI webhook:** per-org credentials (R8) and output through the exporter registry.
- **WS tickets carry scopes,** with a per-message check (R7).

### Phase 5a: Platform, jobs, observability (about 2 weeks)

- **Split `main.go` into fx modules.** Keep the Tauri `CONFIG:` handshake (`main.go:687-692`), the ldflags symbols (`buildstamp_test.go`) and the package-main tests.
- **`lifecycle.Singleton`:**
  - Fails **closed** on lock errors, but runs when no locker exists (desktop).
  - Metric and alert after consecutive skips (retention is a GDPR duty).
  - Heartbeat on the pinned connection; cancel on a lost lease.
  - Lock keys derived from the job name, with a uniqueness test.
  - Scanner alert dedup persisted.
- **Config:** immutable (completes R6), parsed durations.
- **Pool size per replica** = server cap / `maxReplicas`. Document that PgBouncer transaction mode breaks session locks.
- **Observability:**
  - Modules register their own metrics.
  - Prometheus scrape (managed) or KQL alerts.
  - `service.instance.id`.
  - A `secret.Value` type that redacts itself in strings, JSON and logs.
- **Graceful shutdown:** SIGTERM → `/readyz` 503, refuse new chat streams, and close open streams with `SERVER_RESTARTING` so the client resumes.

### Phase 5b: Realtime backplane and multi-replica (about 5 weeks; start only when more than one replica is needed)

- **Over Redis:**
  - SSE fan-out with event IDs for Last-Event-ID
  - subscriber presence
  - stream command channel (cancel, fix decisions)
  - budget ledger, which **fails closed** when Redis is down
- **Conversations:** an optimistic version column.
- **Other per-process state:**
  - undo ring, `uploadIDs` → shared
  - KB lock → advisory lock
  - migration status → DB
  - audit spill → per-instance file
- **Cache invalidation fan-out** with a TTL/generation safety net.
- **Redis payload envelope `{v, kind}`:** additive only; never delete on decode failure; previous-release fixture tests; frozen rate-limit group names.
- **Failure-mode table per mechanism.** `/readyz` reports Redis as *degraded*, not unready.
- **Registry test:** every service-level cache or map declares replica-local or shared.
- **Two-replica tests** in-process on miniredis and in compose. Then raise `maxReplicas`.

### Deliberately cut or deferred

- WS dispatch registry
- SSE/WS unification
- Durable outbox
- Retiring FakeBackend
- Moving v1–v31 to `.sql`
- AI wire-codec rewrite
- Typed domain events over Redis (invalidation keys only)
- Core DTO purity moves
- Expanding `/api/system/features`
- Per-org SSO
- Real API versioning (alias plus a deprecation policy is enough)

---

## Adding a feature afterwards (e.g. "flow approvals")

1. `storage/interfaces/approvals.go`, `database/postgres_approvals.go`, `contract/approvals.go`, and a fake method that passes the contract.
2. `migrations/0032_approvals.up.sql`, marked `compat: expand`, plus `.down.sql`.
3. `service/approvals.go`: `authz.Can`, `audit.Record`, `events.Publish`, metrics.
4. `api/approvals.go`: `Mount(rb)` with `route.Auth`, `Scope`, `RateLimit`, `RLS`, `Modes(Cloud)` and typed adapters.
5. A data-lifecycle handler.
6. fx registrations in the feature module.
7. Frontend types and client, with tolerant enums.

No edits to router lists, policy tables, DI param structs, cache wiring, audit plumbing, retention lists, the migrator or the metrics list.

- **A new provider:** spec file + catalog + frontend schema check.
- **A new rule:** one file with `IntroducedIn`/`Stage`.
- **A new exporter or channel kind:** one file.

## Critical files

- **HTTP:** `internal/api/{router,routes_chi,middleware_chain,rls_middleware,security,handler_helpers,org_access,org_channels,auth,auth_account,audit,audit_spill,events,handlers_chat,handlers_integrations}.go`, `internal/api/middleware/{timeout,metrics,access_log,compress,ratelimit,ip}.go`, `internal/auth/{apitoken,jwt}.go`, `internal/di/{api,services}.go`, `main.go`
- **Storage:** `internal/storage/database/{rls,postgres_lock,postgres_migrations,postgres_vector_index,postgres_flows,postgres_retention,postgres_settings,postgres_orgchannels,capabilities}.go`, `storage/interfaces/storage.go`, `filesystem/local_storage.go`, `testutil/fakebackend.go`, `storage/contract/contract.go`, `internal/migration/migrator.go`, `internal/api/helpers_test.go`
- **Domain:** `internal/service/{authz,flow,library,chat,chat_fixes,chat_resume,chat_conversation,dashboard}.go`, `internal/scanner/scanner.go`, `internal/lifecycle/*`, `internal/websocket/{client,hub,backplane}.go`
- **AI and core:** `internal/ai/{factory,registry,catalog,tools,audited,embedding_identity}.go`, `internal/rag/service.go`, `core/analyzer/{engine,registry,fix_dispatch,incremental,cache,custom_rules}.go`, `core/export/*`, `cmd/bakicli/main.go`
- **Ops:** `.github/workflows/{ci,deploy,bench-fuzz,desktop}.yml`, `infra/main.bicep`, `Dockerfile`; frontend `src/api/schemas.lazy.ts`, `src/types/providers.ts`, `FixProposalCard.tsx`

## Reuse, don't rebuild

- `TryGlobalLock` and `api.GlobalLocker`
- `releaseMigrationLock`'s bad-connection discard
- `EnsureVectorIndex`
- `WithPostCommit`/`hasPostCommit` (moved into `txhook`)
- `lifecycle.TickerLoop` and `lifecycle.Bounded`
- `core/cache.SizedLRU`
- `websocket/backplane.go` and `redisx`
- The `cached_blacklist.go` periodic reload as the lossy-pub/sub safety net
- The Redis rate limiter
- `chat_resume.go`
- `storage/contract.RunSuite`
- `route_path_test.go`
- `perf_statements_test.go`
- miniredis (already used in 4 tests)
- `ai/embedding_identity.go`
- The `registerRule` pattern
- Go 1.25 `testing/synctest` for the timing tests

## Verification

- **Every phase:**
  - `go build ./...`, `go vet`, golangci (`depguard`/`forbidigo`) and `go test ./...` in both modules on the Postgres harness. CI runs on Ubuntu with `-race`; add `timeout-minutes`.
  - gofmt on LF copies (Windows).
  - Swag diff is intentional only.
  - `buildstamp_test.go` and the package-main tests pass.
  - The **desktop boot smoke test** passes (sidecar `CONFIG:` handshake; add it to `desktop.yml`).
  - benchstat shows no regression over 5% on the `BuildHandler` benchmarks, and the statement-count tests are unchanged.
- **Phase 0a:**
  - The previous-release-boots-on-new-schema job passes.
  - The checksum golden passes; CRLF/BOM `.sql` files hash like LF.
  - A long migration is not killed by liveness.
  - The R1 rollback guard is tested in a dry-run workflow.
  - Tests for S1 (member gets a redacted list), S2 (chat-only PAT → 403 on fix-decision) and R2 (unlock failure discards the connection).
- **Phase 0b:**
  - An aborted registry drops late hooks.
  - A hook DB access runs under user RLS.
  - A commit failure produces 503 with restored headers.
  - The runner drains at shutdown.
  - The usage metric survives SIGTERM.
- **Phase 1a:**
  - The differential golden is unchanged.
  - The authz matrix is unchanged.
  - Shadow mode reports zero mismatches over a soak period before the flip.
  - The guard self-test and deny-by-default check pass.
- **Phase 2:**
  - Library and Flow mutations invalidate the same caches.
  - The generation check blocks stale fills.
  - Audit rows keep IP/UA and add actor and org.
- **Phase 3:** contract strict mode is green; the data-lifecycle coverage test passes; the existing DB (live v29 per notes, with v30–v31 pending) boots cleanly.
- **Phase 4:**
  - Registry completeness (providers, rules, exporters, tools).
  - A new preview rule raises no drift alerts and doesn't gate.
  - The frontend handles an unknown provider and auth type.
- **Phase 5:** two replicas on miniredis and in compose:
  - an SSE stream crosses replicas, and cancel works from the other replica
  - the budget cap holds
  - jobs run once with no duplicate alerts after failover
  - mixed-version Redis payloads decode
  - Redis down → budget fails closed and the rest degrades
- **Docker smoke test:** login, import and analyze, chat with a tool call, Azure provider list, admin users page, CI webhook.
- Leave every change uncommitted for the user.

## Order

1. **R1, S1, S2, R2** (days)
2. **0a → 0b → 1a → 1b → 2a → 2b** (Tier 1, about 5 months)
3. **3 → 4a → 4c → 5a** (Tier 2)
4. **4b and 5b** when their trigger happens. Phase 5b is the only gate for raising `maxReplicas`.
