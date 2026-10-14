# Test map

Tests live in the same directory (and package) as the code they test — Go's
requirement — and file names encode the coverage area: `<area>_<subject>_test.go`.
This map is the per-package "browse by coverage" view. Names dropped the
historical work-round tags (audit9/fix2/f2/f3/perf_roundN); what a file pins is
stated in its package doc comment and test names.

Conventions:
- `*_test.go` in-package files may use unexported identifiers (white-box).
- `internal/storage/database` additionally has `package database_test` files (external, black-box integration).
- Shared helpers (`helpers_test.go`, `*helpers_test.go`, `main_test.go` with TestMain) are not coverage files.
- Run everything: `go test ./... ./core/...` (CI also uses `-race`).
- Cross-cutting safety nets (route-policy and authz goldens, migration checksum golden, findings, export and caches
  goldens, architecture ratchets, fake-backend contract, isolated Postgres databases, request-chain benchmark) are
  described in [architecture/safety-nets.md](architecture/safety-nets.md).

## root (package main)

The binary: the build stamps and `main.go` as the package's only file, the app graph with the stamps main passes, the
desktop boot smoke test (builds and runs the backend; `-short` skips it).

- **app*** (1): `app_graph_test.go`
- **buildstamp*** (1): `buildstamp_test.go`
- **desktop*** (3): `desktop_boot_test.go`, `desktop_boot_unix_test.go`, `desktop_boot_windows_test.go`

## internal/app

The fx application main runs: the app graph, the stop order and desktop handshake of a real boot, the stop as a
chat client sees it (draining probes, the terminal SERVER_RESTARTING event before the event stream closes), the retention
sweep, the field-encryption backfill, the key-rotation re-seal pass, the keystore key, the legacy migration source,
the deployment notifier and scanner wiring, the session key handoff, the drain delay's default against the
capability profile, the Postgres configuration built from the config.

- **app*** (1): `app_graph_test.go`
- **config*** (1): `config_test.go`
- **field*** (1): `field_encryption_backfill_test.go`
- **governance*** (1): `governance_test.go`
- **jobs*** (1): `jobs_test.go`
- **keystore*** (1): `keystore_key_test.go`
- **migration_source*** (1): `migration_source_test.go`
- **observability*** (1): `observability_test.go`
- **provide_notifier*** (1): `provide_notifier_test.go`
- **provide_scanner*** (1): `provide_scanner_test.go`
- **retention_sweep*** (1): `retention_sweep_test.go`
- **secret_reseal*** (1): `secret_reseal_test.go`
- **session*** (1): `session_secret_test.go`
- **stop_order*** (1): `stop_order_test.go`
- **stop_sequence*** (1): `stop_sequence_test.go`
- **storage*** (1): `storage_test.go`

## cmd/bakicli

The bakicli binary: gate, fix, rules, folder loading, baselines, custom rules, output formats, several flows and
the library pass, CI surface, binary-level runs.

- **atomicwrite*** (1): `atomicwrite_test.go`
- **baseline*** (1): `baseline_test.go`
- **ci*** (1): `ci_surface_test.go`
- **cli*** (1): `cli_binary_test.go`
- **customrules*** (1): `customrules_test.go`
- **diff*** (1): `diff_test.go`
- **failclosed*** (1): `failclosed_test.go`
- **fix*** (2): `fix_config_test.go`, `fix_test.go`
- **folderset*** (1): `folderset_test.go`
- **gate*** (1): `gate_test.go`
- **gitrev*** (1): `gitrev_test.go`
- **inline*** (1): `inline_rules_test.go`
- **library*** (1): `library_test.go`
- **load*** (1): `load_test.go`
- **loadfolder*** (1): `loadfolder_test.go`
- **main*** (1): `main_test.go`
- **output*** (1): `output_test.go`
- **parity*** (1): `parity_test.go`
- **preview*** (1): `preview_text_test.go`
- **report*** (4): `report_delivery_test.go`, `report_keyid_test.go`, `report_retry_test.go`, `report_test.go`
- **rules*** (3): `rules_init_test.go`, `rules_meta_test.go`, `rules_test_cmd_test.go`
- **suppressions*** (1): `suppressions_test.go`
- **watch*** (1): `watch_test.go`

## internal/api

HTTP layer: handlers by route area, middleware chain, RLS, events/SSE, audit, security. Test routers run on
`testutil.FakeBackend`; the RLS and transaction tests use Postgres (`DATABASE_URL`).

