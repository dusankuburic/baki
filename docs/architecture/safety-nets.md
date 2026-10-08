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

## Tenant scoping: `internal/storage/database/rls_scoped_methods_test.go`

`TestRLS_TablesWithoutRowSecurityAreListed` fails on a table that has neither row-level security nor a listed
reason. It is a catalog query, so it runs in CI as well. The `TestRLS_ScopedMethods_*` tests open a stranger's RLS
scope and prove that the id-only methods reach none of another tenant's rows. They need an unprivileged role.

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

## App graph: `TestAppGraph`

`fx.ValidateApp` checks that every provider and invoke in `appOptions()` (main.go) has its dependencies,
without running anything. A missing provider used to surface only when the binary booted.
