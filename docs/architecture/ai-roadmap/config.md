# AI roadmap: Configuration and credentials

Items of category `config` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-config-5

**Resilience knobs are unbounded: PAD_RETRY_MAX_ATTEMPTS and PAD_CB_FAILURES accept any integer, the durations have no ceiling, and none is documented outside .env.example** · tier 2

**Problem.** `PAD_CB_FAILURES` and `PAD_RETRY_MAX_ATTEMPTS` are bound with `intEnv` and never range-checked (`internal/config/loader.go:418`, `420`). The fields are bare ints with defaults 5 and 3 (`config.go:383-385`, `438-441`), and grep finds no other validation site. `validateDurations` checks `PAD_CB_OPEN_DURATION` and `PAD_RETRY_BASE_DELAY` for positivity only, with `max=0` (`loader.go:788-789`). `ResilienceConfig.withDefaults` silently replaces values <= 0 and passes any positive value through (`internal/ai/factory.go:61-75`). `cappedBackoff` honours a base delay above the 30 s ceiling (`retry.go:53-59`). `retry.go:18-21` documents that attempts are "operator-configurable with no bound"; the 30 s cap exists only to survive the resulting overflow. The effects of bad values:
- `PAD_RETRY_MAX_ATTEMPTS=50` turns every provider outage into up to 49 retries per call, each up to 30 s. That inflates `pad_ai_retry_attempts_total` and holds a stream's 10 min wall budget in backoff; `retry_observability_test.go:28-29` names this misconfiguration.
- `PAD_CB_FAILURES=1` trips the shared circuit on any single 5xx, and 10000 silently disables the breaker.
- `PAD_RETRY_BASE_DELAY=10m` parks chat turns past the idle watchdog.

The only loader test rejects a non-integer, not an out-of-range value (`loader_test.go:307-308`). The only operator-facing description is `.env.example:490-497`; grep over `docs/` for `PAD_CB_` or `PAD_RETRY_` finds nothing.

**Evidence.**
- `internal/config/loader.go:418` — `intEnv` bindings for `PAD_CB_FAILURES` (418) and `PAD_RETRY_MAX_ATTEMPTS` (420) with no range check; fields and defaults at `config.go:383-385`, `438-441`
- `internal/config/loader.go:788` — `validateDurations` rows for `PAD_CB_OPEN_DURATION` and `PAD_RETRY_BASE_DELAY`: `positive=true`, `max=0` (no ceiling); there is no integer counterpart
- `internal/ai/factory.go:61` — `withDefaults` replaces only Threshold/OpenFor/Attempts/BaseDelay <= 0 (62-73); any positive value passes through
- `internal/ai/retry.go:18` — "retryMaxAttempts is operator-configurable with no bound" is the overflow guard; `cappedBackoff` (53-59) uses `max(retryMaxDelay, baseDelay)`, so a huge base is honoured
- `internal/config/loader_test.go:307` — the only `PAD_CB_FAILURES` / `PAD_RETRY_MAX_ATTEMPTS` tests reject a non-integer, not a range (307-308); `retry_observability_test.go:28-29` names the unbounded misconfiguration
- `.env.example:490` — the only operator-facing description (490-497); grep over `docs/` for `PAD_CB_` or `PAD_RETRY_` finds nothing

**Change.** Add a `validateInts` table beside `validateDurations`, which already names both the env var and the config field. Bound `PAD_RETRY_MAX_ATTEMPTS` to [1, 10] and `PAD_CB_FAILURES` to [1, 100]. The error names the env var and the config field, and the check applies to both env and config-file sources. Add ceilings through `durationMaxReasons`: `PAD_RETRY_BASE_DELAY` <= 30 s and `PAD_CB_OPEN_DURATION` <= 10 min. Document all four knobs with their defaults, bounds and the retry/breaker semantics in `docs/architecture/operations.md` and `.env.example`. This only bites under explicit operator misconfiguration, and the defaults are sane. Backoff also partly limits itself. It gives up with the real error when the caller's deadline cannot cover the wait (`retry.go:86-88`). A Retry-After above 20 s is reported at once (`retry.go:78-82`). Tier 2 or 3.

**Touches.** `internal/config/loader.go`, `internal/config/config.go`, `internal/config/loader_test.go`, `internal/config/load_test.go`, `docs/architecture/operations.md`, `.env.example`

**Done when.**
- Config load fails with a message naming the variable and field for each of: `PAD_RETRY_MAX_ATTEMPTS=0` or `=11`, `PAD_CB_FAILURES=0` or `=101`, `PAD_RETRY_BASE_DELAY=31s`, `PAD_CB_OPEN_DURATION=11m`. A config file with `{"Runtime":{"RetryMaxAttempts":0}}` fails the same way.
- The existing defaults (3 attempts, 5 failures, 500ms, 30s) still boot, and `TestProviderFactory_ResilienceIsAppliedToEveryChain` is unchanged.
- `docs/architecture/operations.md` contains each knob's name, default and bound, checked by a doc test or a grep in CI.