- **audit*** (3): `audit_helpers_test.go`, `audit_record_sites_test.go`, `audit_record_test.go`
- **auth*** (2): `auth_apitoken_test.go`, `auth_avatar_test.go`
- **authz*** (3): `authz_header_first_test.go`, `authz_matrix_golden_test.go`, `authz_routes_test.go`
- **azure_config*** (2): `azure_config_audit_test.go`, `azure_config_validation_test.go`
- **azure_foundry*** (2): `azure_foundry_e2e_helpers_test.go`, `azure_foundry_e2e_test.go`
- **baseline*** (1): `baseline_ruleset_test.go`
- **bench*** (1): `bench_chain_test.go`
- **ci*** (5): `ci_webhook_delivery_pg_test.go`, `ci_webhook_delivery_test.go`, `ci_webhook_gate_test.go`, `ci_webhook_pg_test.go`, `ci_webhook_token_test.go`
- **dashboard*** (1): `dashboard_params_test.go`
- **declared*** (1): `declared_policy_test.go`
- **endpoint*** (1): `endpoint_test.go`
- **events*** (8): `events_audience_test.go`, `events_deliver_test.go`, `events_fanout_gap_test.go`, `events_fanout_test.go`, `events_revoke_remote_test.go`, `events_revoke_replicas_test.go`, `events_revoke_test.go`, `events_test.go`
- **export*** (1): `export_routes_test.go`
- **f1*** (1): `f1_helpers_test.go`
- **f2*** (1): `f2_helpers_test.go`
- **finding*** (1): `finding_fanout_events_test.go`
- **flow*** (3): `flow_authz_can_test.go`, `flow_record_writes_viewer_test.go`, `flow_snapshots_test.go`
- **governance*** (1): `governance_notices_test.go`
- **handlers_account*** (1): `handlers_account_test.go`
- **handlers_admin*** (2): `handlers_admin_panic_test.go`, `handlers_admin_test.go`
- **handlers_alerts*** (2): `handlers_alerts_detached_test.go`, `handlers_alerts_fallback_test.go`
- **handlers_analysis*** (3): `handlers_analysis_gate_test.go`, `handlers_analysis_record_test.go`, `handlers_analysis_test.go`
- **handlers_analyze*** (1): `handlers_analyze_raw_test.go`
- **handlers_apitoken*** (2): `handlers_apitoken_foreign_test.go`, `handlers_apitoken_test.go`
- **handlers_auth*** (7): `handlers_auth_changepw_test.go`, `handlers_auth_mail_async_test.go`, `handlers_auth_register_test.go`, `handlers_auth_reset_test.go`, `handlers_auth_sessions_test.go`, `handlers_auth_test.go`, `handlers_auth_validation_test.go`
- **handlers_authz*** (1): `handlers_authz_test.go`
- **handlers_azure*** (1): `handlers_azure_registry_test.go`
- **handlers_chat*** (6): `handlers_chat_activestream_test.go`, `handlers_chat_approvals_test.go`, `handlers_chat_commands_test.go`, `handlers_chat_policy_test.go`, `handlers_chat_test.go`, `handlers_chat_version_test.go`
- **handlers_comments*** (2): `handlers_comments_email_test.go`, `handlers_comments_test.go`
- **handlers_csp*** (1): `handlers_csp_test.go`
- **handlers_dashboard*** (2): `handlers_dashboard_analytics_test.go`, `handlers_dashboard_test.go`
- **handlers_dataflow*** (1): `handlers_dataflow_test.go`
- **handlers_export*** (1): `handlers_export_test.go`
- **handlers_flow*** (7): `handlers_flow_block_test.go`, `handlers_flow_cloudfix_test.go`, `handlers_flow_desktop_test.go`, `handlers_flow_gate_test.go`, `handlers_flow_limiter_test.go`, `handlers_flow_tags_test.go`, `handlers_flow_test.go`
- **handlers_governance*** (1): `handlers_governance_test.go`
- **handlers_integrations*** (1): `handlers_integrations_test.go`
- **handlers_library*** (7): `handlers_library_authz_reads_test.go`, `handlers_library_authz_test.go`, `handlers_library_content_etag_test.go`, `handlers_library_filepath_test.go`, `handlers_library_occ_test.go`, `handlers_library_test.go`, `handlers_library_versions_test.go`
- **handlers_org*** (11): `handlers_org_channels_allowed_hosts_test.go`, `handlers_org_channels_kinds_test.go`, `handlers_org_channels_secrets_test.go`, `handlers_org_channels_test.go`, `handlers_org_create_contract_test.go`, `handlers_org_knowledge_identity_test.go`, `handlers_org_knowledge_test.go`, `handlers_org_providers_test.go`, `handlers_org_roster_loads_test.go`, `handlers_org_settings_allowlist_test.go`, `handlers_org_test.go`
- **handlers_orgrules*** (2): `handlers_orgrules_schema_test.go`, `handlers_orgrules_test.go`
- **handlers_policy*** (2): `handlers_policy_crud_test.go`, `handlers_policy_test.go`
- **handlers_portfolio*** (1): `handlers_portfolio_test.go`
- **handlers_providers*** (4): `handlers_providers_azure_test.go`, `handlers_providers_catalog_test.go`, `handlers_providers_local_test.go`, `handlers_providers_test.go`
- **handlers_security*** (1): `handlers_security_headers_test.go`
- **handlers_shared*** (1): `handlers_shared_shell_test.go`
- **handlers_sharing*** (1): `handlers_sharing_test.go`
- **handlers_sso*** (3): `handlers_sso_flow_test.go`, `handlers_sso_link_test.go`, `handlers_sso_test.go`
- **handlers_static*** (2): `handlers_static_precompressed_test.go`, `handlers_static_share_test.go`
- **handlers_suppressions*** (1): `handlers_suppressions_test.go`
- **handlers_system*** (4): `handlers_system_authz_test.go`, `handlers_system_logerror_test.go`, `handlers_system_test.go`, `handlers_system_version_test.go`
- **handlers_triage*** (1): `handlers_triage_test.go`
- **handlers_wsticket*** (1): `handlers_wsticket_test.go`
- **helpers*** (1): `helpers_test.go`
- **idempotency*** (2): `idempotency_pg_test.go`, `idempotency_test.go`
- **knowledge*** (1): `knowledge_reindex_resume_test.go`
- **local*** (1): `local_config_test.go`
- **main*** (1): `main_test.go`
- **metrics*** (1): `metrics_probe_test.go`
- **middleware*** (2): `middleware_chain_panic_test.go`, `middleware_chain_test.go`
- **migration_runner*** (1): `migration_runner_test.go`
- **migration_status*** (1): `migration_status_shared_test.go`
- **org*** (4): `org_ci_credentials_orphan_test.go`, `org_ci_credentials_test.go`, `org_credential_lifecycle_test.go`, `org_invite_email_test.go`
- **org_knowledge*** (2): `org_knowledge_policy_unavailable_test.go`, `org_knowledge_test.go`
- **params*** (1): `params_test.go`
- **per*** (2): `per_user_limiter_wiring_test.go`, `per_user_ratelimit_test.go`
- **perf_hotpath*** (1): `perf_hotpath_reads_test.go`
- **pprof*** (1): `pprof_exposure_test.go`
- **ratelimit*** (1): `ratelimit_groups_golden_test.go`
- **request*** (1): `request_actor_test.go`
- **rls*** (1): `rls_commit_test.go`
- **rls_lazy*** (1): `rls_lazy_scope_test.go`
- **rls_middleware*** (1): `rls_middleware_test.go`
- **route*** (3): `route_path_test.go`, `route_policy_golden_test.go`, `route_policy_test.go`
- **router*** (2): `router_test.go`, `router_ws_drain_test.go`
- **security*** (1): `security_integration_test.go`
- **session*** (1): `session_analytics_test.go`
- **settings*** (1): `settings_localdb_pg_test.go`
- **shutdown*** (1): `shutdown_drain_test.go`
- **sse*** (1): `sse_byte_budget_test.go`
- **version*** (1): `version_header_test.go`
- **views*** (1): `views_wire_shape_test.go`
- **ws*** (3): `ws_e2e_test.go`, `ws_recheck_e2e_test.go`, `ws_ticket_redis_test.go`

## internal/api/middleware

HTTP middleware: compression, rate limiting, access log, timeouts, metrics, recovery.

- **access*** (2): `access_log_test.go`, `access_log_trace_test.go`
- **compress*** (3): `compress_cost_test.go`, `compress_fast_level_test.go`, `compress_test.go`
- **ip*** (1): `ip_test.go`
- **metrics*** (1): `metrics_test.go`
- **metricsguard*** (1): `metricsguard_test.go`
- **middleware_metrics*** (1): `middleware_metrics_test.go`
- **ratelimit*** (4): `ratelimit_compat_test.go`, `ratelimit_redis_test.go`, `ratelimit_retry_after_test.go`, `ratelimit_test.go`
- **recorder*** (1): `recorder_test.go`
- **recovery*** (1): `recovery_test.go`
- **request*** (1): `request_info_test.go`
- **timeout*** (2): `timeout_stream_test.go`, `timeout_test.go`

## internal/api/render

Response rendering helpers.

- **render*** (1): `render_test.go`

## internal/api/route

Route declarations: the policy each route declares, the resolver that picks a request's route, its allocations.

- **resolver*** (2): `resolver_alloc_test.go`, `resolver_test.go`
- **route*** (1): `route_test.go`

## internal/service

Domain services: chat streaming, flows, analysis, providers, dashboard, authz, settings.

