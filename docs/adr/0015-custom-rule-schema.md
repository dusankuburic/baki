# 15. Custom rules carry a kind and a schema version, decoded strictly on writes and tolerantly on reads

Date: 2026-10-08. Status: accepted.

## Context

A custom rule is JSON stored in `org_custom_rules.config`, checked into repositories (`-custom-rules` files, `.bakirc.json`), set via `PAD_CUSTOM_RULES` and sent to the API — with 12 keys, no version, and nine decode sites using plain `json.Unmarshal`, so an unknown key (a misspelled matcher) vanished silently and the rule fired on every block; a rule from a future release would match every block in every older `bakicli`.

## Decision

- A rule has `kind` (today's `"block"`) and `schemaVersion` (1); a missing kind means `block` and a missing version means 1, forever — no migration rewrites stored rows. Writes spell both out (`EncodeCustomRule`).
- One decoder and compiler in `pad-core` (`core/analyzer/custom_rule_schema.go`, `custom_rules.go`): `CustomRuleConfig.UnmarshalJSON` (decodes exactly as `encoding/json`, records unknown top-level keys), `DecodeCustomRule`, `DecodeCustomRuleDocument` (array, one object, or `{"rules": …}`), `CompileCustomRule(cfg, mode)`, `CheckCustomRule` for lint.
- Modes: strict on writes (API save/validate/test; `bakicli rules test`/`lint`) — unknown field, missing id, a built-in's id are errors, a missing matcher is a warning. Tolerant on reads (stored rows, `PAD_CUSTOM_RULES`, checked-in files) — the same problems are warnings and the rule runs. In both modes a `kind` or `schemaVersion` this build cannot read is refused: the server skips and reports the rule, the CLI exits 2.
- Typed errors `CustomRuleError{...}` with stable reasons: `INVALID_JSON`, `UNKNOWN_FIELD`, `UNKNOWN_KIND`, `UNSUPPORTED_VERSION`, `MISSING_ID`, `RESERVED_ID`, `INVALID_SEVERITY`, `INVALID_PATTERN`, `UNKNOWN_AUTOFIX`, warning-only `NO_MATCHER`; the API adds `details.field` and `details.reason`.
- Digests: `RuleDigest` never moves for an existing rule (kind/version cleared at v1 values before hashing, unknown fields never hashed; pinned `617f98157a643508`). `MatchDigest` (a custom rule's revision) hashes kind, the matchers (`typeMatch`, `rawTypeMatch`, `nameMatch`, `propertyHas`, `propertyNot`) and `autoFix` — not id, name, description, severity, category or `schemaVersion`; new fields join only with `omitempty`.
- Forward compatibility: decoded configs keep unknown fields; a save merges the stored row's unknown keys back (`WithUnknownFieldsFrom`) so this release's edit does not drop a later release's field. Pausing a row must not re-validate its config; an edit keeping the stored `rule_id` must not fail on `RESERVED_ID` (the server's save path owns both).
- Versioning rule: a field an older reader may ignore safely (the rule fires the same or more) is added within a version; a new matcher, changed matching semantics, or a field whose absence widens/narrows matching is a new `schemaVersion` or kind, which older readers refuse. Within a version a field never changes JSON type.

## Consequences

The first release that writes another kind or a `schemaVersion` above 1 must state that rollback below this release is unsupported and must keep exports at version 1 — every earlier release would run such a rule against every block. Decoding costs a second JSON pass per rule, at most once per org resolve and once at boot.