### R-config-6

**Package-init env reads (PAD_AI_MAX_CONNS_PER_HOST, *_API_URL) bypass config validation, are pinned archtest exceptions and are undocumented** · tier 3

**Problem.** `internal/ai/httpclient.go` reads `PAD_AI_MAX_CONNS_PER_HOST` at var init and silently falls back to 256 when the value is not an integer or is negative (`httpclient.go:17-23`). `providerURL` in `internal/ai/providerconfig.go` reads eight provider base-URL variables at package init and validates none of them as URLs (`providerconfig.go:12-17`). Seven are listed in its comment (`CLAUDE_API_URL`, `OPENAI_API_URL`, `GEMINI_API_URL`, `GLM_API_URL`, `XAI_API_URL`, `GITHUB_MODELS_API_URL`, `COPILOT_API_URL`; `providerconfig.go:10-11`); the eighth, `COPILOT_MODELS_API_URL` (`internal/ai/copilot.go:13`), is missing from it. Both files are pinned `os.Getenv` exceptions in the archtest env ratchet (`internal/archtest/archtest_test.go:402-405`), and neither variable is bound next to the Runtime resilience knobs (`internal/config/loader.go:417-426`). Neither appears in `.env.example`, where a grep finds no `API_URL` or `MAX_CONNS_PER_HOST` entry. In `docs/`, the only hits are `VITE_API_URL` (`docs/deployment.md:66`) and a design note in ADR 0025 that the fixed-host `*_API_URL` overrides bypass every egress guard (`docs/adr/0025-provider-specs.md:21`); neither is operator documentation of these knobs. The variables are therefore invisible to the config dump path the rest of Runtime goes through. An operator who redirects a provider to a proxy, which is the documented purpose, has no documented knob, and a typo goes unreported.

**Evidence.**
- `internal/ai/httpclient.go:17` — `maxConnsPerHostFromEnv`: a strconv error or `n < 0` silently returns 256 (18-23)
- `internal/ai/providerconfig.go:12` — `providerURL` reads `envKey` at package init with no validation (12-17); the comment lists seven variables (10-11)
- `internal/ai/copilot.go:13` — `COPILOT_MODELS_API_URL`, the eighth variable, missing from the comment
- `internal/archtest/archtest_test.go:402` — both files pinned as `os.Getenv` exceptions (403-404)
- `internal/config/loader.go:417` — the Runtime resilience knobs are bound here (417-426); the two AI env reads are not
- `.env.example:490` — documents `PAD_CB_*`/`PAD_RETRY_*` (490-497) but has no `API_URL` or `MAX_CONNS_PER_HOST` entry
- `docs/adr/0025-provider-specs.md:21` — "fixed-host `*_API_URL` overrides bypass every egress guard (operator-controlled)"
- `internal/di/services.go:157` — `ai.ConfigureProviders(ai.ProviderOptions{…})` already exists at boot (157-161); the type is at `internal/ai/spec.go:348`

**Change.** Tier 3. Move both reads into `config.Runtime`: `AIMaxConnsPerHost int` with a range check, and `AIProviderBaseURLs map[string]string` covering all eight variables, each validated as an absolute http(s) URL. Do not add an egress-policy check. ADR 0025 records the bypass as deliberate (`docs/adr/0025-provider-specs.md:21`), so an egress check would be a policy change, to be decided explicitly by amending ADR 0025. Pass the values through the existing `ai.ProviderOptions` (`internal/ai/spec.go:348`, set up at `internal/di/services.go:157-161`), and have provider constructors read the override from the options instead of a package var. Reduce the two archtest pins to an empty map. Document the knobs in `.env.example`, `docs/architecture/operations.md` and `docs/architecture/ai-providers.md`.

**Touches.** `internal/ai/httpclient.go`, `internal/ai/providerconfig.go`, `internal/ai/spec.go`, `internal/config/config.go`, `internal/config/loader.go`, `internal/di/services.go`, `internal/archtest/archtest_test.go`, `.env.example`, `docs/architecture/operations.md`, `docs/architecture/ai-providers.md`

**Done when.**
- `TestArch_EnvReadsOnlyInConfig` passes with an empty pin map.
- `PAD_AI_MAX_CONNS_PER_HOST=abc` fails config load and names the variable; `=0` disables the cap; the transport's `MaxConnsPerHost` reflects the configured value.
- `OPENAI_API_URL=ftp://x` or a non-absolute URL fails config load; a valid override is used by the OpenAI provider in an httptest-backed test.