- **account*** (1): `account_lifecycle_test.go`
- **ai*** (4): `ai_budget_hardening_test.go`, `ai_budget_reason_test.go`, `ai_budget_redis_test.go`, `ai_budget_test.go`
- **analysis*** (8): `analysis_batch_library_test.go`, `analysis_budget_gate_test.go`, `analysis_cache_tenant_test.go`, `analysis_fresh_claim_test.go`, `analysis_progress_scope_test.go`, `analysis_rules_options_test.go`, `analysis_rules_org_test.go`, `analysis_test.go`
- **analysisdiff*** (1): `analysisdiff_test.go`
- **apitokens*** (1): `apitokens_test.go`
- **auth_mail*** (1): `auth_mail_test.go`
- **authz*** (5): `authz_bench_test.go`, `authz_can_test.go`, `authz_flowperms_test.go`, `authz_memo_test.go`, `authz_test.go`
- **avatar*** (1): `avatar_test.go`
- **azure_knowledge*** (1): `azure_knowledge_embedder_policy_test.go`
- **azure_models*** (12): `azure_models_import_pending_test.go`, `azure_models_kb_test.go`, `azure_models_legacy_test.go`, `azure_models_lifecycle_test.go`, `azure_models_picker_test.go`, `azure_models_policy_test.go`, `azure_models_probe_test.go`, `azure_models_resolve_test.go`, `azure_models_runtime_retention_test.go`, `azure_models_test.go`, `azure_models_text_test.go`, `azure_models_writes_test.go`
- **channels*** (3): `channels_allowed_hosts_test.go`, `channels_org_kinds_test.go`, `channels_test.go`
- **chat*** (1): `chat_test.go`
- **chat_activestream*** (1): `chat_activestream_report_test.go`
- **chat_approvals*** (1): `chat_approvals_test.go`
- **chat_azure*** (2): `chat_azure_registry_test.go`, `chat_azure_setup_errors_test.go`
- **chat_batch*** (1): `chat_batch_test.go`
- **chat_budget*** (2): `chat_budget_alias_test.go`, `chat_budget_concurrent_test.go`
- **chat_commands*** (3): `chat_commands_compat_test.go`, `chat_commands_hardening_test.go`, `chat_commands_test.go`
- **chat_context*** (5): `chat_context_estimate_test.go`, `chat_context_roles_test.go`, `chat_context_scrub_cache_test.go`, `chat_context_scrub_test.go`, `chat_context_window_test.go`
- **chat_conversation*** (4): `chat_conversation_adopt_test.go`, `chat_conversation_cloud_test.go`, `chat_conversation_file_race_test.go`, `chat_conversation_version_test.go`
- **chat_convmutex*** (1): `chat_convmutex_test.go`
- **chat_errors*** (1): `chat_errors_test.go`
- **chat_failure*** (1): `chat_failure_copy_test.go`
- **chat_fix*** (2): `chat_fix_decision_race_test.go`, `chat_fix_item_ids_test.go`
- **chat_fixes*** (4): `chat_fixes_keepalive_test.go`, `chat_fixes_scope_test.go`, `chat_fixes_stale_test.go`, `chat_fixes_test.go`
- **chat_history*** (1): `chat_history_loss_test.go`
- **chat_hosted*** (1): `chat_hosted_idle_test.go`
- **chat_journal*** (1): `chat_journal_test.go`
- **chat_loop*** (2): `chat_loop_memory_label_test.go`, `chat_loop_memory_test.go`
- **chat_org*** (1): `chat_org_key_test.go`
- **chat_output*** (1): `chat_output_limit_test.go`
- **chat_perf*** (1): `chat_perf_test.go`
- **chat_preferences*** (1): `chat_preferences_test.go`
- **chat_preview*** (1): `chat_preview_policy_test.go`
- **chat_prompt*** (1): `chat_prompt_loop_test.go`
- **chat_rag*** (1): `chat_rag_gate_test.go`
- **chat_replay*** (1): `chat_replay_test.go`
- **chat_residual*** (1): `chat_residual_test.go`
- **chat_resume*** (2): `chat_resume_compat_test.go`, `chat_resume_test.go`
- **chat_scrubber*** (1): `chat_scrubber_ordering_test.go`
- **chat_served*** (1): `chat_served_rates_test.go`
- **chat_settings*** (1): `chat_settings_policy_test.go`
- **chat_shared*** (1): `chat_shared_error_flag_test.go`
- **chat_shutdown*** (1): `chat_shutdown_test.go`
- **chat_sourcefiles*** (1): `chat_sourcefiles_test.go`
- **chat_stream*** (1): `chat_stream_history_test.go`
- **chat_token*** (1): `chat_token_memo_test.go`
- **chat_tool*** (1): `chat_tool_exec_test.go`
- **chat_toolevent*** (1): `chat_toolevent_test.go`
- **chat_turn*** (2): `chat_turn_lifecycle_test.go`, `chat_turn_persist_test.go`
- **chat_unknown*** (1): `chat_unknown_provider_test.go`
- **chat_usage*** (1): `chat_usage_memo_test.go`
- **chatprep*** (1): `chatprep_test.go`
- **ci*** (3): `ci_credentials_review_test.go`, `ci_credentials_test.go`, `ci_delivery_test.go`
- **collaborator*** (1): `collaborator_test.go`
- **comments*** (1): `comments_test.go`
- **conversation*** (2): `conversation_store_label_test.go`, `conversation_store_test.go`
- **dashboard*** (5): `dashboard_analytics_test.go`, `dashboard_cache_test.go`, `dashboard_record_test.go`, `dashboard_ruleset_test.go`, `dashboard_test.go`
- **document*** (3): `document_provider_budget_test.go`, `document_provider_cache_test.go`, `document_provider_filepath_test.go`
- **export*** (1): `export_test.go`
- **flow*** (2): `flow_events_test.go`, `flow_test.go`
- **flow_ast*** (1): `flow_ast_cache_test.go`
- **flow_backend*** (1): `flow_backend_reads_test.go`
- **flow_blockedit*** (1): `flow_blockedit_test.go`
- **flow_cloudfix*** (1): `flow_cloudfix_test.go`
- **flow_desktop*** (1): `flow_desktop_folder_test.go`
- **flow_editor*** (1): `flow_editor_coherence_test.go`
- **flow_extra*** (1): `flow_extra_test.go`
- **flow_fix*** (1): `flow_fix_declined_test.go`
- **flow_fixbatch*** (4): `flow_fixbatch_cloud_folder_partial_test.go`, `flow_fixbatch_limit_test.go`, `flow_fixbatch_partial_test.go`, `flow_fixbatch_settings_test.go`
- **flow_folder*** (1): `flow_folder_cache_test.go`
- **flow_header*** (1): `flow_header_failclosed_test.go`
- **flow_search*** (3): `flow_search_index_cache_test.go`, `flow_search_library_candidates_test.go`, `flow_search_oversize_test.go`
- **flow_searchlibrary*** (1): `flow_searchlibrary_test.go`
- **flow_serialize*** (1): `flow_serialize_fallback_test.go`
- **flow_snapshot*** (1): `flow_snapshot_budget_test.go`
- **flow_snapshots*** (3): `flow_snapshots_retention_test.go`, `flow_snapshots_store_test.go`, `flow_snapshots_test.go`
- **flow_upload*** (3): `flow_upload_cache_test.go`, `flow_upload_id_test.go`, `flow_upload_ids_redis_test.go`
- **governance*** (1): `governance_alerts_test.go`
- **health*** (1): `health_test.go`
- **identity*** (1): `identity_test.go`
- **library*** (4): `library_dashboard_notify_test.go`, `library_page_memberships_test.go`, `library_test.go`, `library_versions_test.go`
- **loadallfromfolder*** (1): `loadallfromfolder_test.go`
- **metrics*** (1): `metrics_test.go`
- **notifier*** (1): `notifier_test.go`
- **org_rules*** (2): `org_rules_schema_test.go`, `org_rules_test.go`
- **pathguard*** (1): `pathguard_test.go`
- **policy*** (1): `policy_test.go`
- **portfolio*** (1): `portfolio_test.go`
- **provider_policy*** (2): `provider_policy_test.go`, `provider_policy_unavailable_test.go`
- **providers*** (3): `providers_budget_test.go`, `providers_catalog_test.go`, `providers_test.go`
- **providers_azure*** (2): `providers_azure_registry_test.go`, `providers_azure_test.go`
- **providers_connection*** (1): `providers_connection_entra_test.go`
- **providers_model*** (1): `providers_model_cache_test.go`
- **providers_org*** (3): `providers_org_lender_test.go`, `providers_org_lifecycle_test.go`, `providers_org_test.go`
- **readsourcefiles*** (1): `readsourcefiles_test.go`
- **realtime*** (1): `realtime_test.go`
- **remote*** (1): `remote_appliers_test.go`
- **replicated*** (1): `replicated_subscribers_test.go`
- **rule_profile*** (4): `rule_profile_reader_test.go`, `rule_profile_rls_pg_test.go`, `rule_profile_singleflight_test.go`, `rule_profile_test.go`
- **rulecatalog*** (1): `rulecatalog_test.go`
- **ruleconfig*** (1): `ruleconfig_test.go`
- **rulesummary*** (1): `rulesummary_test.go`
- **sessions*** (1): `sessions_test.go`
- **settings*** (3): `settings_backend_test.go`, `settings_bridge_test.go`, `settings_events_test.go`
- **share*** (1): `share_tokens_test.go`
- **system*** (4): `system_error_report_test.go`, `system_logerror_test.go`, `system_settings_scope_test.go`, `system_test.go`
- **triage*** (1): `triage_test.go`
- **user*** (2): `user_admin_test.go`, `user_directory_test.go`
- **webhook*** (1): `webhook_test.go`

