# AI roadmap: Documentation drift

Items of category `docs` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-docs-1

**No AI incident runbook: cost attribution, provider outage, stuck streams, dropped usage and budget refusals have no documented procedure** · tier 3

**Problem.** `docs/architecture/operations.md` limits its scope to connections, pooling, the stop budget, replicas and alerts (line 3). AI comes up only in passing (lines 108, 126, 132), with no procedure attached. The only operational content in `docs/architecture/ai-providers.md` is the ledger addendum (16-24). Nothing tells an operator:
- how to answer "who spent the budget today". The budget's "today" is `created_at >= date_trunc('day', now())` in the Postgres session's time zone, which is visible only in `internal/storage/database/postgres_users.go:1024-1034`.
- what to do when a circuit breaker opens. The breaker half-opens on its own after 30 s (`internal/ai/circuitbreaker.go:25`, `153-156`). `resetBreakerRegistry` is unexported and test-only (`145-151`), and the exported API is just the two constructors (`177-194`), so the only way to reset a breaker early is a restart.
- what `pad_usage_dropped_total` means for the budget.
- which of the `pad_ai_*` (`internal/metrics/testdata/metrics.golden:11-18`) and `pad_chat_*` (`41-50`) series to read for a stuck or runaway stream.

**Evidence.**
- `docs/architecture/operations.md:3` — scope statement lists DB, pooling, stop budget, replicas, alerts; no AI section
- `docs/architecture/operations.md:108` — Redis-down behaviour of the in-flight ledger
- `docs/architecture/operations.md:126` — provider circuit breakers listed as per replica, in passing
- `docs/architecture/operations.md:132` — alerts name `pad_ai_ledger_errors_total` and `pad_usage_dropped_total{reason}` with no procedure attached
- `docs/architecture/ai-providers.md:16` — the ledger addendum (16-24) is the only operational content
- `internal/storage/database/postgres_users.go:1024` — `GetDailyUsage`: `created_at >= date_trunc('day', now())` (1024-1026 org, 1032-1034 user)
- `internal/ai/circuitbreaker.go:24` — opens after 5 consecutive retryable failures, one probe after 30 s (24-25); 429s excluded (327-328); WARN line `AI circuit breaker transition` (350)
- `internal/ai/circuitbreaker.go:147` — `resetBreakerRegistry` is test-only and unexported (145-151); the exported API is `NewCircuitBreakerProvider` / `WithConfig` (177-194)
- `internal/metrics/testdata/metrics.golden:1` — `ai_circuit_breaker_transitions_total`, `ai_request_duration_seconds`, `ai_request_errors_total` (1-3) sit outside `pad_ai_*`
- `internal/storage/database/postgres_analytics.go:259` — the Analytics queries bucket days in the caller's time zone (259-268)

**Change.** Tier 3. Add an "AI assistant" section to `operations.md` with five parts:
1. **Cost attribution.** Point first at the Analytics page (`LoadUsageAnalytics`, `postgres_analytics.go:253`). Add raw SQL over `usage_metrics` only for per-user breakdowns inside an org, using the same `date_trunc('day', now())` boundary as `GetDailyUsage`. Note that the Analytics page uses the caller's time zone (`postgres_analytics.go:259-268`), while the budget uses the database session's day (`postgres_users.go:1024-1034`).
2. **Provider outage.**
   - Signals: `pad_ai_retry_attempts_total`, `ai_circuit_breaker_transitions_total`, `ai_request_errors_total` and `ai_request_duration_seconds`, plus the breaker WARN line.
   - Breaker behaviour: it opens after 5 consecutive retryable failures, with 429s excluded. It allows one probe after 30 s and heals on its own. It is keyed per provider and model, and kept per replica.
   - Action: normally wait or fix the upstream; restart only as a last resort.
3. **Stuck or runaway streams.** Which `pad_chat_*` series and log lines to read, and how to end a stream.
4. **Dropped usage.** What `pad_usage_dropped_total{reason}` means, and what an under-counted budget means.
5. **Budget refusals.** What `pad_ai_budget_refusals_total{reason}` means, with a cross-link to the ledger addendum.

