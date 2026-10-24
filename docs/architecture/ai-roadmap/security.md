# AI roadmap: Security

Items of category `security` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-security-4

**Flow mutations made by the AI assistant (chat-approved and HTTP auto-fixes) and approval decisions leave no audit row** · tier 1

**Problem.** Four routes write no audit entry at all: `POST /api/flow/apply-fix`, `/apply-fix-batch`, `POST /api/chat/fix-decision` and the chat-driven applier. The chat apply leaves only a `RecordFlowOp` metric and a `logger.Info` with flow, rule and fix, with no actor, stream or proposal id. Decisions are counted only in a metric, and the audit action list has no fix-apply or decision action. The only chat audit row is `chat.stream` at stream start, with the `streamId`. The gap goes beyond the assistant. The only `flow.save` record site (`library.go:259`) audits an org library create, and manual content updates (`library.go:409-457`, `flow_source.go:160-190`) are not audited either. Undo snapshots capture the actor's `UserID` (`flow_snapshots.go:255-262`), but that is a bounded undo ring, not an audit trail. The highest-impact write the assistant can make is editing a flow after a one-click approval. For that write, nothing can answer "who changed this flow, when, approved from which chat, with the unsafe flag or not".

**Evidence.**
- `internal/service/chat_fixes.go:324` — `applyApprovedFix`: `RecordFlowOp("chat_apply_fix")` then `applyFixHook`; `371` is only `logger.Info("chat-driven fix applied", flow, rule, fix)`; the batch at `642` is the same; there is no `audit.Recorder` in the file
- `internal/api/flow_fixes.go:27` — `handleApplyFix` (`27-61`) and `handleApplyFixBatch` (`114-157`) call no `h.audit.Record`; the file imports no audit package (`3-13`)
- `internal/api/handlers_chat.go:211` — `handleFixDecision` (`211-242`) records no audit; the only chat row is `ActionChatStream` with `Meta{streamId}` at `88`
- `internal/audit/actions.go:4` — the action constant list (`4-79`) has `flow.save`, `flow.version_save` and `flow.version_restore`, but no fix-apply or fix-decision action
- `internal/service/chat_approvals.go:152` — `emitApprovalDecision`: a `recordChatFixDecision(status)` metric, then the SSE event; no audit, no log
- `internal/api/library.go:259` — the one `ActionFlowSave` record site audits an org library create; content updates (`library.go:409-457`, `flow_source.go:160-190`) record nothing
- `internal/service/flow_snapshots.go:255` — durable undo snapshots store the actor's `UserID` (`255-262`), a bounded trail that is not an audit

**Change.** As the first slice of auditing flow content mutations, add two audit actions:
- `flow.fix_apply`, with Meta: `source=http|chat`, `ruleId`, `fixType`, `blockId`, `unsafe`, `streamId`, `proposalId`, `outcome=applied|applied-unresolved|error`, and the applied count for batches.
- `chat.fix_decision`, with Meta: `streamId`, `proposalId`, `status` (`approved|declined|timeout|cancelled`) and `excludedCount`.

Record them from the HTTP handlers and the fix-decision route. For chat applies, record them from a completion hook that the chat applier calls with the stream owner captured on `streamCtl` and the flow's `OrgID` (thread an `audit.Recorder` or an event into `ChatService`). Add both actions to `AllActions` and the admin filter. Save-source, library content updates, suppress-in-source and snapshot restore are the follow-up slice.

**Touches.** `internal/audit/actions.go`, `internal/api/flow_fixes.go`, `internal/api/handlers_chat.go`, `internal/service/chat_fixes.go`, `internal/service/chat_approvals.go`, `internal/service/chat.go`, `internal/di/services.go`, `internal/api/audit_record_sites_test.go`, `frontend/src/components/admin`