## internal/audit

The audit trail: the closed action set, the recorder and its spill file, the writer, the org backfill.

- **actions*** (1): `actions_test.go`
- **backfill*** (1): `backfill_test.go`
- **recorder*** (1): `recorder_test.go`
- **spill*** (2): `spill_adopt_test.go`, `spill_test.go`
- **write*** (1): `write_test.go`

## internal/capability

Deployment profiles and the capabilities derived from the config.

- **capability*** (1): `capability_test.go`

## internal/ai

AI providers: Azure Foundry/Entra, Claude, OpenAI-compat, Copilot, Gemini, local; streaming, pricing, retries, circuit breaker.

- **audited*** (12): `audited_azure_test.go`, `audited_budget_test.go`, `audited_cache_pricing_test.go`, `audited_chat_error_usage_test.go`, `audited_cost_test.go`, `audited_drain_test.go`, `audited_pricing_test.go`, `audited_served_gate_test.go`, `audited_served_model_test.go`, `audited_stream_estimate_test.go`, `audited_test.go`, `audited_window_override_test.go`
- **azure_auth*** (1): `azure_auth_errors_test.go`
- **azure_catalog*** (2): `azure_catalog_context_limit_test.go`, `azure_catalog_test.go`
- **azure_claude*** (3): `azure_claude_count_tokens_test.go`, `azure_claude_entra_cli_test.go`, `azure_claude_test.go`
- **azure_discovery*** (1): `azure_discovery_test.go`
- **azure_entra*** (1): `azure_entra_test.go`
- **azure_foundry*** (3): `azure_foundry_config_test.go`, `azure_foundry_test.go`, `azure_foundry_transport_test.go`
- **azure_listing*** (1): `azure_listing_test.go`
- **azure_probe*** (1): `azure_probe_test.go`
- **azure_registry*** (1): `azure_registry_test.go`
- **azure_seam*** (1): `azure_seam_test.go`
- **catalog*** (1): `catalog_test.go`
- **circuitbreaker*** (2): `circuitbreaker_probe_test.go`, `circuitbreaker_test.go`
- **claude_openai*** (1): `claude_openai_wire_test.go`
- **claude_replay*** (1): `claude_replay_test.go`
- **claude_request*** (1): `claude_request_test.go`
- **claude_route*** (1): `claude_route_test.go`
- **context*** (2): `context_bound_source_test.go`, `context_test.go`
- **copilot_auth*** (1): `copilot_auth_test.go`
- **copilot_factory*** (1): `copilot_factory_test.go`
- **copilot_provider*** (1): `copilot_provider_test.go`
- **embedding*** (2): `embedding_errors_test.go`, `embedding_identity_frozen_test.go`
- **embeddings*** (1): `embeddings_test.go`
- **embeddings_azure*** (1): `embeddings_azure_test.go`
- **embeddings_batch*** (1): `embeddings_batch_test.go`
- **embeddings_dims*** (2): `embeddings_dims_refusal_test.go`, `embeddings_dims_test.go`
- **entra*** (2): `entra_rejection_reason_test.go`, `entra_token_errors_test.go`
- **estimate*** (1): `estimate_memo_test.go`
- **factory*** (6): `factory_azure_connection_test.go`, `factory_org_test.go`, `factory_policy_test.go`, `factory_resilience_test.go`, `factory_test.go`, `factory_usage_org_test.go`
- **gemini*** (2): `gemini_errors_test.go`, `gemini_temperature_test.go`
- **github*** (1): `github_models_cache_test.go`
- **learned*** (2): `learned_window_repoint_test.go`, `learned_window_test.go`
- **local*** (3): `local_provider_config_test.go`, `local_provider_localhost_test.go`, `local_provider_test.go`
- **metrics*** (1): `metrics_test.go`
- **model*** (1): `model_list_cache_test.go`
- **openai_compat*** (3): `openai_compat_errors_test.go`, `openai_compat_foundry_test.go`, `openai_compat_repairs_test.go`
- **openai_sse*** (1): `openai_sse_test.go`
- **prompt*** (1): `prompt_tools_test.go`
- **provider*** (1): `provider_limits_test.go`
- **providers*** (1): `providers_test.go`
- **providers_extra*** (1): `providers_extra_test.go`
- **reasoning*** (3): `reasoning_effort_test.go`, `reasoning_floor_test.go`, `reasoning_models_test.go`
- **responses*** (1): `responses_test.go`
- **responses_foundry*** (1): `responses_foundry_test.go`
- **responses_replay*** (2): `responses_replay_fast_test.go`, `responses_replay_test.go`
- **retry*** (4): `retry_backoff_test.go`, `retry_observability_test.go`, `retry_test.go`, `retry_transport_test.go`
- **served*** (1): `served_model_test.go`
- **settings*** (1): `settings_defaults_test.go`
- **spec*** (2): `spec_hooks_test.go`, `spec_test.go`
- **spend*** (1): `spend_integrity_test.go`
- **stream*** (1): `stream_test.go`
- **stream_cache*** (1): `stream_cache_test.go`
- **stream_tools*** (1): `stream_tools_test.go`
- **tokens*** (2): `tokens_test.go`, `tokens_truncate_test.go`
- **tools*** (1): `tools_test.go`
- **tools_fixes*** (1): `tools_fixes_test.go`
- **tools_json*** (1): `tools_json_test.go`
- **tools_spec*** (1): `tools_spec_test.go`
- **tools_stage*** (1): `tools_stage_test.go`
- **tools_unsupported*** (1): `tools_unsupported_test.go`

