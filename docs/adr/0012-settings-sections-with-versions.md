# 12. Settings stored per section, each with a version, dual-written with the legacy blob for one release

Date: 2026-10-08. Status: accepted.

## Context

Each settings scope (the deployment's `app_settings`, `user_settings`, `org_settings`, and the desktop's
`settings.json`) stored the whole `AppSettings` document as one JSON blob. Its `version` was always 1 and never
checked, so a change of a section's shape had no upgrade path. Every write replaced the whole document with what the
writing binary's struct knew: after a rollback, an older binary's first save deleted every field a newer one had
added. The storage copy of the struct had also drifted from the domain copy, and a cloud save dropped
`ai.perUserDailyBudget`.

## Decision

- **Sections** are the document's top-level keys: `general`, `appearance`, `layout`, `ai`, `parser`, `analysis`,
  `recentFiles`. One section per key keeps the legacy blob a per-key fallback; grouping the frontend-only sections
  would need a mapping and gain nothing. The document's own `version` stays 1 and is not a section.
- **Declarations** live in `internal/storage/settingsdoc/sections.go`, one entry per section naming the code that
  owns it, with `Version` and one `Upgrade` per version step (a function on the stored JSON, so it does not depend
  on today's Go type). They are in one leaf package rather than in each owner because the shapes are in `pad-core`,
  which cannot import the backend, and both stores need the full set without depending on import side effects.
  `TestSections_CoverTheDocument` keeps every key of both `AppSettings` types declared.
- **Version check.** A section stored at an older version is upgraded on read, step by step; the blob counts as
  `LegacyVersion` (1). A section stored at a newer version (a rollback) is read as it is: the fields this binary
  knows are decoded, and a field whose JSON type the newer version changed reads as zero instead of failing.
- **Writes merge.** A write keeps the stored section's newer version and every field this binary does not know, at
  any depth of struct fields and of struct-valued map entries; a field this binary knows follows the write (absent
  means cleared, a removed map entry stays removed). In a newer section, a known field whose stored value this
  binary cannot decode keeps the stored value. Rules for owners: never give a field another JSON type; an upgrade
  that renames a field cannot carry an older binary's later edits of it forward, so prefer adding fields.
- **Postgres** (migration v35, expand): `settings_sections (scope, scope_id, section, version, data,
  legacy_digest, updated_at)`, with generated `user_id`/`org_id` columns whose foreign keys delete a user's and an
  organisation's rows with them. No RLS, like the blob tables. The **desktop file** keeps its layout and adds a
  `sectionVersions` object; a file without it holds every section at `LegacyVersion`.
- **One release of dual write and dual read.** A save writes, in one transaction, the blob exactly as the release
  before wrote it and the merged section rows, each with `legacy_digest`, the md5 of its section as written into
  the blob. A load is one statement: a section comes from its row when the blob still matches the row's digest
  (or there is no blob, or the digest is NULL), else from the blob. A mismatch means a release that writes only
  the blob (the one before, after a rollback) changed that section since, so its change wins on the roll forward;
  the fields only the row holds are kept by the next write, which merges onto the row.

## Contract steps

1. **Next release.** A resumable boot job (under a global lock, in batches, like the audit `org_id` backfill)
   copies every blob that has no section rows into rows, and reconciles every row whose `legacy_digest` is not
   NULL and no longer matches its blob's section: a release that wrote only the blob (this one's predecessor, after
   a rollback) changed that section since, so the job rewrites the row from the blob section, merged onto the row so
   fields only the row holds survive, and sets the digest to NULL. Until the job has finished, loads keep this
   release's digest check (a mismatched row loses to the blob); skipping it would bring back the pre-rollback value.
   Saves stop writing the blob and write a NULL `legacy_digest`, which this release reads as "the row is current",
   so rolling back to this release stays safe. Loads read the rows and fall back to the blob only for a section
   without a row. From this release on, a section may raise its version.
2. **The release after**, once the job has finished everywhere (every blob has rows and no non-NULL
   `legacy_digest` remains): a `breaking` step drops the blob tables and `legacy_digest`.

## Consequences

A settings save is a transaction of three statements instead of one upsert, and a load reads up to eight rows and
hashes each section of the blob; both are small next to a request. The data-subject export and the API bodies are
unchanged, except that `perUserDailyBudget` is now kept (the storage struct gained the field, and
`TestSettingsRoundTrip_EveryFieldSurvives` fills every field to catch the next drift). Writes still replace the
whole document, so two concurrent saves of different sections can still lose one; a section-level write API is the
follow-up. The data-lifecycle registry must claim `settings_sections` (user and organisation rows go by cascade,
the export reads them through `LoadUserSettings`).