**Done when.**
- Approving an `apply_fix` in chat on an org flow yields two audit rows (`chat.fix_decision`, `flow.fix_apply`). Both carry the same `streamId` and `proposalId`, the stream owner as actor, `OrgID`, `ruleId`, `fixType` and `outcome`.
- `POST /api/flow/apply-fix` (with `unsafe=true` carried in meta) and `/apply-fix-batch` (with the applied count) each record `flow.fix_apply`.
- `audit_record_sites_test` lists the new record sites, the closed-set check on `AllActions` in `actions_test` passes, and the admin audit filter lists the new actions.

### R-security-3

**Chat conversations are flow-scoped, not user-scoped: any viewer of a flow can read, extend or (as editor) migrate-and-delete another user's thread by key** · tier 2

**Problem.** A conversation row is keyed only by `(flow_id, scope)`. The scope is the client-chosen thread id, the legacy context block id, or the literal `flow`. The live RLS policy (v27, `postgres_migrations.go:2109-2112`) delegates to `rls_flows_visible`. Every owner, collaborator and org member who can see the flow therefore has read and write on every conversation row of that flow, and there is no per-user column. `/api/chat/get` needs only `ChatUse` (flowPerm viewer) and passes the client's key straight to `ReadConversation`. That function loads by flow and key and uses the caller's scope only to find an active stream.

On cloud this means:

- Any flow viewer can read and append to, and any editor can clear (`handlers_chat.go:474`), the conversations stored under the legacy `flow` or block-id keys. Older clients and API callers that send no `conversationId` wrote those.
- They can do the same to any UUID thread whose id they obtain.
- User B's own turn on such a key loads user A's history into B's model context.

Account erasure is also affected: it deletes only the conversations of flows the user owns (`docs/architecture/data-lifecycle.md:12`), so a user's conversations on other people's flows survive.

**Evidence.**
- `internal/storage/database/postgres_migrations.go:2109` — `rls_conversations_visible` (2109-2112), the live v27 policy, delegates to `rls_flows_visible`: any caller who can see the flow passes, and there is no per-user column
- `internal/storage/database/postgres_flows.go:705` — `LoadConversation` selects by `flow_id` and `scope` only (708)
- `internal/service/chat_conversation_version.go:274` — `ReadConversation` calls `LoadConversationVersioned(flowID, convKey)`; the caller scope is used only for `activeStreamCtl` at 266
- `core/models/chat.go:146` — `ConversationKey()` returns `conversationId`, else `contextBlockId`, else `"flow"`; the last two are guessable
- `internal/api/handlers_chat.go:369` — `/api/chat/get` resolves the flow with `service.ChatUse` (`authz_can.go:92`: flowPerm viewer) and passes `req.Provider` through as the key (374)

**Change.** Make conversations owned:

- **Migration:** an expand migration adds `owner_id` (NULL for legacy rows). Write it as a `.sql` file in `internal/storage/database/migrations/` with a `-- baki:compat expand` line, and refresh `TestMigrationChecksums_Golden`.
- **Writes:** every server-side write sets `owner_id` (`persistUserTurnVersioned`, `persistAssistantTurn`, `SaveClientConversation`).
- **RLS:** `rls_conversations_visible` tightens to `owner_id = app_current_user_id()`. Legacy NULL rows stay readable by the flow audience for one release and can never be cleared by another user.
- **Reads:** `ReadConversation`, `SaveClientConversation` and Clear also filter by caller.
- **Erasure:** once `owner_id` exists, account erasure must also delete the erased user's conversations on flows they do not own.

**Touches.** `internal/storage/database/migrations/` (new expand step), `internal/storage/database/postgres_flows.go`, `internal/storage/database/postgres_conversations_version.go`, `internal/storage/database/lifecycle_erase.go`, `internal/service/chat_conversation_version.go`, `internal/service/chat_conversation.go`, `internal/api/handlers_chat.go`, `internal/storage/interfaces/*.go`