## internal/storage/database

Postgres backend: statement shapes, RLS policies, migrations, plans, the data lifecycle (export, erasure, retention), knowledge.

- **analytics*** (1): `analytics_history_baseline_test.go`
- **audit_limit*** (1): `audit_limit_test.go`
- **audit_union*** (1): `audit_union_keyset_test.go`
- **azure*** (1): `azure_test.go`
- **azure_registry*** (2): `azure_registry_integration_test.go`, `azure_registry_service_integration_test.go`
- **batch*** (1): `batch_builders_test.go`
- **blob*** (1): `blob_cleaner_test.go`
- **create*** (1): `create_user_concurrency_test.go`
- **export*** (2): `export_completeness_test.go`, `export_sections_test.go`
- **fakesql*** (1): `fakesql_test.go`
- **field*** (1): `field_crypto_test.go`
- **flows*** (2): `flows_save_search_test.go`, `flows_semantics_test.go`
- **governance*** (1): `governance_tenancy_test.go`
- **key*** (1): `key_rotation_test.go`
- **keystore*** (2): `keystore_family_integration_test.go`, `keystore_test.go`
- **knowledge*** (8): `knowledge_documents_paging_test.go`, `knowledge_identity_test.go`, `knowledge_lease_test.go`, `knowledge_ranking_test.go`, `knowledge_reindex_pages_integration_test.go`, `knowledge_reindex_test.go`, `knowledge_sample_integration_test.go`, `knowledge_search_parity_test.go`
- **lifecycle*** (3): `lifecycle_erase_test.go`, `lifecycle_export_test.go`, `lifecycle_test.go`
- **main*** (1): `main_test.go`
- **migration*** (3): `migration_0042_test.go`, `migration_0044_test.go`, `migration_checksums_golden_test.go`
- **migration_v27*** (1): `migration_v27_test.go`
- **migration_v28*** (1): `migration_v28_test.go`
- **migration_v29*** (1): `migration_v29_test.go`
- **migrations*** (1): `migrations_sql_test.go`
- **org*** (3): `org_members_list_test.go`, `org_provider_keys_cas_integration_test.go`, `org_provider_keys_integration_test.go`
- **orgcustomrules*** (1): `orgcustomrules_rls_test.go`
- **perf_indexes*** (1): `perf_indexes_v30_test.go`
- **perf_round4*** (1): `perf_round4_plan_helpers_test.go`
- **perf_statements*** (1): `perf_statements_test.go`
- **plans_audit*** (1): `plans_audit_knowledge_test.go`
- **plans_flows*** (1): `plans_flows_test.go`
- **policy*** (2): `policy_timestamps_test.go`, `policy_waivers_test.go`
- **postgres*** (35): `postgres_analytics_e2e_test.go`, `postgres_audit_backfill_test.go`, `postgres_audit_test.go`, `postgres_azure_registry_test.go`, `postgres_baselines_batch_test.go`, `postgres_blacklist_test.go`, `postgres_blob_e2e_test.go`, `postgres_blob_test.go`, `postgres_ci_credentials_test.go`, `postgres_ci_deliveries_test.go`, `postgres_dashboard_e2e_test.go`, `postgres_flow_snapshots_test.go`, `postgres_flowversion_test.go`, `postgres_idempotency_test.go`, `postgres_lock_lease_test.go`, `postgres_lock_release_test.go`, `postgres_lock_test.go`, `postgres_migrations_checksum_test.go`, `postgres_migrations_compat_test.go`, `postgres_migrations_down_test.go`, `postgres_migrations_guard_test.go`, `postgres_migrations_lock_test.go`, `postgres_migrations_test.go`, `postgres_migrations_v25_test.go`, `postgres_orgchannels_test.go`, `postgres_orgs_pgvector_test.go`, `postgres_orgs_test.go`, `postgres_pgvector_integration_test.go`, `postgres_retention_coverage_test.go`, `postgres_retention_test.go`, `postgres_statement_timeout_test.go`, `postgres_storage_boot_test.go`, `postgres_storage_test.go`, `postgres_users_test.go`, `postgres_vector_index_test.go`
- **retention_purge*** (1): `retention_purge_batches_test.go`
- **rls*** (2): `rls_hook_runner_test.go`, `rls_test.go`
- **rls_delegation*** (1): `rls_delegation_test.go`
- **rls_knowledge*** (1): `rls_knowledge_chunks_test.go`
- **rls_lazy*** (2): `rls_lazy_cancel_test.go`, `rls_lazy_test.go`
- **rls_readfilter*** (1): `rls_readfilter_test.go`
- **rls_role*** (1): `rls_role_test.go`
- **rls_scoped*** (1): `rls_scoped_methods_test.go`
- **rls_second*** (1): `rls_second_conn_test.go`
- **rule_sets*** (1): `rule_sets_test.go`
- **settings*** (2): `settings_sections_test.go`, `settings_tx_test.go`
- **sqlargs*** (1): `sqlargs_test.go`
- **sslmode*** (1): `sslmode_test.go`
- **usage*** (1): `usage_served_model_integration_test.go`
- **user*** (1): `user_settings_defaults_test.go`
- **withtx*** (2): `withtx_hooks_test.go`, `withtx_variants_test.go`

## internal/storage/filesystem

The read-only legacy source the migrator reads, on raw files laid out as old builds wrote them: the contract suite
`contract.RunLegacySourceSuite`, the keyset walk (every flow once, changes during the walk, determinism), external
edits, the full flow source, path segments.

- **legacy*** (1): `legacy_source_test.go`
- **listing*** (1): `listing_bench_test.go`
- **pagination*** (1): `pagination_stability_test.go`
- **pathsafety*** (1): `pathsafety_test.go`

## internal/storage/atomicfile

The retry predicates of the atomic rename and the tolerant read (Windows sharing violations), which the Azure registry
file and the legacy source use.

- **sharing*** (2): `sharing_other_test.go`, `sharing_windows_test.go`

## internal/storage (root)

Storage contracts shared across backends: settings, Azure registry files, keyring.

- **azure_registry*** (1): `azure_registry_file_test.go`
- **keyring*** (1): `keyring_test.go`
- **recent*** (1): `recent_test.go`
- **settings*** (2): `settings_sections_test.go`, `settings_test.go`

## internal/storage/interfaces

Storage interface definitions (doc-level).

- **tags*** (1): `tags_test.go`

## internal/storage/settingsdoc

The settings document stored as sections.

- **sections*** (1): `sections_test.go`
- **settingsdoc*** (1): `settingsdoc_test.go`

## internal/rag

RAG: document ingestion, embedding, search cache, reindex lifecycle, the embedding identity on chunks (ADR 0027:
identity-filtered search and re-index paging, the legacy stamp, the empty-result metrics, the chunker geometry
golden `testdata/chunker_v1.golden`), the on-demand background re-embed job and the org lock of an upload or
re-index across replicas (lease key space, lease released on error/cancel/loss, per-replica cap).