Link the section from `ai-providers.md` and `README.md`.

**Touches.** `docs/architecture/operations.md`, `docs/architecture/ai-providers.md`, `docs/architecture/README.md`

**Done when.**
- `operations.md` has an "AI assistant" section covering cost attribution, provider outage, stuck streams, dropped usage and budget refusals, each naming the metric, the log line and the action.
- The SQL recipe runs against the compose database, and its day boundary matches `GetDailyUsage`.
- A doc test over `internal/metrics/testdata/metrics.golden` checks that every `pad_ai_*` and `pad_chat_*` metric is named at least once in `operations.md` or `ai-providers.md`.

### R-docs-3

**docs/ai-foundry.md and the in-code Foundry messages disagree on the role name, the Claude host rewrite, Mythos gating, the reasoning-floor list and the embedding-width rule** · tier 3

**Problem.** `docs/ai-foundry.md` names the required roles as Foundry User (line 7) and Foundry Agent Consumer (8). The role names in the code do not line up with this:
- The chat error copy for a missing role says "Foundry User" on the resource (`internal/service/chat_errors.go:269`).
- The project probe's 401 copy suggests "Azure AI User" on the project (`internal/ai/azure_probe.go:74`), which is a different scope.

Four other statements in the doc drift from the code:
- **Claude host.** The doc says Claude calls always go to `<resource>.services.ai.azure.com` (17). `claudeOnFoundryBase` rewrites only single-label `*.openai.azure.com` and `*.cognitiveservices.azure.com` hosts and keeps every other host (`internal/ai/azure_claude.go:53-67`).
- **Mythos.** The doc's "the Mythos tier is Entra-only (no API key)" (17) states a platform rule. The code does not enforce it at validation or save: `azure_registry.go` has no Mythos check. It only remaps an API-key 401/403 on a Mythos-named deployment into a clear error after the fact (`claudeMythosEntraOnly`, `azure_claude.go:150-166`).
- **Reasoning floor.** The doc's list (13) omits gpt-chat-latest and codex-mini (`internal/ai/reasoning_floor.go:19`), the gpt-5-chat exclusion (`reasoning_floor.go:30`) and Opus 4.7+ (`internal/ai/claude.go:393`).
- **Embedding width.** The doc's rule "never above the model's native width" (14) holds only for text-embedding-3 deployment names (`internal/ai/azure_foundry.go:487-496`). An ada name gets no `dimensions`, and any other name gets the configured width.

**Evidence.**
- `docs/ai-foundry.md:7` — "needs Foundry User on the resource"; line 8 names Foundry Agent Consumer
- `internal/ai/azure_probe.go:74` — project-scope 401 copy: "give it a role on the project, such as Azure AI User"
- `internal/service/chat_errors.go:269` — "assign it the Foundry User role on the Azure AI Foundry resource"
- `docs/ai-foundry.md:17` — "Claude calls always go to `<resource>.services.ai.azure.com`" and "the Mythos tier is Entra-only (no API key)"
- `internal/ai/azure_claude.go:53` — `claudeOnFoundryBase` rewrites only the two suffixes on single-label hosts (53-67); `claudeMythosEntraOnly` remaps any API-key 401/403 after the fact (150-166); `ValidateAzureRegisteredModel` (`internal/ai/azure_registry.go:203`) has no Mythos check
- `docs/ai-foundry.md:13` — reasoning list compared with `reasoning_floor.go:19` (gpt-chat-latest, codex-mini), `:30` (gpt-5-chat exclusion) and `claude.go:393` (Opus 4.7+); line 14 width rule compared with `azure_foundry.go:487-496` (text-embedding-3 names only)

