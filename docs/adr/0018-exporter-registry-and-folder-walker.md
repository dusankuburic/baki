# 18. Export formats are a registry, and flow folders have one walker

Date: 2026-10-08. Status: accepted (core half wave 1; bakicli, API, export service, CI webhook wave 3).

## Context

Six export formats had six signatures and every surface kept its own list (bakicli usage line and validation map, analyze-raw switch, the export service's three methods, a second weaker SARIF writer in the CI webhook). Folders were walked six ways: `bakicli analyze` skipped dot directories and symlinks while `fix`/`watch` entered `.git`, followed symlinks and matched `.bakiignore` with OS separators, so on Windows fix changed files analyze never reported.

## Decision

1. Formats register themselves: `core/export` holds `Format{ID, Label, MIME, Ext, Binary, Order, Render}` and `Input{Report, Doc}`; each format file calls `Register` from `init`. `Register` panics on a bad or duplicate ID/Ext/Order and after the first read. `Lookup`, `List`, `IDs` and `Bytes(id, in, opts)` read the registry.
2. IDs are stored data — `json`, `sarif`, `junit`, `csv`, `html`, `markdown`, `pdf` are CLI `-format` values, API `format` values and audit `Meta["format"]` — and are never renamed. `text` is the CLI's console output and `Register` refuses it. `Order` keeps today's help-line order.
3. `Render` owns the whole document, including the trailing newline every caller used to append to SARIF and JUnit; every surface writes `Bytes` as is. `json` is a format (`JSONReport`, two-space indent, trailing newline). `Options.ToolVersion` carries the build version (no process global; empty renders `0.1.0`).
4. One folder walker in `core/parser`: `FolderOptions`, `ListFolder`, `ReadFolder`, `ParseFolderTree`. `TreeFolder()` is analyze's file set (recursive `.txt`/`.pad`, `.bakiignore`, no dot directories, no symlinks below the root, reads through `os.Root`, slash-separated keys relative to the folder — these keys feed subflow names, fingerprints, baselines and SARIF URIs). `ExportFolder()` is the desktop's set (flat, `.txt` only, symlinks followed). `IgnoreList` matches with `path.Match` on slash paths on every OS; a pattern matches the relative path, the base name or a `dir/` prefix, and a matching directory is pruned.
5. The CI webhook's `format=sarif` uses the core SARIF, served as `application/sarif+json` (indented, trailing newline; gains rules, partial fingerprints, physical locations, `logicalLocations`, `automationDetails`). The wire change is intended.

## Consequences

`.bakiignore` behaves the same on every OS (`*` no longer crosses `/` on Windows; patterns use forward slashes; a BOM no longer hides the first pattern). When fix and watch move to `ListFolder` they act on analyze's file set. The browser's findings-table CSV stays client-side (a different document, not the registered `csv`). PDF bytes are not reproducible in production (fpdf stamps the date); tests pin them through fpdf's test defaults.