- **adddoc*** (1): `adddoc_atomicity_test.go`
- **azure_embedder*** (1): `azure_embedder_test.go`
- **azure_refusal*** (1): `azure_refusal_policy_test.go`
- **budget*** (1): `budget_test.go`
- **identity*** (1): `identity_test.go`
- **knowledge*** (2): `knowledge_lock_key_test.go`, `knowledge_lock_test.go`
- **lifecycle*** (1): `lifecycle_test.go`
- **model*** (1): `model_guard_test.go`
- **org*** (1): `org_embedder_test.go`
- **overlap*** (1): `overlap_strip_test.go`
- **policy*** (1): `policy_org_test.go`
- **reembed*** (2): `reembed_credential_test.go`, `reembed_job_test.go`
- **reindex*** (2): `reindex_paging_test.go`, `reindex_resume_test.go`
- **replicated*** (1): `replicated_subscribers_test.go`
- **search_budget*** (1): `search_budget_test.go`
- **search_cache*** (1): `search_cache_test.go`
- **search_scrub*** (1): `search_scrub_test.go`
- **service*** (1): `service_test.go`

## internal/websocket

WebSocket hub: presence, heartbeat, backplane, write authz, inbound limiting, access rechecks on access changes.

- **backplane*** (4): `backplane_compat_test.go`, `backplane_fake_test.go`, `backplane_redis_test.go`, `backplane_warn_test.go`
- **handler*** (2): `handler_drain_test.go`, `handler_test.go`
- **heartbeat*** (1): `heartbeat_test.go`
- **hub*** (2): `hub_stress_test.go`, `hub_test.go`
- **inbound*** (2): `inbound_limiter_test.go`, `inbound_needs_test.go`
- **presence*** (2): `presence_test.go`, `presence_writer_test.go`
- **recheck*** (2): `recheck_replicas_test.go`, `recheck_test.go`
- **write*** (1): `write_authz_test.go`

## internal/auth

JWT, API tokens, blacklist caching.

- **actor*** (2): `actor_integration_test.go`, `actor_test.go`
- **apitoken*** (2): `apitoken_scope_test.go`, `apitoken_test.go`
- **auth*** (1): `auth_test.go`
- **cached*** (2): `cached_blacklist_compat_test.go`, `cached_blacklist_test.go`
- **middleware*** (1): `middleware_test.go`
- **password*** (1): `password_cost_test.go`
- **sso*** (1): `sso_flow_test.go`
- **wsticket*** (1): `wsticket_test.go`

## internal/config

Configuration loading and validation: `config.Load` (its sources, the instance id, the drain delay, detached slices,
the healthcheck port), `config.Duration`, the duration ranges, a config file of the pre-`Duration` format
(`testdata/config_pre5a.json`), the secret fields (`secret.Value`: no rendering of the configuration shows one, the
list of secret fields is complete, `SecretEphemeral` cannot come from a file).

- **channels*** (1): `channels_test.go`
- **deploy*** (3): `deploy_passthrough_test.go`, `deploy_pool_test.go`, `deploy_replicas_test.go`
- **duration*** (1): `duration_test.go`
- **encryption*** (1): `encryption_key_previous_test.go`
- **env*** (1): `env_example_test.go`
- **knowledge*** (1): `knowledge_lease_cap_test.go`
- **load*** (1): `load_test.go`
- **loader*** (4): `loader_azure_hosts_test.go`, `loader_ci_webhook_test.go`, `loader_notify_hosts_test.go`, `loader_test.go`
- **resolve*** (1): `resolve_secrets_test.go`
- **secrets*** (1): `secrets_test.go`
- **statement*** (1): `statement_timeout_test.go`

## internal/di

Dependency wiring: service construction, AST cache budget, the declared-cache golden, the event-subscriber golden
(`testdata/subscribers.golden`: every subscriber, replicated or local), deployment custom rules, the
Azure AI Foundry endpoints' egress policy by profile.

- **analysis*** (1): `analysis_decorate_test.go`
- **analyzer*** (1): `analyzer_wiring_test.go`
- **azure_endpoint*** (1): `azure_endpoint_policy_test.go`
- **azure_registry*** (1): `azure_registry_test.go`
- **budget*** (1): `budget_ledger_test.go`
- **cache*** (1): `cache_registry_test.go`
- **catalog*** (1): `catalog_library_test.go`
- **channels*** (1): `channels_allowed_hosts_test.go`
- **deployment*** (1): `deployment_custom_rules_test.go`
- **di*** (1): `di_test.go`
- **embedding*** (1): `embedding_test.go`
- **event_manager*** (1): `event_manager_test.go`
- **events*** (2): `events_accounts_test.go`, `events_flow_test.go`
- **factory_budget*** (1): `factory_budget_test.go`
- **flow*** (2): `flow_checker_test.go`, `flow_snapshots_wiring_test.go`
- **invalidation*** (1): `invalidation_replicas_test.go`
- **knowledge*** (1): `knowledge_budget_test.go`
- **main*** (1): `main_test.go`
- **org*** (1): `org_keys_test.go`
- **provide*** (1): `provide_ast_cache_budget_test.go`
- **redis*** (1): `redis_registry_test.go`
- **setters*** (1): `setters_test.go`
- **subscribers*** (1): `subscribers_test.go`
- **two_replicas*** (1): `two_replicas_test.go`
- **wiring*** (1): `wiring_test.go`

## internal/egress

Outbound-egress guard: allowlists, private-IP blocking, proxy verdicts, transports.

- **deployment*** (1): `deployment_test.go`
- **egress*** (4): `egress_describe_test.go`, `egress_policy_test.go`, `egress_proxy_verdict_test.go`, `egress_test.go`

## internal/scanner

Repo scanner: sweep cost, fan-out, the scan job (fail-closed lock, a panic keeps the schedule, Stop ends an admin
sweep), rule sets (drift split, like-for-like regression, new-rule notices), Stop's one budget for the sweep and
its deliveries.

- **scanner*** (9): `scanner_envelope_test.go`, `scanner_fanout_test.go`, `scanner_filepath_test.go`, `scanner_lock_test.go`, `scanner_rules_test.go`, `scanner_stop_test.go`, `scanner_subscriptions_test.go`, `scanner_sweep_cost_test.go`, `scanner_test.go`

## internal/notify

Notification channels: the kind registry and its validation, the webhook envelope and payload goldens, email,
webhook, Slack/Teams fan-out, the email channel's pacing.

- **channels*** (1): `channels_test.go`
- **email*** (1): `email_pacing_test.go`
- **kinds*** (5): `kinds_allowed_hosts_test.go`, `kinds_org_test.go`, `kinds_teams_workflow_test.go`, `kinds_test.go`, `kinds_text_escape_test.go`
- **notify*** (2): `notify_proxy_test.go`, `notify_test.go`
- **payload*** (1): `payload_golden_test.go`

## internal/migration

Legacy-server migration over the read-only filesystem source: steps, idempotent reruns, read-only data dir, a
Postgres destination (DB-gated), validator, rerun cost.

- **migrator*** (3): `migrator_pg_test.go`, `migrator_rerun_cost_test.go`, `migrator_test.go`
- **runstate*** (1): `runstate_test.go`
- **validator*** (1): `validator_test.go`

## internal/mail

Mail templates, rendering goldens, the MIME writer, the mail runner, SMTP delivery and previews.