**Change.**
- **Doc fixes.** Correct three sentences in `ai-foundry.md`:
  - The Claude host rewrite applies only to single-label `*.openai.azure.com` and `*.cognitiveservices.azure.com` hosts; other hosts are kept.
  - The reasoning-floor list adds gpt-chat-latest, codex-mini and Opus 4.7+, and names the gpt-5-chat exclusion.
  - The embedding-width sentence matches `azureEmbeddingDimensions`.
- **Mythos.** Keep the sentence as a statement of the platform rule. A save-time check in `ValidateAzureRegisteredModel` is optional hardening, not doc drift.
- **Role name.** First check whether the project-scope role in the probe's 401 copy is the same as the resource-scope role named by the doc and the chat error.
  - If it is, put the portal's current name in one exported constant that both messages use and the doc quotes.
  - If it is not, the doc names each role with its scope.

**Touches.** `docs/ai-foundry.md`, `internal/ai/azure_probe.go`, `internal/service/chat_errors.go`, `internal/ai/azure_registry.go`

**Done when.**
- If the project-scope and resource-scope roles are the same, one exported constant for the role name is used by `azure_probe.go` and `chat_errors.go` and quoted in `docs/ai-foundry.md`, and a test greps both messages for it. Otherwise, `docs/ai-foundry.md` names each role with its scope.
- The sentences in `docs/ai-foundry.md` on the Claude host, the reasoning floor and the embedding width match `claudeOnFoundryBase`, `reasoningModelRe`/`claudeRemovesSampling` and `azureEmbeddingDimensions` as implemented, or the code is changed to match the doc.

### R-docs-5

**Approvals and fix-path docs drift: ADR 0026 open item, chat stream and apply-fix swagger, ADR 0014 confidence, no fixes section in analyzer.md** · tier 3

**Problem.** Four documents describe the approvals and fix paths as they used to be:
- **ADR 0026.** Its open item (line 20) says a decision on another replica or after a restart answers 404. ADR 0033 implemented cross-replica relay: `DecideApproval` loads the pending key and forwards the decision to the owning replica (`internal/service/chat_commands.go:455-479`). Only the restart half is still true.
- **Stream swagger.** The swagger for `POST /api/chat/stream` (`internal/api/handlers_chat.go:37`) lists chunk, tool, tool_result, done and error events. The stream also emits `fix_proposal` and `fix_decision` (`internal/service/chat_approvals.go:124`, `157`), and the client parses `heartbeat` (`frontend/src/lib/chatEvent.ts:27`, `210`).
- **Fix swagger.** The apply-fix swagger (`internal/api/flow_fixes.go:16-18`) says "Desktop/local only", and the preview swagger (65) says "for single-file flows". In fact `ApplyFix` has a cloud branch (`internal/service/flow.go:1181-1188`), and `PreviewFix` previews folder members (`flow.go:1551-1571`).
- **analyzer.md.** `docs/architecture/analyzer.md` mentions the fix loop only in passing (lines 18, 23, 34) and never names `registerFix`, `PatchForFix` or `AutoFixableRules`.

The ADR 0014 point in the title turned out not to be drift. ADR 0014 §1 (line 11) says Confidence is "required; no implicit Medium". `core/analyzer/meta_test.go:36-40` enforces this for built-ins. The runtime Medium default (`core/analyzer/engine.go:690-696`, `core/analyzer/meta.go:90-98`) applies only to custom and unknown rule ids.