**Done when.**
- Postgres test: users A and B are both org members of one flow. B's `POST /api/chat/get` with A's thread key (and with key `flow` or a block id A used) returns an empty conversation or 403, never A's messages.
- B's `/api/chat/stream` with `conversationId` equal to A's thread id starts a conversation that contains none of A's history.
- Account erasure deletes the erased user's conversations on flows they do not own.

### R-security-7

**Weak encryption key accepted outside cloud mode; the JWT secret doubles as the keystore key by default** · tier 2

**Problem.** The strength rule for `PAD_ENCRYPTION_KEY` applies only when `Mode == cloud`, yet every database-backed deployment, local mode included, seals every stored provider key with it. The auth-secret fallback is strength-checked whenever auth is enabled (`loader.go:652`). The weak-key case is therefore only an explicit weak `PAD_ENCRYPTION_KEY` with `PAD_STORAGE=database` outside cloud.

Without `PAD_ENCRYPTION_KEY`, the auth secret becomes the keystore key, with only a warning. `docker-compose.prod.yml:73` already requires the key, but `infra/main.bicep:622` allows the fallback. On that path, the most common operator action, rotating `PAD_AUTH_SECRET`, makes every stored credential fail with `ErrSecretDecryptFailed`. The only way to avoid that is to list the old secret in `PAD_ENCRYPTION_KEY_PREVIOUS` beside a new dedicated key, which the operator has to know to do. The damage is not limited to provider keys:

- all field-sealed columns (ADR 0005; `internal/app/keystore.go:96`);
- Redis command signing, whose key derives from the same shared secret (`internal/di/services.go:692-698`).

**Evidence.**
- `internal/config/loader.go:660` — `isWeakSecret` runs on `EncryptionKey` only when `cfg.Mode == ModeCloud` and the key is set
- `internal/app/keystore.go:20` — `keystoreKey` falls back to `a.Secret` when `EncryptionKey` is unset (27)
- `internal/app/keystore.go:75` — the fallback produces a `logger.Warn`, not a refusal (75-77)
- `internal/storage/database/keystore.go:23` — doc comment: the auth secret is accepted for backward compatibility, and a key no longer configured renders its ciphertext undecryptable (26-29)
- `internal/config/loader.go:833` — `validateEncryptionKeyPrevious`: rotating away from the fallback requires listing the old secret by hand (834)

**Change.** First priority: in cloud mode, refuse the `PAD_AUTH_SECRET` fallback (or give it a dated deprecation), and make the bicep `encryptionKey` parameter required, matching `docker-compose.prod.yml:73`. A cheap add-on is to extend the strength rule to local mode with a database keystore (`PAD_STORAGE=database`). Add a `bakicli` check, next to the existing `bakicli migrate` tooling, that reports which configured key currently opens the stored rows, so operators can reseal before a JWT rotation.

**Touches.** `internal/config/loader.go`, `internal/app/keystore.go`, `cmd/bakicli/`, `infra/main.bicep`, `docs/architecture/operations.md`

**Done when.**
- Config test: `PAD_MODE=local PAD_STORAGE=database PAD_ENCRYPTION_KEY=secret` fails validation.
- Config test: `PAD_MODE=cloud` without `PAD_ENCRYPTION_KEY` fails, or warns with a dated deprecation (per the decision), instead of silently keying the keystore from `PAD_AUTH_SECRET`.
- `infra/main.bicep` requires the `encryptionKey` parameter, as `docker-compose.prod.yml:73` requires `PAD_ENCRYPTION_KEY`.

### R-security-5

**Knowledge upload filename is unvalidated: empty, oversized or newline-bearing names are stored, drive replace semantics and are rendered into the prompt** · tier 3