- **golden*** (1): `golden_test.go`
- **mail*** (1): `mail_test.go`
- **mime*** (1): `mime_test.go`
- **preview*** (1): `preview_test.go`
- **relay*** (1): `relay_cap_test.go`
- **sender*** (1): `sender_boot_test.go`
- **service*** (1): `service_test.go`
- **templates*** (1): `templates_test.go`

## internal/collaboration

Org collaboration repository.

- **membership*** (1): `membership_lookup_test.go`
- **org*** (2): `org_events_test.go`, `org_test.go`
- **repository*** (1): `repository_test.go`

## internal/lifecycle

Ticker loops, bounded pools and singleton jobs (fail-closed skips, lost leases, a panic keeps the schedule, Stop and
the group drain, job state, overdue, the jobs golden and key uniqueness).

- **bounded*** (3): `bounded_group_test.go`, `bounded_test.go`, `bounded_wait_test.go`
- **singleton*** (2): `singleton_registry_test.go`, `singleton_test.go`
- **tickerloop*** (1): `tickerloop_test.go`

## internal/platform/apperr

The API's typed error: kinds, codes, statuses.

- **apperr*** (1): `apperr_test.go`

## internal/platform/drain

The draining state of the server's stop (ADR 0024): Begin, the stop's first steps, the SERVER_RESTARTING refusal.

- **drain*** (1): `drain_test.go`

## internal/platform/eventbus

The in-process domain event bus, and its cross-replica seam (Phase 5b): the Local/Replicated choice every subscriber
makes, publication of invalidation keys after the commit only, the remote entry point, the subscriber listing.

- **bus*** (1): `bus_test.go`
- **replicated*** (1): `replicated_test.go`

## internal/platform/hmacsig

The CI webhook's HMAC signature: signing, verification, the timestamp window.

- **hmacsig*** (1): `hmacsig_test.go`

## internal/platform/reqinfo

What the HTTP layers learn about a request.

- **reqinfo*** (1): `reqinfo_test.go`

## internal/platform/secret

A secret that redacts itself in logs, errors and JSON; only Reveal reads it.

- **secret*** (1): `secret_test.go`

## internal/platform/txhook

Work deferred until the transaction commits.

- **txhook*** (1): `txhook_test.go`

## internal/metrics

The Prometheus registry and its registration funnel, the metrics golden and the histogram-bucket pin, label
cardinality guards, the cache collector. The metrics themselves live beside their recorders (`metrics.go` in each
owning package).

- **cache*** (1): `cache_collector_test.go`
- **cardinality*** (1): `cardinality_test.go`
- **export*** (1): `export_test.go`
- **metrics*** (2): `metrics_golden_test.go`, `metrics_test.go`
- **pool*** (1): `pool_stats_test.go`
- **registration*** (1): `registration_test.go`
- **registry*** (1): `registry_external_test.go`

## internal/sso

SSO client.

- **client*** (1): `client_test.go`

## internal/telemetry

Telemetry error sink.

- **errsink*** (1): `errsink_test.go`
- **telemetry*** (1): `telemetry_test.go`

## internal/invalidation

The cross-replica fan-out of cache invalidations and access rechecks over Redis pub/sub ([ADR 0032](adr/0032-cache-invalidation-fanout-and-state-registry.md)),
on the `redisreplica` harness: two replicas with their own bus, cache and fan-out on one miniredis.

- **fanout*** (2): `fanout_flap_test.go`, `fanout_test.go`
- **queue*** (1): `queue_test.go`

## internal/redisx

Redis client, pub/sub, the payload envelope, the key registry and the health ([ADR 0029](adr/0029-redis-envelope-key-registry-and-health.md)). Previous-release payloads: `testdata/redis/prev/<release>/`.

- **command*** (3): `command_auth_test.go`, `command_export_test.go`, `command_test.go`
- **envelope*** (1): `envelope_test.go`
- **fixtures*** (1): `fixtures_test.go`
- **hardening*** (1): `hardening_test.go`
- **health*** (1): `health_test.go`
- **inflight*** (1): `inflight_test.go`
- **pubsub*** (1): `pubsub_test.go`
- **redisx*** (2): `redisx_options_test.go`, `redisx_test.go`
- **registry*** (1): `registry_test.go`
- **sse*** (1): `sse_test.go`

## internal/errreport

Error aggregation reporting.

- **report*** (2): `report_instance_test.go`, `report_test.go`

## internal/testutil

Shared test fakes/fixtures (regular package imported by 130+ test files).

- **fake*** (2): `fake_azure_registry_test.go`, `fake_idempotency_test.go`
- **fakebackend*** (6): `fakebackend_cicredentials_test.go`, `fakebackend_cideliveries_test.go`, `fakebackend_contract_test.go`, `fakebackend_flow_snapshots_test.go`, `fakebackend_test.go`, `fakebackend_usage_test.go`

The conversation-version scenarios (compare-and-swap save and delete, no ABA after delete and re-create, a blind save invalidating a held version, ADR 0031) are `internal/storage/contract/conversations_version.go`, run by `TestFakeBackend_Contract` here and `TestPostgres_Contract` in `internal/storage/database`.

## internal/testutil/redisreplica

The two-replica harness (ADR 0029): N replicas sharing one miniredis, each with its own instance id, client and health.

- **redisreplica*** (1): `redisreplica_test.go`

## internal/archtest

Architecture ratchets over the source of both modules: layering, storage type assertions, store handles, the
cross-tenant system reads, declared caches, no write to the configuration outside `internal/config`, provider ids
spelled only in their specs (see [architecture/safety-nets.md](architecture/safety-nets.md)).

- **archtest*** (1): `archtest_test.go`
- **caches*** (1): `caches_test.go`
- **channels*** (1): `channels_test.go`
- **config*** (1): `config_test.go`
- **providers*** (1): `providers_test.go`
- **redis*** (1): `redis_test.go`
- **state*** (1): `state_test.go`
- **storageassert*** (1): `storageassert_test.go`
- **storehandles*** (1): `storehandles_test.go`
- **systemreads*** (1): `systemreads_test.go`

## core/analyzer (pad-core)

Analyzer engine: rules and their metadata, rule kinds and lazy facts, dataflow, autofix, cache, suppression,
incremental, gates and rule sets, library rules, the findings goldens.

