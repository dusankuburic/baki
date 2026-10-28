# 12. Settings stored per section, each with a version, dual-written with the legacy blob for one release

Date: 2026-10-08. Status: accepted.

## Context

Each settings scope stored the whole `AppSettings` document as one JSON blob; every write replaced the whole document with what the writing binary's struct knew, so after a rollback an older binary's first save deleted every field a newer one had added.

## Decision

- Sections are the document's top-level keys: `general`, `appearance`, `layout`, `ai`, `parser`, `analysis`, `recentFiles`. Declarations live in `internal/storage/settingsdoc/sections.go`, one entry per section with `Version` and one `Upgrade` per version step (a function on the stored JSON); `TestSections_CoverTheDocument` keeps every key of both `AppSettings` types declared.
- Read: a section stored at an older version is upgraded step by step (the blob counts as version 1); a section stored newer (a rollback) decodes the fields this binary knows, a changed-type field reading as zero. Writes merge: they keep the stored newer version and every field this binary does not know, at any depth; a known field follows the write (absent means cleared). Owner rules: never change a field's JSON type; prefer adding fields (a rename cannot carry an older binary's later edits forward).
- Postgres (v35, expand): `settings_sections (scope, scope_id, section, version, data, legacy_digest, updated_at)` with generated `user_id`/`org_id` cascade columns, no RLS. The desktop file adds a `sectionVersions` object.
- One release of dual write and dual read: a save writes, in one transaction, the blob as the previous release wrote it plus merged section rows, each with `legacy_digest` (md5 of its section as written into the blob); a load prefers the row while the blob matches the row's digest, else the blob (a release that writes only the blob changed that section). Contract steps: (1) next release, a resumable boot job under a global lock copies blobs into rows and reconciles digest mismatches, saves stop writing the blob (NULL `legacy_digest` reads as current); (2) the release after, once no non-NULL digest remains, a `breaking` step drops the blob tables.
- Amended (open-items sweep): the settings routes write only the top-level sections a body names (`service.SettingsSections`); an omitted section keeps its stored value, an explicit `{}` or `null` clears one.
- Amended (R-config-4, 2026-10-27): the section-level write exists for organisations: `GET` and `PATCH /api/system/settings/org/{id}/sections/{section}`. A PATCH names fields of one section (`{values, ifRevision}`); `UpdateOrgSettingsWith` locks the owner row `FOR NO KEY UPDATE`, then the blob and section rows `FOR UPDATE`, reads the document in the same transaction and merges the named fields into the section as stored, so concurrent writes to different fields of one section both land. `ifRevision`, a digest of the section that GET and PATCH return, answers 409 `SETTINGS_STALE` when the section moved since it was read; the client reloads before retrying. The whole-document POST keeps its read outside the lock. `OrgAIProvidersSection` writes through the PATCH and falls back to the POST on a replica without the route (404).

## Consequences

`perUserDailyBudget` is now kept (`TestSettingsRoundTrip_EveryFieldSurvives` catches the next drift). Two concurrent saves of different sections through the whole-document routes can still lose one; the organisation's section write (amendment above) does not, and the user and deployment documents can get one the same way. The data-lifecycle registry must claim `settings_sections`.
