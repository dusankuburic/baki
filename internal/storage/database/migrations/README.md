# Schema migrations (v32 and later)

Versions 1–31 are Go string literals in `../postgres_migrations.go` and are frozen: never edit them, because the
boot-time checksum check refuses a database whose recorded checksum no longer matches. Every new step is a file
in this directory, embedded into the binary and appended after v31.

## Files

- `NNNN_name.up.sql`: the step, for example `0032_audit_org_id.up.sql`. The version is the 4-digit prefix and
  must continue the sequence without gaps. The name is lowercase letters, digits and `_`.
- `NNNN_name.down.sql` (optional): the reverse, used only by `bakicli migrate down`. No down file means the step
  is irreversible, so `migrate down` refuses to cross it.

Each step runs in one transaction together with its `schema_migrations` row. The checksum covers the up file's
text after stripping a UTF-8 BOM and converting CRLF to LF, so the same file hashes identically on every
checkout. `.gitattributes` also keeps `*.sql` files LF.

## Compatibility directive (required)

Every up file carries exactly one line:

```sql
-- baki:compat expand
```

or `-- baki:compat breaking`.

- **expand**: the release before this one keeps working on the new schema. Examples: new tables, new nullable
  columns, columns with a default, new indexes, new functions. The value is recorded in `schema_migrations`, and
  an older binary (one with this guard) boots against a schema that is ahead of it by expand-only steps. That
  makes an image rollback safe without a schema rollback.
- **breaking**: anything the previous release cannot run against. Examples: dropping or renaming a column or
  table, adding `NOT NULL` without a default, changing a type, RLS changes that hide rows the old code reads. An
  older binary refuses to boot until the schema is rolled back.

Prefer expand/contract over breaking steps. First ship an expand step plus code that works with both shapes.
Ship the contract step (the drop) one release later, after no deployed code reads the old shape.

## Long-running work

Each step runs with a `lock_timeout` (`SET LOCAL`, 5 s) and is retried with a growing pause when a table lock is
not available, so a step never queues the still-serving revision's queries behind it for long.

Do not put `CREATE INDEX CONCURRENTLY` or long backfills in a step. Build indexes concurrently after boot (see
`EnsureVectorIndex`), and run data backfills as resumable jobs.

## Checks

`go test ./internal/storage/database/` loads this directory and checks the following:

- the sequence has no gaps;
- every up file has its directive;
- shipped steps still match `testdata/migration_checksums.golden`.

When you add a step, refresh the golden file:

```
go test ./internal/storage/database/ -run TestMigrationChecksums_Golden -update-migration-checksums
```