**Problem.** `handleKnowledgeUpload` checks only the content length (`org_knowledge.go:88-91`), and `AddDocument` checks only that the content is not blank (`rag/service.go:495-497`). The filename is the replace key: every existing document with the same name is superseded (`rag/service.go:533-535`). It is stored in a `TEXT` column with no length constraint (`postgres_migrations.go:1069`) and recorded unbounded in the audit meta (`org_knowledge.go:101`). It is also rendered verbatim as `[filename]` in the guidelines block of every chat turn that retrieves the chunk (`rag/service.go:947`). An empty name makes every nameless upload supersede the previous one, and a megabyte-long name is accepted. This is an input-validation and robustness item on an admin-only route (`org_knowledge.go:359-364`), not a prompt-injection issue. The filename sits inside the scrubbed guidelines block (`chat.go:617`), and the same admin controls the content anyway.

**Evidence.**
- `internal/api/org_knowledge.go:81` — body `{filename, content}`; only `len(content)` is checked (88-91) before `AddDocument` (93); the raw filename goes into the audit meta (101)
- `internal/api/org_knowledge.go:359` — upload and reindex are org-admin routes (359-364)
- `internal/rag/service.go:494` — `AddDocument` validates the content only (495-497)
- `internal/rag/service.go:535` — `supersededDocIDs` is keyed on the filename (replace semantics, 533-535)
- `internal/rag/service.go:947` — `assembleGuidelines` renders `- [%s] %s` with the raw filename
- `internal/storage/database/postgres_migrations.go:1069` — `knowledge_documents.filename TEXT NOT NULL`, no length bound
- `internal/service/chat.go:617` — the guidelines enter the user turn through `scrubber.ScrubText`

**Change.** Validate the filename in the handler: non-empty after trimming, at most 255 bytes, valid UTF-8, no control characters and no path separators (basename only). Refuse an invalid name with 400 and a machine code before any embedding call.

**Touches.** `internal/api/org_knowledge.go`, `internal/api/handlers_org_knowledge_test.go`

**Done when.**
- Uploads with the filename `''`, `'a\nb'`, `'../x.md'` or a 300-byte name are refused with 400 before any embedding call.

### R-security-8

**Desktop (keyring + file registry) has no orphan recovery for Azure credentials** · tier 3

**Problem.** The orphan sweep returns early without a `SecretFamilyLister`, because the keyring cannot list (`azure_models_lifecycle.go:182-184`). `credentialStamp` reports "not known" (`:254-256`), so desktop deletes are unconditional (`:274-277`). A failed post-commit keyring delete is only logged, with the note "the orphan sweep retries" (`azure_models.go:1602-1605`). On the desktop the sweep never runs, so that log text is wrong on the keyring path. `rollbackStalePending` covers pending rows only (`azure_models_lifecycle.go:290-310`). A resource whose keychain delete failed leaves a live credential in the OS keyring forever, invisible to the UI. A database deployment without an encryption key has the same no-sweep behaviour, because it keeps provider keys in the OS keyring (`internal/app/keystore.go:69-74`).

**Evidence.**
- `internal/service/azure_models_lifecycle.go:182` — `sweepOrphans`: `s.keyFamilies == nil` → return (182-184)
- `internal/service/azure_models_lifecycle.go:254` — `credentialStamp`: no `keyFamilies` → `known=false` (254-256)
- `internal/service/azure_models_lifecycle.go:274` — `deleteCredentialIfUnchanged`: `!known` → unconditional `deleteCredential` (275-277)
- `internal/service/azure_models.go:1602` — txhook: a failed credential delete is only logged, "the orphan sweep retries" (1602-1605)
- `internal/service/azure_models_lifecycle.go:296` — `rollbackStalePending` handles pending rows only (290-310)
- `internal/app/keystore.go:69` — a database deployment without `PAD_ENCRYPTION_KEY`/`PAD_AUTH_SECRET` keeps provider keys in the OS keyring (69-74)

**Change.** Fix the log text on the keyring path so it no longer promises a sweep. When a keyring delete fails, record the key and a timestamp in a small list in the file registry. Retry each listed delete on the next `prepareOwner`/`rollbackStalePending` pass, and drop the entry once the keyring delete succeeds. The panel counter and the GDPR-export changes are dropped.