**Evidence.**
- `docs/adr/0026-tool-specs-and-approvals.md:20` — "Open: pending approvals are process memory — a decision on another replica or after a restart answers 404 (Phase 5b)"
- `internal/service/chat_commands.go:455` — `DecideApproval` loads the pending key and relays the decision to the owner replica (455-479)
- `internal/api/handlers_chat.go:37` — stream swagger lists chunk, tool, tool_result, done and error only; `fix_proposal`/`fix_decision` are emitted at `internal/service/chat_approvals.go:124`/`157`; `heartbeat` is parsed at `frontend/src/lib/chatEvent.ts:210`
- `internal/api/flow_fixes.go:16` — "Desktop/local only" (16-18); line 65 "for single-file flows"
- `internal/service/flow.go:1181` — `ApplyFix` cloud branch (1181-1188); `PreviewFix` folder members (1551-1571)
- `docs/architecture/analyzer.md:18` — the fix loop is mentioned only at 18, 23 and 34; `registerFix` (`core/analyzer/fix_dispatch.go:61`), `PatchForFix` (`fix_dispatch.go:89`) and `AutoFixableRules` (`core/analyzer/registry.go:49`) are not named
- `docs/adr/0014-rule-metadata-and-stages.md:11` — "Confidence (required; no implicit Medium)"; built-ins are held to it by `core/analyzer/meta_test.go:36-40`, and the runtime Medium in `engine.go:690-696` and `meta.go:90-98` covers only custom and unknown ids

**Change.**
- **ADR 0026.** Update the consequences to point at ADR 0033 for cross-replica decisions and to name the remaining restart gap.
- **Stream swagger.** Add `fix_proposal`, `fix_decision` and `heartbeat` to the description, with their payload keys.
- **Fix swagger.** The apply-fix text says cloud mode is supported. The preview-fix text says folder flows preview the target member.
- **analyzer.md.** Add a "Fixes" section covering `registerFix`, `PatchForFix`, `AutoFixableRules` and the iterative loop.
- **Regenerate** `docs/swagger.*`.
- **ADR 0014.** No enforcement change. At most, add a clause saying the runtime Medium default covers only custom and unknown rules.

**Touches.** `docs/adr/0026-tool-specs-and-approvals.md`, `internal/api/handlers_chat.go`, `internal/api/flow_fixes.go`, `docs/adr/0014-rule-metadata-and-stages.md`, `docs/architecture/analyzer.md`, `docs/swagger.yaml`, `docs/swagger.json`, `docs/docs.go`

**Done when.**
- ADR 0026's consequences name ADR 0033 for cross-replica decisions and no longer claim they answer 404.
- The generated swagger for `/api/chat/stream` names `fix_proposal`, `fix_decision` and `heartbeat`, and apply-fix no longer says "Desktop/local only".
- `analyzer.md` has a Fixes section.

**ADR.** ADR-0026 open item superseded by ADR-0033 (replica half)

### R-docs-7

**Knowledge docs drift: ADR 0027 and the embedding-model settings copy describe behaviour the subsystem no longer has** · tier 3

**Problem.**
- **ADR 0027 §3 (sample guard).** Line 13 says the sample guard stays "only for rows without an identity". `checkCorpusModel` also falls through to the billed sample whenever the census shows stranded rows: only `Stranded == 0 && Legacy() == 0` skips it (`internal/rag/service.go:382-389`).
- **ADR 0027 §9 (locking).** Line 19 describes only the transaction-scoped advisory lock. The code also holds a cross-replica per-org lease, with a cap and a `KNOWLEDGE_LOCK_UNAVAILABLE` 503 (`internal/rag/knowledge_lock.go:15`, `26`, `33`, `62-80`). `docs/architecture/operations.md:27` documents the lease; the ADR does not.
- **Settings copy.** The hint under the non-Azure embedding-model override (`frontend/src/i18n/en.settings.ts:997`) still says "The pgvector index tolerates a model swap; mismatched-dimension chunks are excluded at insert". In fact, an upload into a corpus embedded with a different model is refused with `EMBEDDING_MODEL_CHANGED` until a forced re-index (`internal/rag/service.go:540-551`).
- **ADR 0027 §7 holds in production.** Its rule that a background run needs the org's shared credential is true there. `onOrgCredential` also accepts `embedOverride` (`internal/rag/reembed_job.go:87-89`), but that field is a test hook, nil in production (`service.go:100`). The misleading text is the comment at `reembed_job.go:85`, which calls it "the single-provider deployment's" embedder.

