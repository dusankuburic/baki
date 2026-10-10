# 18. Export formats are a registry, and flow folders have one walker

Date: 2026-10-08. Status: accepted. The core half shipped first (Phase 4a wave 1); bakicli, the API, the export
service and the CI webhook switch to it in wave 3.

## Context

`core/export` had six formats with six signatures: HTML and Markdown returned a string, the rest `[]byte`, and only
SARIF and JUnit took `Options`. Every surface kept its own list. bakicli had a usage line, a validation map and a
switch, plus its own `json` encoder. analyze-raw had a validation switch and an if-chain, the export service had
three near-identical methods, and the CI webhook had a second, weaker SARIF writer. Each caller appended the newline
SARIF and JUnit lack. The SARIF tool version was a process global set by an `init()` in `internal/service`, so
bakicli, which does not import that package, always reported `0.1.0`.

Folders were loaded six ways. `bakicli analyze` walked recursively, skipped dot directories and symlinks, read
through `os.Root` and matched `.bakiignore` with `filepath.Match`. `bakicli fix` and `watch` used a second walk that
entered `.git`, followed symlinks and computed relative paths with OS separators, so on Windows the `vendor/` rule
never matched and `*` crossed `/`. fix therefore changed files that analyze never reported. The desktop's
`parser.ParseFolder` was flat and `.txt`-only.

## Decision

1. **Formats register themselves.** `core/export` holds `Format{ID, Label, MIME, Ext, Binary, Order, Render}` and
   `Input{Report, Doc}`. Each format file calls `Register` from its `init()`, so a new format is one file. `Lookup`,
   `List`, `IDs` and `Bytes(id, in, opts)` read the registry. Registration is init-only: `Register` panics on a bad
   or duplicate ID, Ext or Order and after the first read, which also makes the lock-free reads safe. Registration
   cannot go through fx: `pad-core` cannot import `internal`, and the CLI has no fx.
2. **IDs are stored data.** `json`, `sarif`, `junit`, `csv`, `html`, `markdown` and `pdf` are CLI `-format` values,
   API `format` values and the audit `Meta["format"]` of stored rows. They are never renamed. `text` is the CLI's
   console output (it depends on `-quiet`, stderr warnings and the policy printout), and `Register` refuses it.
3. **`Order` keeps today's lists.** It is `json, sarif, junit, csv, html`, followed by `markdown, pdf`, so the CLI help
   line keeps its order when the CLI gains the two formats. Sorting by ID would reorder the help line.
4. **`Render` owns the whole document**, including the trailing newline every caller appended to SARIF and JUnit.
   Every text format ends with a newline, and every surface writes `Bytes` as it is. The built-in formats render in
   memory and write once. `Bytes` always runs `Render`, which hands it that slice without copying it through a
   buffer, so a `Render` replaced on a looked-up copy is never bypassed.
5. **`json` is a format.** Its document is `JSONReport` (the report with the policy verdict beside it), indented by
   two spaces and ending in a newline. Those are the bytes `bakicli -format json` and `-report-json` have always
   written. analyze-raw's default compact `render.JSON` body is a route decision and does not change here.
6. **`Options.ToolVersion` carries the build version.** There is no process global: the server's `ExportService.Render`
   fills in `service.Version` and bakicli its own version; an empty option renders the default `0.1.0`.
7. **No `ServerRendered` flag.** Every registered format is rendered on the server by construction. A flag that says
   how the web UI obtains the bytes is a UI concern and does not belong in `pad-core`. The only export the browser
   renders is the findings-table CSV: the filtered list, eight columns and no summary line. That is a different
   document, not the registered `csv`, so it stays client-side and outside the registry.
8. **One folder walker in `core/parser`.** It consists of `FolderOptions`, `ListFolder`, `ReadFolder` and
   `ParseFolderTree`.
   - `TreeFolder()` is bakicli analyze's file set, unchanged: recursive `.txt` and `.pad`, `.bakiignore`, no dot
     directories, no symlinks, reads through `os.Root`, slash keys relative to the folder, and `ParseFiles` order.
     The keys feed subflow names, fingerprints, baselines and SARIF URIs.
   - `ExportFolder()` is the desktop's set: flat, `.txt` only, symlinks followed. `ParseFolder` discovers its
     members through it and keeps its document shape: absolute `Files[].Path`, base-name `SourceFile` and
     `IsFolder` always. `ListFolder` with `MaxFileSize` offers the desktop's stat-only size check: it fails before
     anything is read, and a followed symlink counts its target's size. `LoadFlowFolder` still runs its own
     `os.ReadDir` pre-check (which counts a symlink's own size); it has not moved to `ListFolder`.
   - `IgnoreList` matches with `path.Match` on slash paths on every OS. A pattern matches the relative path, the
     base name, or a `dir/` prefix, and a matching directory is pruned.
9. **The CI webhook's `format=sarif` switches to the core SARIF** in wave 3, served as `application/sarif+json`. The
   wire change is intended. The body takes the core's schema URL, driver name and version and full message text, and
   gains rules, partial fingerprints, physical locations, the spec's `logicalLocations` array and
   `automationDetails`. It is indented and ends with a newline, where it was compact `application/json`.
   `bakicli report -format sarif` prints it verbatim.

## Consequences

- Help text, `-format` validation and analyze-raw's error message are built from the registry (wave 3). The CLI
  gains `markdown` and `pdf`. analyze-raw and the webhook keep their current format sets.
- Byte goldens (`core/export/testdata/golden`, now including `json`) and the registry tests prove that `Render` and
  `Bytes` produce exactly the pre-registry bytes for every format.
- `.bakiignore` behaves the same on every OS. On Windows, `*` no longer crosses `/`. A backslash escapes the next
  character, so patterns must use forward slashes; backslash patterns never matched in analyze. A UTF-8 BOM no
  longer hides the first pattern. When fix and watch move to `ListFolder` (wave 3), they act on analyze's file set:
  no dot directories, no symlinks, and the ignore file is honoured on Windows.
- Desktop folders stay flat. Recursion would need path validation wherever folder edits join `SourceFile`.
- The server's surfaces render through `ExportService.Render`: analyze-raw (`sarif`, `junit`, `csv` besides its
  JSON report), `/api/analysis/export/{sarif,junit}` (raw bodies, attachments named `pad-analysis<ext>`),
  `/api/export/{markdown,html,pdf}` (the base64 envelope) and the CI webhook. The web UI fetches SARIF and JUnit
  as text and saves the server's bytes; its findings CSV stays client-rendered (item 7).
- PDF bytes are still not reproducible in production (fpdf stamps the date and writes font resources in map order).
  The tests pin them through fpdf's test defaults.

## Alternatives rejected

- `Render` returning `[]byte`: the CLI streams to stdout. `Bytes` serves callers that need a slice.
- A central list of formats: it contradicts "a new format is one file".
- bakicli reusing `parser.ParseFolder` as it was: that would change keys, fingerprints and SARIF URIs, and reopen the
  symlink leak that `TreeFolder`'s confinement closes.