**Touches.** `internal/storage/azure_registry_file.go`, `internal/storage/interfaces/azure_registry.go`, `internal/service/azure_models.go`, `internal/service/azure_models_lifecycle.go`

**Done when.**
- File-registry test: a delete whose keyring `Delete` fails leaves an entry in the list, and the next `prepareOwner` retries it and clears it once `Delete` succeeds.
- The log for a failed keyring delete no longer says the orphan sweep retries.

### R-security-11

**AI usage records are missing from the user data export** · tier 3

**Problem.** The lifecycle table registry marks `usage_metrics` "not in the bundle yet (open: AI usage records)" (`lifecycle_users.go:34-36`). A user's personal AI spend history (provider, model, tokens, cost, timestamps) is personal data. Erasure handles it (`erasedByStep`), but the data-subject export omits it.

**Evidence.**
- `internal/storage/database/lifecycle_users.go:34` — `usage_metrics` `noExport`: "not in the bundle yet (open: AI usage records)", `erase: erasedByStep` (34-36)
- `internal/storage/database/lifecycle_users.go:87` — erasure keeps org-billed rows without `user_id` and deletes the rest (87-88)
- `internal/storage/database/lifecycle_users.go:126` — `exportAuditEvents`: the keyset paging pattern (126-142)
- `internal/storage/interfaces/lifecycle.go:6` — `UserDataExport`, the bundle type (6-23)
- `internal/storage/database/postgres_migrations.go:2070` — `usage_metrics_user_created_idx ON usage_metrics (user_id, created_at)`
- `internal/storage/database/postgres_migrations.go:1047` — `usage_metrics` columns: provider, model, prompt/completion tokens, estimated cost, `created_at`

**Change.** Add an `aiUsage` export section through the `section(...)` registry in `lifecycle_users.go`. Page the requesting user's rows (`user_id = $1`), including org-billed ones with their `org_id`, using the keyset pattern of the audit-event export. Walk by `(user_id, created_at)` on `usage_metrics_user_created_idx`. Add the field to `UserDataExport` in `internal/storage/interfaces/lifecycle.go`. (`internal/storage/interfaces/export.go` does not exist.) Give the table claim `export: "aiUsage"` and drop the `noExport` text. Org rows keep `org_id`, per the existing `orgKept` claim. After erasure, those rows lose `user_id` (`lifecycle_users.go:87`), so the section matches the erase semantics.

**Touches.** `internal/storage/database/lifecycle_users.go`, `internal/storage/interfaces/lifecycle.go`

**Done when.**
- A `DATABASE_URL` test shows that the export of a user with usage rows contains them. The registry entry gains `export: "aiUsage"`, and the no-export reason is removed.

### R-security-c22

**Billed routes come from a hand-kept path list: preview-context and knowledge upload/reindex make billed provider calls on the general bucket, and no test enforces 'billed means co…** · tier 3

**Problem.** The rule that a billed provider call uses a cost bucket exists only as a comment in `rateLimitGroup` (`middleware_chain.go:194`) and as a few hard-coded paths and test cases (`TestRateLimitGroup_OtherBuckets`, `middleware_chain_test.go:39`). Nothing enforces it.

`POST /api/chat/preview-context` sits on the general bucket (`route_policy.golden:415`). It runs `buildScrubbedContext`, then `ragGuidelines`, then `KnowledgeService.Search`, then `provider.Embed` (`handlers_chat.go:513`; `chat_context.go:632`, `:222`, `:235`). That is one billed query embedding for every distinct message on an org flow with a non-empty corpus. The query-embedding cache helps only when the same user sends an identical query within 10 minutes (`search_cache.go:23`, `:40-44`). Knowledge upload (up to 2,500 chunk embeddings per call) and reindex (up to 10,000 per request) are on the general bucket too (`route_policy.golden:875`, `:881`; `rag/service.go:70`, `:84`). Re-uploading identical content under the same filename re-embeds every chunk.

