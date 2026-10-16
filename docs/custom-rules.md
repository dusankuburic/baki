# Custom rules

A custom rule is a check you write yourself, in JSON, that Baki runs next to its built-in rules: the format reference, where rules live, and the errors you get back. Design and versioning: [ADR 0015](adr/0015-custom-rule-schema.md).

## Where custom rules live

An organisation's rules (Settings > Organizations > Custom rules, `POST /api/orgs/{id}/rules`; checked strictly on save, tolerantly when analyses read them; a rule id is unique within one organisation and must not be a built-in rule's id) · `PAD_CUSTOM_RULES=<file>` on the server (the operator; every tenant; tolerantly at boot) · `bakicli -custom-rules <file>` or `customRules` in `.bakirc.json` (the repository; tolerantly) · `customRulesInline` in `.bakirc.json` (the repository; tolerantly) · `bakicli rules test` / `bakicli rules lint` and the editor's Check / Try it (the author; strictly).

## The format

Every rule today has the kind `block`: it looks at one block at a time and reports a finding on each match.

```json
{
  "kind": "block", "schemaVersion": 1, "id": "no-http-without-retry",
  "name": "HTTP actions need a retry policy", "severity": "warning",
  "category": "Reliability", "rawTypeMatch": "^Web\\.InvokeWebService",
  "propertyNot": {"Retry": ""}, "suggestion": "Wrap the call in ON ERROR with a retry",
  "autoFix": "wrap-in-retry"
}
```

| Field | Type | Meaning |
|---|---|---|
| `kind` / `schemaVersion` | string / integer | `block` is the only kind this version runs (absent = `block`); version `1` (absent or 0 means 1) |
| `id` | string | Required. The id on findings, in suppressions (`pad-ignore[<id>]`) and in rule settings; must not be a built-in rule's id |
| `name` / `description` / `suggestion` | string | Finding title / description / what to do (absent suggestion: "Review this block for potential issues.") |
| `severity` | string | `error`, `warning` or `info`, lower case; absent or empty means `warning` |
| `category` | string | e.g. `Style`, `Security`, `Reliability`, `Performance`, `Maintainability` |
| `typeMatch` | string | Block type, case-insensitive: `ACTION`, `LOOP`, `CONDITION`, `SUBFLOW`, `ERROR_HANDLER`, `WAIT`, `VARIABLE`, … |
| `rawTypeMatch` | string | RE2 regex the block's action identifier must match (e.g. `^Web\.`); case-insensitive, not anchored |
| `nameMatch` | string | RE2 regex the block's name must match; case-insensitive, not anchored |
| `propertyHas` | object | Block must have every named property and each value must contain its text, case-insensitively |
| `propertyNot` | object | Block must have no property whose value contains its text (`{"Retry": ""}` = no `Retry` property) |
| `autoFix` | string | One of: `append-output`, `init-variable`, `insert-close`, `insert-default`, `insert-delay`, `insert-delay-in-loop`, `insert-error-log`, `insert-exit-condition`, `insert-handler-log`, `mask-sensitive-variable`, `parameterize-sql`, `remove-block`, `replace-with-variable`, `sanitize-command-vars`, `set-timeout`, `suppress`, `upgrade-to-https`, `wrap-error-handler`, `wrap-in-retry` |

A block matches when **every** set criterion holds; no criteria matches every block (valid; strict checks warn `NO_MATCHER`). Keys match exactly, else case-insensitively; an unknown key is never silently ignored.

A rules document (`-custom-rules`, `PAD_CUSTOM_RULES`, `bakicli rules test`/`lint`, the validate endpoint) holds an array of rules, one rule object with an `id`, or either inside `{"rules": […]}`; `null` is an empty document. In `.bakirc.json`, `customRules` names a file and `customRulesInline` is an array.

## Strict and tolerant checks

Checks run in order: kind/version (a `kind` other than `block` or a `schemaVersion` above 1 is refused in every mode), shape (unknown field, missing `id`, a built-in rule's id), values (`severity`, `nameMatch`, `rawTypeMatch`, `autoFix`). **Strict** (authored or saved): shape problems are errors. **Tolerant** (stored or checked-in, read by an analysis): shape problems are warnings and the rule runs (unknown field ignored, missing id runs with an empty id, a rule without a matcher warns `NO_MATCHER`); a value error skips that one rule. A file that is not JSON, or a field of the wrong JSON type, fails as a whole: bakicli exits 2 and the server loads no `PAD_CUSTOM_RULES` rules. A newer release's kind/version is listed `UNKNOWN_KIND` / `UNSUPPORTED_VERSION` and refused everywhere.

## Organisation rules in the API

Cloud mode only; reads need membership, writes org admin.

- `GET /api/orgs/{id}/rules` → `{id, ruleId, config, enabled, createdAt, updatedAt}`, plus `problem` / `problemReason` when an analysis skips the rule. `config` is the stored rule with `kind` and `schemaVersion` explicit and unknown fields dropped, so sending it back is always a valid save.
- `POST /api/orgs/{id}/rules` with `{"config": {…}, "enabled": true, "id": "<row id>"}` saves strictly (`enabled` defaults true; fields a newer release added are kept). 409 `CONFLICT` on a `config.id` differing from the stored one (rename) or a stored rule written by a newer release.
- The same POST with `{"id": …, "enabled": false}` and **no** `config` pauses/resumes without re-checking the config.
- `DELETE /api/orgs/{id}/rules/{rowId}` deletes it.
- `GET /api/orgs/{id}/rules/export` returns a `.bakirc.json` for CI (enabled rules as `customRulesInline`); a skipped rule is still exported with a `problem`, an undecodable one makes the export a 409.
- Without the server store (desktop too): `POST /api/analysis/custom-rules/validate` takes a rules document (up to 1 MiB), checks each entry strictly → `{"entries": […], "valid": n, "invalid": n}`; `POST /api/analysis/custom-rules/test` takes `{"rule": {…}, "flowId": …}` and runs one rule → `{"matches": n, "findings": […], "flowName": …}`.

## Errors

A refused rule is a 400 `BAD_REQUEST`, message `invalid rule: <reason>`, with `details.field` (`config.<field>` on a save, `rule.<field>` on a test) and a stable `details.reason`:

| Reason | Meaning | Strict | Tolerant |
|---|---|---|---|
| `INVALID_JSON` | not JSON, not an object, or a field of the wrong JSON type | error | error (unreadable) |
| `UNKNOWN_KIND` / `UNSUPPORTED_VERSION` | a kind this version does not run / a version above 1 | error | error |
| `UNKNOWN_FIELD` | a field this version does not know (`field` names it) | error | warning, ignored |
| `MISSING_ID` | no `id` | error | warning, empty rule id |
| `RESERVED_ID` | a built-in rule's id | error | warning, both run |
| `INVALID_SEVERITY` / `INVALID_PATTERN` / `UNKNOWN_AUTOFIX` | invalid value (severity, regex, fix) | error | error, rule skipped |
| `NO_MATCHER` | no matcher; matches every block | warning | — |

Saving an organisation rule already stored under a built-in id accepts `RESERVED_ID` (saved before ids were checked). Within a version a field may only be added when a reader ignoring it fires the rule the same way or more; anything else is a new `schemaVersion` or kind. A rule's baseline revision is a digest of what it matches (`kind`, the matchers, `autoFix`).