- **apply_fixes*** (5): `apply_fixes_bench_test.go`, `apply_fixes_ctx_test.go`, `apply_fixes_options_test.go`, `apply_fixes_partial_test.go`, `apply_fixes_stall_test.go`
- **autofix*** (1): `autofix_test.go`
- **autofix_extra*** (1): `autofix_extra_test.go`
- **autofix_indent*** (1): `autofix_indent_test.go`
- **autofix_replace*** (1): `autofix_replace_anchor_test.go`
- **autofix_retry*** (1): `autofix_retry_test.go`
- **autofix_roundtrip*** (1): `autofix_roundtrip_gaps_test.go`
- **autofix_security*** (1): `autofix_security_test.go`
- **batch*** (1): `batch_test.go`
- **block*** (2): `block_handler_containment_test.go`, `block_patterns_test.go`
- **cache*** (12): `cache_bench_test.go`, `cache_budget_test.go`, `cache_cancel_test.go`, `cache_collision_test.go`, `cache_custom_test.go`, `cache_flowhash_test.go`, `cache_helpers_test.go`, `cache_identity_test.go`, `cache_lru_test.go`, `cache_ruleset_test.go`, `cache_tenant_test.go`, `cache_test.go`
- **catalog*** (1): `catalog_kind_test.go`
- **custom*** (3): `custom_rule_schema_test.go`, `custom_rule_severity_test.go`, `custom_rules_loader_test.go`
- **dashboard*** (1): `dashboard_test.go`
- **dataflow*** (2): `dataflow_limits_test.go`, `dataflow_test.go`
- **dedup*** (1): `dedup_test.go`
- **determinism*** (1): `determinism_test.go`
- **engine*** (1): `engine_test.go`
- **engine_accessor*** (1): `engine_accessor_test.go`
- **engine_error*** (1): `engine_error_handler_test.go`
- **engine_flowrule*** (1): `engine_flowrule_test.go`
- **engine_index*** (1): `engine_index_context_test.go`
- **engine_progress*** (1): `engine_progress_test.go`
- **findings*** (1): `findings_golden_test.go`
- **fingerprint*** (1): `fingerprint_test.go`
- **fix*** (1): `fix_dispatch_test.go`
- **flowcompare*** (1): `flowcompare_test.go`
- **flowrule*** (1): `flowrule_oracle_test.go`
- **fuzz*** (1): `fuzz_test.go`
- **gate*** (1): `gate_test.go`
- **history*** (1): `history_test.go`
- **incremental*** (1): `incremental_test.go`
- **infinite*** (1): `infinite_loop_taxonomy_test.go`
- **lazy*** (1): `lazy_facts_test.go`
- **library*** (3): `library_bench_test.go`, `library_rule_duplicate_subflow_test.go`, `library_test.go`
- **loop*** (1): `loop_rules_test.go`
- **meta*** (1): `meta_test.go`
- **metrics*** (2): `metrics_reuse_test.go`, `metrics_test.go`
- **onboarding*** (1): `onboarding_sample_test.go`
- **parallel*** (1): `parallel_dedup_compare_test.go`
- **parse*** (1): `parse_error_cap_test.go`
- **perf_adversarial*** (1): `perf_adversarial_test.go`
- **perf_measure*** (1): `perf_measure_test.go`
- **policy*** (1): `policy_test.go`
- **profile*** (1): `profile_digest_test.go`
- **registry*** (2): `registry_order_test.go`, `registry_pin_test.go`
- **related*** (1): `related_findings_test.go`
- **report*** (2): `report_groups_test.go`, `report_retained_test.go`
- **rule_dead*** (1): `rule_dead_data_test.go`
- **rule_goto*** (1): `rule_goto_antipattern_test.go`
- **rule_hardcoded*** (2): `rule_hardcoded_credential_test.go`, `rule_hardcoded_filepath_test.go`
- **rule_infinite*** (1): `rule_infinite_loop_test.go`
- **rule_option*** (1): `rule_option_test.go`
- **rule_resource*** (1): `rule_resource_leak_test.go`
- **rule_security*** (1): `rule_security_new_test.go`
- **rule_sql*** (1): `rule_sql_injection_taint_test.go`
- **rule_subflow*** (2): `rule_subflow_mismatch_autofix_test.go`, `rule_subflow_mismatch_test.go`
- **rule_tainted*** (1): `rule_tainted_sink_test.go`
- **rule_uninitialized*** (1): `rule_uninitialized_variable_test.go`
- **rules*** (8): `rules_extra_test.go`, `rules_new_rules2_test.go`, `rules_new_rules3_test.go`, `rules_new_rules_test.go`, `rules_new_test.go`, `rules_reliability_test.go`, `rules_subflow_test.go`, `rules_test.go`
- **ruleset*** (1): `ruleset_test.go`
- **serialize*** (1): `serialize_findings_test.go`
- **source*** (1): `source_retention_test.go`
- **subflow*** (1): `subflow_fix_test.go`
- **suppress*** (5): `suppress_failopen_test.go`, `suppress_inventory_skip_test.go`, `suppress_inventory_test.go`, `suppress_scope_test.go`, `suppress_test.go`
- **unsafe*** (1): `unsafe_fixes_test.go`
- **unterminated*** (1): `unterminated_span_test.go`
- **walk*** (1): `walk_bench_test.go`

## core/parser (pad-core)

PAD language: tokenizer, parser, serializer, diff, grammar, the folder walker.

- **classifier*** (1): `classifier_test.go`
- **column*** (1): `column_test.go`
- **diff*** (1): `diff_test.go`
- **elseif*** (1): `elseif_test.go`
- **extractors*** (3): `extractors_edge_test.go`, `extractors_linear_test.go`, `extractors_test.go`
- **folder*** (1): `folder_test.go`
- **grammar*** (1): `grammar_test.go`
- **parser*** (1): `parser_test.go`
- **progress*** (1): `progress_test.go`
- **serialize*** (1): `serialize_test.go`
- **tokenizer*** (1): `tokenizer_test.go`

## core/models (pad-core)

Domain model types.

- **analysis*** (1): `analysis_test.go`
- **chat_conversation*** (1): `chat_conversation_key_test.go`
- **library*** (1): `library_test.go`
- **models*** (1): `models_test.go`
- **rebuild*** (1): `rebuild_indexes_test.go`

## core/export (pad-core)

Report export: the format registry; JSON, SARIF, JUnit, CSV, HTML, Markdown, PDF; byte goldens and preview findings.

- **ci*** (1): `ci_context_test.go`
- **csv*** (1): `csv_injection_test.go`
- **export*** (1): `export_golden_test.go`
- **html*** (1): `html_test.go`
- **junit*** (1): `junit_csv_test.go`
- **markdown*** (1): `markdown_test.go`
- **pdf*** (2): `pdf_compress_test.go`, `pdf_test.go`
- **preview*** (1): `preview_test.go`
- **registry*** (1): `registry_test.go`
- **sarif*** (2): `sarif_test.go`, `sarif_uri_test.go`

## core/search (pad-core)

Search index: tokenization, retention, equivalence.

- **concurrent*** (1): `concurrent_test.go`
- **retention*** (1): `retention_test.go`
- **retention_index*** (1): `retention_index_test.go`
- **search*** (1): `search_test.go`
- **search_equivalence*** (1): `search_equivalence_test.go`
- **tokenize*** (2): `tokenize_diff_test.go`, `tokenize_test.go`

## core/ai/scrubber (pad-core)

Secret scrubbing: batch and streaming, anchor holds, leak guards.

- **scrubber*** (2): `scrubber_prefilter_test.go`, `scrubber_test.go`
- **source*** (1): `source_test.go`
- **stream*** (1): `stream_test.go`
- **stream_bench*** (1): `stream_bench_test.go`
- **stream_leak*** (1): `stream_leak_test.go`
- **stream_utf8*** (1): `stream_utf8_test.go`

## core/cache (pad-core)

Sized LRU cache and declared caches: specs, typed key parts (fuzzed), typed storage and fills.

- **key*** (1): `key_fuzz_test.go`
- **lru*** (1): `lru_test.go`
- **part*** (1): `part_test.go`
- **sized_lru*** (1): `sized_lru_test.go`
- **spec*** (1): `spec_test.go`
- **typed*** (1): `typed_test.go`

## core/logger (pad-core)

Logging and rotation.

- **context*** (1): `context_test.go`
- **logger*** (1): `logger_test.go`