The impact is smaller than this suggests:
- The general bucket's sustained rate (1 rps, `config.go:416`) is tighter than the chat bucket's (3 rps, `config.go:422`).
- Preview-context runs on a button click (`ChatInput.tsx:538` → `useAIChat.ts:140`), not on every keystroke.
- `Search` and `AddDocument` check the budget (`rag/service.go:674`, `:504`), except for Foundry, which is never gated (`:499`).
- Upload and reindex are org-admin-only (`org_knowledge.go:359-364`).

The concrete gaps are the unenforced billed-route classification and the re-embedding of identical re-uploads. This is tier 3.

**Evidence.**
- `internal/api/testdata/route_policy.golden:415` — `POST /api/chat/preview-context ... ratelimit=general`
- `internal/api/testdata/route_policy.golden:875` — `POST /api/orgs/{id}/knowledge/reindex ... ratelimit=general timeout=bulk`; upload at 881 likewise
- `internal/api/middleware_chain.go:194` — the provider test "fires up to three billed completions … so it rides the chat bucket": the only statement of the rule
- `internal/api/middleware_chain_test.go:39` — `TestRateLimitGroup_OtherBuckets` pins buckets by explicit path; no case covers preview-context or knowledge
- `internal/api/handlers_chat.go:513` — `handlePreviewContext` calls `chatSvc.PreviewContext`
- `internal/service/chat_context.go:632` — `PreviewContext` calls `buildScrubbedContext`, which calls `ragGuidelines` (222), which calls `s.knowledge.Search` (235)
- `internal/rag/search_cache.go:40` — query embeddings are keyed by caller and query (40-44), 10 min TTL (23)
- `internal/rag/service.go:84` — `maxKnowledgeChunks = 2500`; `maxReindexChunks = 10000` (70)
- `internal/config/config.go:416` — general bucket 1 rps; chat bucket 3 rps (422)
- `frontend/src/components/chat/ChatInput.tsx:538` — the preview is a button; `useAIChat.ts:140` calls `chatApi.previewContext`
- `internal/rag/service.go:674` — `Search` checks the budget; `AddDocument` at 504; Foundry is never gated (499)
- `internal/api/org_knowledge.go:359` — upload and reindex are org-admin routes (359-364)
- `internal/storage/database/migrations/0042_knowledge_embedding_identity.up.sql:9` — chunks carry `embedding_identity` and documents carry `chunker_version`; no content hash is stored

**Change.** Add a `route.Billed()` policy attribute, required on every handler that can reach `ai.Provider` Chat, Stream or Embed. Derive the cost buckets from it: the RL-2 per-member AI bucket (item c21) plus the address bucket. Add a `billed=yes` column to `route_policy.golden`, plus a test that fails if a billed route uses the general bucket. Move preview-context onto the per-member AI bucket. Moving it onto the chat bucket alone would loosen it (3 rps against 1 rps). Give knowledge upload and reindex their own embed bucket. Skip re-embedding when an uploaded document's content hash and embedding identity match the stored document with the same filename, and report the document as unchanged. `knowledge_documents` stores no content hash today, so one must be added.

**Touches.** `internal/api/route/`, `internal/api/route_policy.go`, `internal/api/middleware_chain.go`, `internal/api/handlers_chat.go`, `internal/api/org_knowledge.go`, `internal/api/testdata/route_policy.golden`, `internal/api/middleware_chain_test.go`, `internal/rag/service.go`

**Done when.**
- `route_policy.golden` marks chat/stream, chat/preview-context, providers/test, both Azure tests and knowledge upload/reindex as billed, and a test fails if any billed route resolves to the general bucket.
- Requests to preview-context count against the per-member AI bucket.
- Re-uploading byte-identical content under the same filename makes zero embedding calls (a fake provider counts them) and reports the document as unchanged.
