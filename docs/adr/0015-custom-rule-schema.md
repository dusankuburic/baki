# 15. Custom rules carry a kind and a schema version, decoded strictly on writes and tolerantly on reads

Date: 2026-10-08. Status: accepted.

## Context

A custom rule (`analyzer.CustomRuleConfig`) is JSON with 12 keys and no version. It is stored in org rows
(`org_custom_rules.config`), checked into customer repositories (`-custom-rules` files, `.bakirc.json`
`customRulesInline`), set by operators (`PAD_CUSTOM_RULES`) and sent to the API (save, validate, test). Nine decode
sites used plain `json.Unmarshal`, so an unknown key was dropped without a word: a rule whose only matcher was
misspelled (`nameMatches`) had no matcher left and fired on every block. The same holds for a rule a later release
might write with a new kind or matcher: every release so far compiles it into a rule that matches every block, and
old `bakicli` binaries pinned in pipelines cannot be upgraded by us.

The rule's `RuleDigest` (a hash of the whole config) keys the analysis cache, the suppression inventory and the
scanner's skip check. Baselines are about to persist a per-rule revision (ADR 0016), which must not move when only a
name or a severity changes. A custom rule may also reuse a built-in rule's id, and then shares its settings entry,
suppressions and stage.

## Decision

- **Kind and version.** A rule has `kind` (today's is `"block"`: it matches one block at a time) and
  `schemaVersion` (1). A missing kind means `block` and a missing version means 1, forever; no migration rewrites
  stored rows. Writes spell both out (`EncodeCustomRule`), first in the object.
- **One decoder and compiler in `pad-core`**, shared by the CLI and the server (`core/analyzer/custom_rule_schema.go`,
  `custom_rules.go`): `CustomRuleConfig.UnmarshalJSON` decodes the known fields exactly as `encoding/json` does
  (case-insensitive keys, `null` a no-op, the same type-error messages, merging) and records the unknown top-level
  keys; `DecodeCustomRule` reads one row; `DecodeCustomRuleDocument` reads a document (an array, one object with an
  id, or either inside `{"rules": …}`) entry by entry; `CompileCustomRule(cfg, mode)` is the only construction path,
  and `CheckCustomRule` lists every problem for lint. `NewCustomRule` and `LoadCustomRules` stay as tolerant
  wrappers.
- **Modes.** Strict on writes (API save, validate and test; `bakicli rules test` and `rules lint`): an unknown field,
  a missing id and a built-in rule's id are errors, and a rule without a matcher is a warning (it stays valid). Tolerant
  on reads (stored rows, `PAD_CUSTOM_RULES`, `-custom-rules`, `.bakirc.json`): the same problems are warnings and
  the rule runs, so a checked-in file never breaks a pipeline on upgrade. In **both** modes a kind or a
  `schemaVersion` this build cannot read is refused: the server skips the rule and reports it as a problem, the CLI
  exits 2 ("needs a newer bakicli"). Running it as a block rule would match blocks it was never meant to.
- **Typed errors.** `CustomRuleError{Index, RuleID, Field, Reason, Msg, Warning, Err}` with stable reasons:
  `INVALID_JSON`, `UNKNOWN_FIELD`, `UNKNOWN_KIND`, `UNSUPPORTED_VERSION`, `MISSING_ID`, `RESERVED_ID`,
  `INVALID_SEVERITY`, `INVALID_PATTERN`, `UNKNOWN_AUTOFIX` and the warning-only `NO_MATCHER`. The existing checks
  keep their messages. The API keeps the code `BAD_REQUEST` and adds `details.field` and `details.reason`.
- **Digests.** `RuleDigest` does not move for any existing rule: kind and version are cleared at their v1 values
  before hashing (both are `omitempty`, so the bytes are the old marshal) and unknown fields are never hashed
  (pinned: `617f98157a643508`). `MatchDigest` is new and is a custom rule's revision in a rule set: it hashes the
  kind, the matchers (`typeMatch`, `rawTypeMatch`, `nameMatch`, `propertyHas`, `propertyNot`) and `autoFix`, not the
  id, name, description, severity, category, suggestion or `schemaVersion` (a version step keeps meaning). A field
  joins it only with `omitempty`, so existing values never change; a test makes every new field choose a side.
- **Forward compatibility.** The decoded config keeps unknown fields with their raw values. A save merges the stored
  row's unknown keys into what it writes (`WithUnknownFieldsFrom`, then `EncodeCustomRule`), so this release's edit
  does not drop a later release's additive field. Echoes marshal the known fields only, so the editor never sends
  back an unknown field; the CI export writes the stored rule whole (`EncodeCustomRule`), so a newer release's rule
  reaches bakicli intact. Strict checks that earlier releases did not make still refuse some stored rows
  when they are echoed back: an id equal to a built-in rule's (`RESERVED_ID`; save never refused it before) and the
  legacy invalid severities. Pausing such a row must not re-validate its config, and an edit that keeps the stored
  `rule_id` must not fail on `RESERVED_ID`; the server's save path (not the schema) owns both.
- **Versioning rule.** A field an older reader may ignore safely (the rule then fires the same way or more, never
  silently less) is added within a version. Anything else (a new matcher, a changed matching semantic, a field whose
  absence widens or narrows matching) is a new `schemaVersion` or a new kind, which older readers refuse. Within a
  version a field never changes its JSON type. Upgrades between versions, when one exists, are steps on the raw JSON
  as in ADR 0012; none exist at version 1.
- **Also decided** (implemented with the server and CLI): the export keeps emitting rows that no longer compile,
  with a `problem` field, and the list shows it; an id rename is refused (the editor makes the id read-only, the
  store answers 409 instead of 500); a named `-custom-rules` or `PAD_CUSTOM_RULES` file that does not exist makes the
  CLI exit 2 and the server log a warning. Custom rules have no `stage` field yet.

## Rollback

This release writes `kind` and `schemaVersion`; every earlier release ignores both and reads the rule as before. The
first release that **writes** another kind or a `schemaVersion` above 1 must state that rollback below this release
is not supported and must keep exports at version 1 for clients that do not ask for more, because every release
before this one, and every older `bakicli`, would run such a rule against every block.

## Consequences

Typos and unreadable rules are reported where they used to vanish, and a rule from the future is refused rather than
run wrongly. Decoding costs a second JSON pass per rule, at most once per org resolve and once at boot. Wire changes
are additive. One residual: inside another struct (a request body, `.bakirc.json`), `encoding/json` names that
struct, not `CustomRuleConfig`, in a type error of any type with its own `UnmarshalJSON`; the field path is
unchanged. Rejected: strict decoding of read paths (breaks checked-in files on upgrade); "an explicit
`schemaVersion` makes the reader strict" (a newer export would fail in an older bakicli); `kind` columns (they
duplicate the JSON and nothing queries by kind); `json.RawMessage` fields in every request struct instead of
`UnmarshalJSON`; and isolating one entry's type error in `LoadCustomRules` (the whole file still fails, as before).