**Evidence.**
- `docs/adr/0027-embedding-identity-on-chunks.md:13` — "The sample guard stays only for rows without an identity"
- `internal/rag/service.go:382` — falls through to the sample unless `Stranded == 0 && Legacy() == 0` (382-389)
- `docs/adr/0027-embedding-identity-on-chunks.md:19` — §9 names only `pg_advisory_xact_lock(hashtext(org_id))`
- `internal/rag/knowledge_lock.go:62` — `lockOrg` takes a cross-replica lease per org (62-80); `ErrKnowledgeLockUnavailable` (:15), key space (:26), `DefaultLeaseCap` (:33); already documented at `docs/architecture/operations.md:27`
- `internal/rag/reembed_job.go:85` — the comment calls the override "the single-provider deployment's"; `onOrgCredential` accepts any `embedOverride` (87-89), which `internal/rag/service.go:100` documents as "a test hook; nil in production"
- `frontend/src/i18n/en.settings.ts:997` — "The pgvector index tolerates a model swap; mismatched-dimension chunks are excluded at insert", compared with `internal/rag/service.go:540-551` (`ErrEmbeddingModelChanged`)

**Change.**
- **ADR 0027 §3.** Say the sample also runs on a corpus with stranded rows.
- **ADR 0027 §9.** Add the cross-replica lease, or record it in a short new ADR:
  - its key space: `0x6b6e6f77` high word, FNV-1a-32 of `knowledge_org:<org id>` low word;
  - the per-replica cap (`PAD_KNOWLEDGE_LEASE_CAP`, default 4);
  - `KNOWLEDGE_LOCK_UNAVAILABLE`.
- **Leave unchanged:** the Context at line 7, which describes the state before the change, and `operations.md`, which already documents the lease.
- **`reembed_job.go:85`.** Fix the comment to say the override is a test hook.
- **Settings copy.** Rewrite the embedding-model hint in `en.settings.ts` to say that a different model makes uploads fail with `EMBEDDING_MODEL_CHANGED` until a forced re-index. `REINDEX_REQUIRED` belongs to the Azure registry's knowledge-base model (`internal/service/azure_models_entries.go:294`, `docs/ai-foundry.md:14`), not to this setting.

**Touches.** `docs/adr/0027-embedding-identity-on-chunks.md`, `frontend/src/i18n/en.settings.ts`, `internal/rag/reembed_job.go`

**Done when.**
- ADR 0027 (or a new ADR) names the lease key space, the cap and the 503 code, and states that the sample runs on a stranded corpus.
- The settings copy no longer claims the index tolerates a model swap, and the i18n coverage test still passes for the edited key.

**ADR.** ADR-0027 §3 and §9 describe narrower behaviour than internal/rag implements

### R-docs-8

**ADR 0031 drift: an unadopted server-owned turn leaves a stale version, so the next clear or compact is refused as 'changed elsewhere'** · tier 3

**Problem.** ADR 0031's consequences (line 19) say the client's save after a server-owned turn it cannot adopt "conflicts by design and costs one reload". For a later client save that is roughly right, because `saveThreadConversation` reconciles quietly with a reload (`frontend/src/lib/conversationSync.ts:80-99`). The drift is limited to clear and compact.

The code never saves a server-owned turn on done: save is `!serverOwned && !adopted` (`frontend/src/components/chat/hooks/stream-engine/events.ts:58`). So when `adoptStreamVersion` refuses, the thread keeps its stale version and nothing reloads it. `adoptStreamVersion` refuses unless the held version equals the base the server names (`conversationSync.ts:60-67`).

The concrete case where the server's own turn is the only other writer is a reload or re-attach mid-stream:
1. `loadThreadHistory` stores `load.version`, which is read after the question write, then resumes the stream (`frontend/src/components/chat/hooks/useChatConversations.ts:30-37`).
2. The done event's `conversationBase` is the version from before the question write (`internal/service/chat_conversation_version.go:87-90`, `175-182`), so adoption fails at `conversationSync.ts:64`.
3. The next Clear or Compact gets a 409, adopts the server list and returns false (`clearThreadConversation`, `conversationSync.ts:125-135`; `replaceThreadConversation`, `106-122`).
4. The UI shows the "changed elsewhere" toast (`frontend/src/components/chat/hooks/useAIChat.ts:205-208`) although nobody else wrote, and the user has to repeat the action.

