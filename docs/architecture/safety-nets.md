# Test safety nets

These tests exist so the refactoring phases can change structure without changing behaviour. A behaviour
change then has to be deliberate, and it shows up as a reviewable diff.

## Route policy goldens: `internal/api/testdata/route_policy*.golden`

Both goldens cover the same requests: every registered route, in the spellings that have caused bypasses before
(`/api/v1` alias, trailing slash, dot segment, encoded `/`, HEAD/OPTIONS), plus paths that are no route.

`TestRoutePolicy_Golden` (`route_policy.golden`) records what the legacy path tables decide:

- the route chi dispatches to;
- whether `jwtAuth` authenticates the request;
- the minimal scope sets an API token needs;
- the rate-limit bucket;
- the RLS mode;
- the deadline class;
- the metrics label (the resolved policy's label, in every mode).

`TestRoutePolicyDeclared_Golden` (`route_policy_declared.golden`) records the policy the resolver gives each
request (ADR 0006):

- whether it is a route, a 405 or no route, and its pattern;
- who may call it: `none`, `self` or `jwt`;
- the minimal scope sets;
- the bucket, the transaction, the deadline class and the label;
- a 405's `Allow` list.

`TestRoutePolicy_DeclaredMatchesLegacy` fails when a request that resolves to a route decides anything
differently from the legacy tables (the label excepted). On 405, OPTIONS and no-route lines the two differ by
design, and those differences are reviewed in the declared golden.

Each value comes from the function the middleware itself calls. Refresh only for an intended change:

```
go test ./internal/api/ -run TestRoutePolicy_Golden -update-route-policy
go test ./internal/api/ -run TestRoutePolicyDeclared_Golden -update-route-policy-declared
```

The test routers run in the `PAD_ROUTE_POLICY` mode named by `BAKI_TEST_ROUTE_POLICY` (default `shadow`), so the
whole API suite runs under the declared policy with:

```
BAKI_TEST_ROUTE_POLICY=declarative go test ./internal/api/...
```

At boot, `finishRoutes` resolves a sample path of every route and refuses to start unless each resolves to the
route's own policy. Tests that add routes after construction do it with `mountTestRoutes`, which declares their
policy the same way.

## Authorization matrix: `internal/api/testdata/authz_matrix.golden`

`TestAuthzMatrix_Golden` sends a representative request of every route group as anonymous, a viewer, an owner,
an org outsider, a global admin and scoped API tokens, and records each status. Refresh with
`-update-authz-matrix`.

## Migration checksums: `internal/storage/database/testdata/migration_checksums.golden`

This pins every shipped step's checksum, so an edited migration fails CI instead of failing boot at deploy time.
After adding a step, refresh with `-update-migration-checksums`.

## Analysis goldens: `core/analyzer/testdata/findings/*.golden`

`TestFindingsGolden` analyzes every sample flow under `core/parser/testdata/samples` (each file, and each sample
folder through `ParseFolder`), an inline flow with `pad-ignore` directives, generated large flows and two
determinism probes, under six variants: `DefaultSettings`, nil settings (`bakicli` without a config), changed
thresholds, disabled rules, severity overrides and the auto-fixable rule subset. Each records the stats, progress,
metrics, rule profiles, the suppression inventory and every finding with its severity, confidence, fix, anchor and
fingerprint. Block and subflow ids are replaced by positions, so a golden holds across re-parses; variants that
only regrade the default findings print the differences, and very large flows print digests. An intended change of
rule output, finding order, fingerprints or health is a golden diff that explains itself:

```
go test ./core/analyzer/ -run TestFindingsGolden -update-findings
```

Beside it, `TestAllRules_OrderPinned` (`registry_order_test.go`) pins the rule order, which decides finding order,
the `F-nnn` ids and the fix loop's pick order, and `registry_pin_test.go` pins each rule's `IntroducedIn`,
`Revision` and `Stage` (ADR 0014). The full-corpus golden and determinism tests skip under `-short`.

## Export goldens: `core/export/testdata/golden/*.golden`

`TestExportGolden` renders every registered format (JSON, SARIF, JUnit, CSV, HTML, Markdown, PDF) for five
reports: a flow with suppressions, a two-file folder with a policy verdict, a hand-built report without a document
(unknown rule, invalid severity, formula-like text), a clean report, and preview findings (stamped, and a rule
flipped to preview). The goldens are the exact bytes, except PDF: fpdf's test defaults make it deterministic, and
its golden is the SHA-256 of the bytes plus every page's decoded content stream. Three of the reports come from a
real analysis, so an intended analyzer change on those flows refreshes these goldens too:

```
go test ./core/export/ -run TestExportGolden -update-export-golden
```

Both golden tests compare with CR stripped (a CRLF checkout passes) and, in update mode only, delete goldens whose
input is gone; otherwise a stale golden fails.

## Mail goldens: `internal/mail/testdata/*.golden`

`TestMailGolden` renders every registered email template (ADR 0022) from its fixture and pins the subject, the
text and HTML bodies, and the whole MIME message (`.eml`, with the date, boundary and Message-ID fixed and the CRLF
line ends written as LF). The fixtures carry markup, quotes and a forged MIME boundary in the user-controlled
fields, so an escaping or encoding change shows in the diff. `TestMailGolden_Coverage` fails when a registered
template has no fixture or golden, or a golden no fixture:

```
go test ./internal/mail/ -run TestMailGolden -update-mail-golden
```

## Notification payloads: `internal/notify/testdata/payloads/*.golden`

`TestPayloadGolden` delivers one event of each type (`drift`, `health_regression`, `analysis_complete`, `test`,
`new_rules`) through every channel kind (webhook, Slack, Teams, Teams Workflows, Jira, email) built from the kind
registry, and pins what the receiver gets: the request line, the headers (the envelope's, the signatures) and the
body bytes, with a fixed clock and fixed delivery ids (ADR 0021). The webhook is rendered as an organisation's
channel, plus one deployment webhook without `orgId`. A change of what receivers get is a reviewed diff:

```
go test ./internal/notify/ -run TestPayloadGolden -update-notify-golden
```

## Declared caches: `internal/di/testdata/caches.golden`

`TestCacheRegistry_Golden` lists every declared cache (ADR 0017): its name, the key parts every key must carry,
its tenant (or the reason it is deliberately global) and where its entries live. Review a change like a
route-policy change. `TestCacheRegistry_CoversEveryDeclaration` fails when a `cache.Declare` in the source is not
linked into that test binary, so the golden cannot silently miss one:

```
go test ./internal/di/ -run TestCacheRegistry_Golden -update-caches
```

## Background jobs: `internal/lifecycle/testdata/jobs.golden`

`TestJobRegistry_Golden` lists every job declared with `lifecycle.DeclareJob` (ADR 0023): its name, its advisory-lock
key (with its ASCII spelling), whether the key is a legacy constant or derived from the name, and its schedule. A
changed legacy key lets two revisions run the job side by side during a rolling deploy, so review a change like a
route-policy change. `TestJobRegistry_CoversEveryDeclaration` fails when a `DeclareJob` in the source is not linked
into that test binary (import its package in `singleton_registry_test.go`), and `TestJobKeys_Unique` reads every
`*LockKey` constant of the source (package `main` included) and checks that the keys of the one int8 key space are
distinct:

```
go test ./internal/lifecycle/ -run TestJobRegistry_Golden -update-jobs
```

## Previous release on the new schema: the `previous-release-boot` CI job

A binary boots on a newer schema only when every newer step is marked `-- baki:compat expand`
([ADR 0002](../adr/0002-migration-compatibility-and-rollback.md)), and that is what lets an image rollback skip
the schema rollback. This job checks the promise with real binaries. `scripts/ci/previous-release-boot.sh` boots
the previous release on an empty database, then this revision on the same database (the deploy), then the
previous release again (the rollback), which must answer `/readyz` within two minutes.

- The previous release is the pull request's base commit, or for a push the commit the branch pointed at before.
- A newer step marked `breaking` fails the job, and so does a step marked `expand` that the previous release
  cannot boot on. Since `deploy.yml` gates on this workflow, a deliberately breaking step also stops the
  automatic deploy of its push. A manual `deploy.yml` run has no previous commit to compare with, so that is the
  way to ship one.
- The job passes and says why when there is nothing to check: no previous commit (a new branch or tag, a manual
  run), a previous commit that cannot be fetched or does not build, or one that predates the compat guard
  (Phase 0a) while the schema has moved past it, since such a binary refuses every newer schema.

To run it locally, put `psql` and `curl` on `PATH` and give it an empty throwaway database (recreate it before
every run):

```
docker run -d --rm --name baki-bootdb -e POSTGRES_PASSWORD=postgres -p 127.0.0.1:55433:5432 pgvector/pgvector:pg16
docker exec baki-bootdb psql -U postgres -c "CREATE ROLE baki_app LOGIN PASSWORD 'baki_app' NOSUPERUSER NOBYPASSRLS;" -c "CREATE DATABASE baki_boot OWNER baki_app;"
docker exec baki-bootdb psql -U postgres -d baki_boot -c "CREATE EXTENSION vector;"
mkdir -p /tmp/baki-previous && git archive HEAD~1 | tar -x -C /tmp/baki-previous
(cd /tmp/baki-previous && CGO_ENABLED=0 go build -o /tmp/baki-previous.bin .) && CGO_ENABLED=0 go build -o /tmp/baki-new.bin .
BOOT_DATABASE_URL="postgres://baki_app:baki_app@127.0.0.1:55433/baki_boot?sslmode=disable" bash scripts/ci/previous-release-boot.sh /tmp/baki-previous.bin /tmp/baki-new.bin
```

## Architecture ratchets: `internal/archtest`

These tests pin today's exceptions to the layering and convention rules in [README.md](README.md). They fail
when an exception grows and when it shrinks, so each pinned number can only go down.
`TestArch_SystemReadsPinned` lists the jobs that may read across tenants (ADR 0013).

`caches_test.go` scans `core/` as well (ADR 0017). It pins the caches still built without a declared spec
(`cache.NewLRUCache`, `cache.NewSizedLRU`, golang-lru constructors; only the undo ring is left) and the
`singleflight.Group`s, per file. `cache.Declare` may only be the value of a package-level var, with a literal name
unique across both modules. `undeclaredMemos` lists the memos deliberately left off the registry, each with its
reason, and `TestArch_UndeclaredMemosStillExist` checks that each is still where it says.

`config_test.go` type-checks the module, as `storehandles_test.go` does. `TestArch_ConfigNotMutatedOutsideConfig`
fails on a write to the configuration outside `internal/config`, with no exceptions: an assignment, `++`/`--`, an
op-assign, `append`/`copy`/`delete`/`clear`, a pointer-receiver method call or a taken address, on anything reached
from a `config.Config` or one of its section types (a field at any depth, a slice element, a map entry), or on a
package-level variable of `internal/config`. `config.Load` fills the configuration in once, and the providers share
it by pointer.

`providers_test.go` pins where a provider id may be spelled: `TestArch_ProviderIDLiteralsOutsideSpecs` counts
every string literal equal to a registered id (`ai.Specs()`, so a new `spec_<id>.go` is covered the moment it
registers) outside the specs, the catalogue and config's copy of the embedding-capable ids, per file, and
`TestArch_RegisterProviderOnlyInTests` keeps `ai.RegisterProvider` out of production code. A provider is
described once, in its spec; code that needs to know asks `Caps`, `Keys`, `Modes` or `Listing`.

## Tenant scoping: `internal/storage/database/rls_scoped_methods_test.go`

`TestRLS_TablesWithoutRowSecurityAreListed` fails on a table that has neither row-level security nor a listed
reason. It is a catalog query, so it runs in CI as well. The `TestRLS_ScopedMethods_*` tests open a stranger's RLS
scope and prove that the id-only methods reach none of another tenant's rows. They need an unprivileged role.

## Sealed columns: `internal/storage/database/key_rotation_test.go`

`TestReseal_CoversEverySealedColumn` reads the package's source. It fails when a `sealField` call or a keystore
statement writing `ciphertext` names a column that the re-seal pass of a key rotation does not walk
(`fieldSealedColumns`, `keystoreSealedColumns`; ADR 0005). Without the pass, such a column would become unreadable
once the previous key is removed. The `TestKeyRotation_*` tests (`DATABASE_URL`) cover the rotation itself.

## DI setters: `internal/di/setters_test.go`

`TestDISetters_Listed` lists every post-construction `Set*`/`On*` call in `internal/di` with its reason (see
[di-wiring.md](di-wiring.md)). It fails on an unlisted setter and on a listed one that is gone.
`TestServiceModule_BuildsInEveryMode` builds the services in the desktop, local+db and cloud shapes, running
the constructors that `TestAppGraph`'s type check skips.

## Fake backend contract: `internal/testutil/fakebackend_contract_test.go`

The storage contract (`internal/storage/contract`) also runs against `testutil.FakeBackend`, with no known
divergence: the fake follows Postgres on every method it implements (visibility filters, optimistic versions, what
a write stamps, defaults, ordering, `ErrNotFound`, cascades), so a test that passes on the fake holds on Postgres.
`contract/semantics.go` holds those store-semantics scenarios, `contract/lifecycle.go` the data lifecycle's (account
erasure, organisation deletion, the export) and the organisation store's; a new one must pass on Postgres too
(`go test ./internal/storage/database/ -run TestPostgres_Contract` with `DATABASE_URL`). The fake refuses rows
whose parent is missing where Postgres has a foreign key (collaborators, versions, comments, invitations, user and
org settings), so seed the user, org or flow first; a test may set stored fields directly (e.g. a timestamp a save
stamps itself) through the exported maps. No backend lists known divergences (`KnownDivergence`,
`CONTRACT_STRICT=1`) any more: the filesystem backend, the last to list them, is now only the read-only legacy
source below. The API, DI and service tests run their routers and services on the fake; a test about Postgres
behaviour itself (row-level security, transactions, statement counts) uses `pgtest`.

## Legacy source contract: `internal/storage/contract/legacy.go`

The legacy-server migrator reads a PAD_DATA_DIR through `migration.LegacySource`, implemented read-only by
`filesystem.OpenLegacySource`. `RunLegacySourceSuite` writes its fixtures as raw files in the layout old builds
left (`contract.LegacyDir`), never through a writer of this build, and includes a flow file with literal keys, so a
renamed JSON tag on `FlowDocument` fails it. It also makes the directory read-only and compares a snapshot of the
whole tree before and after a full read: the source may not create, change or remove anything. Windows ignores the
read-only bit on directories, so there only the snapshot proves it.

## Isolated test databases: `internal/testutil/pgtest`

Tests that rewrite schema state, such as migrate down, tampered checksums or future-version rows, get a database
of their own. It is cloned from a template that is migrated once per schema version. The test role needs
`CREATEDB`, which CI grants and which does not bypass RLS, plus a `baki_template` database carrying the
`vector` extension.

To run locally against a throwaway container, never the live compose database (the test refuses a database
named `baki`):

```
docker run -d --rm --name baki-testdb -e POSTGRES_PASSWORD=postgres -p 127.0.0.1:55432:5432 pgvector/pgvector:pg16
docker exec baki-testdb psql -U postgres -c "CREATE ROLE baki_app LOGIN PASSWORD 'baki_app' NOSUPERUSER NOBYPASSRLS CREATEDB;" -c "CREATE DATABASE baki_app OWNER baki_app;" -c "CREATE DATABASE baki_template OWNER baki_app;"
docker exec baki-testdb psql -U postgres -d baki_app -c "CREATE EXTENSION vector;"
docker exec baki-testdb psql -U postgres -d baki_template -c "CREATE EXTENSION vector;"
DATABASE_URL="postgres://baki_app:baki_app@127.0.0.1:55432/baki_app?sslmode=disable" go test ./internal/storage/database/
```

## Plan tests (EXPLAIN assertions)

A plan test must not depend on what earlier tests left in the shared database. The audit plan tests used to pass
only once earlier runs had churned the table, and failed on every fresh database, CI's included. They now:

- open a database of their own (`openIsolatedMigrationDB`);
- seed in random physical order, so a perfectly time-correlated heap does not make a filtered index walk look
  artificially cheap;
- `ANALYZE` before they `EXPLAIN`.

Follow the same pattern for new plan tests.

## Request-chain benchmark: `BenchmarkBuildHandler`

This sends one request through the whole production stack (`BuildHandler`) over the in-memory fake, for a
library list, a library save and the dashboard. Compare before and after a phase on the same machine:

```
go test ./internal/api/ -run '^$' -bench BenchmarkBuildHandler -benchmem -count 8 > new.txt
benchstat old.txt new.txt
```

The nightly `chain-benchmark` job in `.github/workflows/bench-fuzz.yml` does the same between the previous day
and HEAD.

## App graph and boot: `TestAppGraph`, `TestApp_StopOrder`, `TestDesktopBoot_Smoke`

`fx.ValidateApp` checks that every provider and invoke in `app.Options` (`internal/app`; the root package checks the
same graph with the build stamps `main` passes) has its dependencies, without running anything. A missing provider
used to surface only when the binary booted.

`TestApp_StopOrder` (`internal/app`) boots the real graph in a child process of the test binary and records the stop
hooks as fx runs them (`fxevent`): in the desktop profile, with the per-user directories at a temporary one, and with
`DATABASE_URL` in the cloud profile on a database of its own. It pins the order of the hooks (by the function that
registered each; see [di-wiring.md](di-wiring.md#the-application-internalapp)), the desktop handshake (the `CONFIG:`
line alone on stdout, printed while the graph is built, before any start hook), and that neither boot reaches the OS
keychain (the child's keyring fails every call, and the keyring store would log it). It boots in a child process
because the server's stop closes every `lifecycle.Bounded` runner of the process.

`TestDesktopBoot_Smoke` (root package; `-short` skips it) builds the binary and runs it as the Tauri shell does, with
the per-user directories at a temporary one: the first stdout line is `CONFIG:{"port":…,"tokenPath":…}`, the session
key is at `tokenPath`, `/healthz` answers 200, and SIGTERM (on Windows a CTRL_BREAK to its process group) stops it with
exit code 0 within the stop budget, with nothing else on stdout. `desktop.yml` runs it on Windows before the bundle
build. `TestMainPackageIsMainGoAlone` keeps `main.go` the root package's only non-test file, since the Dockerfile and
CI build `go build main.go`.
