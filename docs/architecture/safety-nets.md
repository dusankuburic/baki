# Test safety nets

These tests exist so the refactoring phases can change structure without changing behaviour. A behaviour
change then has to be deliberate, and it shows up as a reviewable diff.

## Route policy golden: `internal/api/testdata/route_policy.golden`

`TestRoutePolicy_Golden` records, for every registered route and the spellings that have caused bypasses
before (`/api/v1` alias, trailing slash, dot segment, encoded `/`, HEAD/OPTIONS, non-route paths):

- the route chi dispatches to;
- whether `jwtAuth` authenticates the request;
- the minimal scope sets an API token needs;
- the rate-limit bucket;
- the RLS mode;
- the deadline class;
- the metrics label.

Each value comes from the function the middleware itself calls. Refresh it only for an intended change:

```
go test ./internal/api/ -run TestRoutePolicy_Golden -update-route-policy
```

## Authorization matrix: `internal/api/testdata/authz_matrix.golden`

`TestAuthzMatrix_Golden` sends a representative request of every route group as anonymous, a viewer, an owner,
an org outsider, a global admin and scoped API tokens, and records each status. Refresh with
`-update-authz-matrix`.

## Migration checksums: `internal/storage/database/testdata/migration_checksums.golden`

This pins every shipped step's checksum, so an edited migration fails CI instead of failing boot at deploy time.
After adding a step, refresh with `-update-migration-checksums`.

## Architecture ratchets: `internal/archtest`

These tests pin today's exceptions to the layering and convention rules in [README.md](README.md). They fail
when an exception grows and when it shrinks, so each pinned number can only go down.

## Fake backend contract: `internal/testutil/fakebackend_contract_test.go`

The storage contract (`internal/storage/contract`) now also runs against `testutil.FakeBackend`. The scenarios
where the fake still diverges are listed with their reason, and the list may only shrink. Run them anyway with
`CONTRACT_STRICT=1`.

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