When `conversationBase` is absent because another writer did get in between (`chat_conversation_version.go:175-177`), the toast is truthful.

**Evidence.**
- `docs/adr/0031-conversation-versioning.md:19` — "The client's save after a server-owned turn that it cannot adopt conflicts by design and costs one reload"
- `frontend/src/components/chat/hooks/stream-engine/events.ts:52` — `adopted = adoptStreamVersion(...)`; `save: !acc.serverOwned && !adopted` (58), so a server-owned turn is never saved
- `frontend/src/lib/conversationSync.ts:60` — `adoptStreamVersion` moves the version only from the exact base (60-67); `saveThreadConversation` reconciles with a reload (80-99)
- `frontend/src/components/chat/hooks/useChatConversations.ts:30` — history load stores `load.version`, then resumes the active stream (30-37)
- `internal/service/chat_conversation_version.go:87` — `convWrite{Base, Version}` (87-90); `stampConversationVersion` sends both only when this stream wrote and no one else did (175-182)
- `frontend/src/lib/conversationSync.ts:125` — `clearThreadConversation`: on conflict calls `adoptServerConversation` and returns false (125-135); `replaceThreadConversation` does the same (106-122)
- `frontend/src/components/chat/hooks/useAIChat.ts:205` — false leads to the `errors.changedElsewhere` toast (205-208)

**Change.** Fix the ADR 0031 sentence to describe what actually happens: no client save follows a server-owned turn, and a done that cannot be adopted triggers one refresh of the thread's version. Add a small `syncThreadVersion` that does one GET with no save and adopts `load.version`. Call it only for the done of a server-owned turn that carries no `conversationVersion`, or whose `conversationBase` differs from the held version. Keep the existing "changed elsewhere" behaviour when another writer really did write, because the toast is truthful then.

**Touches.** `frontend/src/components/chat/hooks/stream-engine/events.ts`, `frontend/src/lib/conversationSync.ts`, `docs/adr/0031-conversation-versioning.md`, `frontend/src/components/chat/hooks/useChatStreamEngine.saveOnDone.test.ts`

**Done when.**
- A `useChatStreamEngine` test shows that a done whose `conversationBase` differs from the held version triggers exactly one `/api/chat/get`, and the thread adopts `load.version`. A clear that follows sends that version and succeeds without a toast.
- ADR 0031's consequence describes the refresh, not a client save.

**ADR.** ADR-0031 Consequences describes a client save after a server-owned turn that the code never makes

### R-docs-9

**ADR 0005, ADR 0012 and the provider-test swagger drift: keystore format, settings sections and blob job, Azure test budget gating, deprecated routes without a removal date** · tier 3

**Problem.**
- **ADR 0005 (keystore format).** Line 11 states one `enc:v1:` format with table/column/row AAD, and lists `provider_keys.ciphertext` and `org_provider_keys.ciphertext` among the sealed columns. The keystore actually stores bare base64(nonce‖ciphertext) bound to a (scope, provider) AAD (`internal/storage/database/keystore.go:80-92`).
- **ADR 0012 (section list).** Line 11 lists seven sections and leaves out `profile`, which `internal/storage/settingsdoc/sections.go:15` declares.
- **ADR 0012 (step 1).** The ADR schedules contract step (1), a boot job that copies blobs into rows after which saves stop writing the blob (line 14), for the "next release". The code consistently still writes the blob (`sections.go:10-12`; `postgres_settings.go:25-26`; writes at `:156` and `:178`), and `internal/app/app.go:63-79` wires no such job. Whether to schedule the job or extend the dual write is a decision to record, not a docs mismatch.
- **Azure test swagger (budget).** The org Azure test swagger (`internal/api/org_azure.go:144`) says both that baki's daily budget does not count Azure usage and that a model test is gated on the organisation's daily budget. `admitProbe` asks only the provider policy (`internal/service/azure_models_runtime.go:850-855`), and ADR 0028 says the Azure probes carry no checker (`docs/adr/0028-ai-budget-hook-and-provider-policy.md:22`).
- **Provider test swagger.** `internal/api/handlers_providers.go:97` says "the daily AI budget gates the test like any other call", which is true only for priced providers.
- **Billing.** ADR 0028:22 also says the probes "make no billed call by design", which contradicts `org_azure.go:144` ("one tiny request Azure bills").
- **Deprecated forms.** Both test routes carry a deprecated form "(one release)" with no tracked removal.

**Evidence.**
- `docs/adr/0005-secret-columns-encrypted-at-rest.md:11` — `enc:v1` format with table/column/row AAD; lists `provider_keys.ciphertext` and `org_provider_keys.ciphertext`
- `internal/storage/database/keystore.go:80` — `keyAAD(scope, provider)`; `encrypt` returns bare base64(nonce‖ciphertext) with no prefix (86-92)
- `docs/adr/0012-settings-sections-with-versions.md:11` — section list omits `profile` (`sections.go:15` declares it); line 14 schedules step (1) for the next release
- `internal/storage/database/postgres_settings.go:26` — "for one release, also whole in the legacy blob"; `saveLegacySQL` (156) and the section upsert (178) are both still written; `internal/app/app.go:63-79` wires no blob-copy job
- `internal/api/org_azure.go:144` — "baki's daily budget does not count Azure usage", "a model test one tiny request Azure bills" and "a model test is also gated on the organisation's daily budget" in one description; "Deprecated form (one release)"
- `internal/service/azure_models_runtime.go:850` — `admitProbe` is `factory.Admit` (provider policy) only (850-855)
- `docs/adr/0028-ai-budget-hook-and-provider-policy.md:22` — "the Azure probes, carry no checker — they make no billed call by design"
- `internal/api/handlers_providers.go:97` — "the daily AI budget gates the test like any other call"; "For azure-foundry this form is deprecated (one release)"

**Change.**
- **ADR 0005.** Amend the format sentence to describe the keystore's own encoding and its (scope, provider) AAD.
- **ADR 0012.** Add `profile` to the section list, and record the step (1) decision: schedule the blob-copy job, or state that the dual write continues.
- **Swagger and ADR 0028.**
  - Fix both swagger descriptions: the Azure probes are gated by the provider policy, not the budget.
  - Fix ADR 0028:22's "no billed call" sentence in the same change, so the texts agree on whether a model test is billed.
  - Regenerate `docs/swagger.*`.
- **Deprecated forms.** Open a tracked removal item, with release and date, in the refactoring roadmap for the deprecated single-document test forms.

**Touches.** `docs/adr/0005-secret-columns-encrypted-at-rest.md`, `docs/adr/0012-settings-sections-with-versions.md`, `docs/adr/0028-ai-budget-hook-and-provider-policy.md`, `internal/api/org_azure.go`, `internal/api/handlers_providers.go`, `docs/swagger.yaml`, `docs/swagger.json`, `docs/docs.go`, `docs/architecture/backend-refactoring-proposal.md`

**Done when.**
- The Azure test route descriptions no longer say the daily budget gates them, ADR 0028:22 agrees with them on billing, and `docs/swagger.*` is regenerated. This is checked in review; no swagger-grep test is needed.
- ADR 0012's section list matches the keys of `settingsdoc.Default`. Any automated check reuses `TestSections_CoverTheDocument` rather than parsing the markdown.
- ADR 0012 records the step (1) decision.
- A dated removal entry for the deprecated test forms exists in the refactoring roadmap.

**ADR.** ADR-0005 format sentence and ADR-0012 section list do not match the code; ADR-0012 step (1) needs its scheduling decision recorded; ADR-0028's "no billed call" sentence contradicts the org Azure test swagger
