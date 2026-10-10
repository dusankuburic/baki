# Custom rules

A custom rule is a check you write yourself, in JSON, that Baki runs next to its built-in rules. This page is the
format reference: where rules live, every field and how it matches, how rules are checked, and the errors you get
back. The design and its versioning rules are [ADR 0015](adr/0015-custom-rule-schema.md).

## Where custom rules live

| Source | Who sets it | Checked |
|---|---|---|
| An organisation's rules (Settings > Organisations > Custom rules, `POST /api/orgs/{id}/rules`) | org admins | strictly on save, tolerantly when analyses read them |
| `PAD_CUSTOM_RULES=<file>` on the server | the operator; every tenant gets these rules | tolerantly, once at boot |
| `bakicli -custom-rules <file>`, or `customRules` in `.bakirc.json` | the repository | tolerantly |
| `customRulesInline` in `.bakirc.json` (what the org export writes) | the repository | tolerantly |
| `bakicli rules test`, `bakicli rules lint`, the editor's Check and Try it (`/api/analysis/custom-rules/validate`, `/test`) | the author | strictly |

An organisation's rules apply to its members' analyses of the organisation's flows. A rule id is unique within one
organisation: two organisations may both have `house-style` and mean different things.

## The format

One rule is a JSON object. Today every rule has the kind `block`: it looks at one block (action, loop, condition,
…) at a time and reports a finding on each block it matches.

```json
{
  "kind": "block",
  "schemaVersion": 1,
  "id": "no-http-without-retry",
  "name": "HTTP actions need a retry policy",
  "description": "Calls to partner APIs fail transiently; the team wraps them in a retry.",
  "severity": "warning",
  "category": "Reliability",
  "rawTypeMatch": "^Web\\.InvokeWebService",
  "propertyNot": {"Retry": ""},
  "suggestion": "Wrap the call in ON ERROR with a retry",
  "autoFix": "wrap-in-retry"
}
```

| Field | Type | Meaning |
|---|---|---|
| `kind` | string | What the rule matches. `block` is the only kind this version runs. Absent means `block`. |
| `schemaVersion` | integer | The version of the kind's format: `1`. Absent (or 0) means 1. |
| `id` | string | The rule id on every finding, in suppressions (`pad-ignore[<id>]`) and in rule settings. Required. It must not be a built-in rule's id (see `bakicli rules`). A saved organisation rule keeps its id; to use another, create a new rule and delete the old one. |
| `name` | string | The finding's title. |
| `description` | string | The finding's description. |
| `severity` | string | `error`, `warning` or `info`, lower case. Absent or empty means `warning`. |
| `category` | string | The finding's category, e.g. `Style`, `Security`, `Reliability`, `Performance`, `Maintainability`. |
| `typeMatch` | string | The block's type, compared case-insensitively: `ACTION`, `LOOP`, `CONDITION`, `SUBFLOW`, `ERROR_HANDLER`, `WAIT`, `VARIABLE`, … |
| `rawTypeMatch` | string | A regular expression ([RE2 syntax](https://github.com/google/re2/wiki/Syntax)) the block's action identifier must match, e.g. `^Web\.` or `^SET$`. Case-insensitive, not anchored. |
| `nameMatch` | string | A regular expression the block's name must match. Case-insensitive, not anchored. |
| `propertyHas` | object | Property name to text: the block must have **every** property (exact name) and each value must contain its text, case-insensitively. |
| `propertyNot` | object | Property name to text: the block must have **no** property whose value contains its text. `{"Retry": ""}` means "has no `Retry` property". |
| `suggestion` | string | What to do about it. Absent: "Review this block for potential issues." |
| `autoFix` | string | A fix the findings offer: `append-output`, `init-variable`, `insert-close`, `insert-default`, `insert-delay`, `insert-delay-in-loop`, `insert-error-log`, `insert-exit-condition`, `insert-handler-log`, `mask-sensitive-variable`, `parameterize-sql`, `remove-block`, `replace-with-variable`, `sanitize-command-vars`, `set-timeout`, `suppress`, `upgrade-to-https`, `wrap-error-handler`, `wrap-in-retry`. |

A block matches when **every** criterion you set holds (`typeMatch`, `rawTypeMatch`, `nameMatch`, `propertyHas`,
`propertyNot`). A rule without any of them matches every block: that is valid, and the strict checks warn about it
(`NO_MATCHER`).

Keys are matched as Go's `encoding/json` matches them: exactly, else case-insensitively (`RawTypeMatch` is
`rawTypeMatch`). A key Baki does not know is never silently ignored: see the next section.

### Rules documents

A rules file (`-custom-rules`, `PAD_CUSTOM_RULES`, `bakicli rules test`/`lint`, the validate endpoint) holds one of:

- an array of rules: `[{…}, {…}]`;
- one rule object with an `id`;
- either inside an envelope: `{"rules": […]}`.

`null` is an empty document. In `.bakirc.json` the rules sit in `customRulesInline` (an array), and `customRules`
names a rules file:

```json
{
  "failOn": "error",
  "customRules": "ci/rules.json",
  "customRulesInline": [
    {"kind": "block", "schemaVersion": 1, "id": "acme-no-set", "name": "Acme bans SET", "severity": "error",
     "category": "Style", "rawTypeMatch": "^SET$"}
  ]
}
```

## Strict and tolerant checks

Every rule goes through the same checks in `pad-core` (`CompileCustomRule`), in this order:

1. **Kind and version.** A `kind` other than `block`, or a `schemaVersion` above 1 (or below 0), is refused in every
   mode: running it as a block rule would match blocks it was never meant to. It was written by a newer release of
   Baki or bakicli.
2. **Shape.** An unknown field, a missing `id` and a built-in rule's id.
3. **Values.** `severity`, `nameMatch`, `rawTypeMatch` and `autoFix`.

**Strict** (anything you author or save): the shape problems are errors. A rule without a matcher is a warning.

**Tolerant** (anything already stored or checked in, read by an analysis): the shape problems are warnings and the
rule runs: an unknown field is ignored, a rule without an id runs with an empty rule id, and a rule under a built-in
id runs next to the built-in rule. A value error skips that one rule with a warning. A checked-in file therefore never
breaks a pipeline when Baki learns to check more.

| Surface | A shape problem | A value error | A kind or version from a newer release | A named file that does not exist |
|---|---|---|---|---|
| Org rules, save | 400, not stored | 400, not stored | 400 | – |
| Org rules, analysis | warning in the server log, the rule runs | the rule is skipped; the rule list shows the `problem` | skipped; the list shows the `problem` | – |
| `PAD_CUSTOM_RULES` | warning in the server log | skipped, warning | skipped, warning | warning in the server log, no rules |
| `bakicli` (`-custom-rules`, `.bakirc.json`) | warning on stderr | skipped, warning on stderr | exit 2, "needs a newer bakicli" | exit 2 |
| `bakicli rules test` | exit 2 | exit 2 | exit 2 | exit 2 |
| `bakicli rules lint` | listed, exit 1 | listed, exit 1 | listed, exit 1 | exit 2 |

A malformed rules file (not JSON, or a JSON type error such as `"severity": 3` in any entry) fails as a whole, as it
always has: bakicli exits 2 and the server loads no `PAD_CUSTOM_RULES` rules (an ERROR in its log). A stored
organisation rule that does not decode is skipped on its own, listed with `INVALID_JSON`, unless its `kind` or
`schemaVersion` is a newer release's (a later version may give a field another JSON type): that rule is listed with
`UNKNOWN_KIND` or `UNSUPPORTED_VERSION` like any newer release's rule, cannot be overwritten here, and is exported as
stored.

The server logs a stored organisation rule's warnings and problems once per rule content (again after an edit), not on
every analysis.

## Organisation rules in the API

All routes need the server store (cloud mode). Reads need membership, writes org admin.

- `GET /api/orgs/{id}/rules` lists the rules: `{id, ruleId, config, enabled, createdAt, updatedAt}`, plus `problem`
  (why the analysis skips the rule) and `problemReason` (its reason code) when it does. `config` is the stored rule
  with `kind` and `schemaVersion` spelled out and **without** fields this version does not know, so sending it back
  is always a valid save.
- `POST /api/orgs/{id}/rules` with `{"config": {…}, "enabled": true, "id": "<row id>"}` saves a rule (`id` and
  `enabled` optional; `enabled` defaults to true). The body is checked strictly, the rule too. Fields a newer release
  added to the stored rule are kept. Two cases are a 409 `CONFLICT`: a `config.id` different from the stored rule's
  (rename), and a stored rule written by a newer release (edit it there).
- `POST /api/orgs/{id}/rules` with `{"id": "<row id>", "enabled": false}` and **no** `config` pauses or resumes a
  saved rule without touching or re-checking its config; a rule that no longer validates can still be paused.
- `DELETE /api/orgs/{id}/rules/{rowId}` deletes it.
- `GET /api/orgs/{id}/rules/export` returns a `.bakirc.json` for CI: the organisation's rule settings
  (`ruleConfig`) and its **enabled** rules (`customRulesInline`), each as stored with `kind` and `schemaVersion` first.
  A rule the server skips is still exported, with a `problem` field, so CI never silently loses it: bakicli skips it
  with a warning, or exits 2 for a newer release's rule. A rule whose stored JSON does not decode (and that no newer
  release wrote) makes the export a 409; edit and save it, or delete it.

The validate and test endpoints work without the server store (also on the desktop):

- `POST /api/analysis/custom-rules/validate` takes a rules document (up to 1 MiB) and checks each entry strictly
  and on its own. It answers 200 with `{"entries": [...], "valid": n, "invalid": n}`; each entry is `{index, id,
  valid, error, field, reason, problems}`, where `error`/`field`/`reason` are the first error and `problems` lists
  every error and warning (`{field, reason, message, warning}`). A body that is not a rules document is a 400.
- `POST /api/analysis/custom-rules/test` takes `{"rule": {…}, "flowId": "…"}`, checks the rule strictly and runs it
  alone against the flow: `{"matches": n, "findings": [...], "flowName": "…"}`.

## Errors

A rule the strict check refuses is a 400 with the code `BAD_REQUEST`, the message `invalid rule: <reason text>`, and
details naming the field and the reason:

```json
{"code": "BAD_REQUEST", "message": "invalid rule: custom rule \"house\": unknown field \"nameMatches\"",
 "details": {"field": "config.nameMatches", "reason": "UNKNOWN_FIELD"}}
```

`details.field` is the request's path to the field: `config.<field>` on a save, `rule.<field>` on a test. It is
absent when the problem is the rule as a whole. The reason codes are stable:

| Reason | Meaning | Strict | Tolerant |
|---|---|---|---|
| `INVALID_JSON` | not JSON, not an object, or a field of the wrong JSON type | error | error (the file or row is unreadable) |
| `UNKNOWN_KIND` | a `kind` this version does not run | error | error |
| `UNSUPPORTED_VERSION` | a `schemaVersion` above 1, or negative | error | error |
| `UNKNOWN_FIELD` | a field this version does not know (`field` names it) | error | warning, ignored |
| `MISSING_ID` | no `id` | error | warning, empty rule id |
| `RESERVED_ID` | the `id` is a built-in rule's | error | warning, both run |
| `INVALID_SEVERITY` | `severity` is not `error`, `warning` or `info` | error | error, rule skipped |
| `INVALID_PATTERN` | `rawTypeMatch` or `nameMatch` is not a valid regular expression | error | error, rule skipped |
| `UNKNOWN_AUTOFIX` | `autoFix` is not a fix Baki has | error | error, rule skipped |
| `NO_MATCHER` | no matcher: the rule matches every block | warning | – |

One exception keeps old rules editable: saving an organisation rule whose stored `rule_id` already is a built-in
rule's id (saved before ids were checked) accepts `RESERVED_ID`. A rule saved before severities were checked may hold
an invalid severity: it is listed with its problem, can be paused, and is fixed by editing its severity.

## Versions and compatibility

- A rule without `kind` or `schemaVersion` is a `block` rule of version 1, forever. Baki writes both on save and in
  exports; every earlier release and every older bakicli ignores them, so rolling back is safe.
- A field may be added within a version only when ignoring it is safe: a reader that does not know it fires the rule
  the same way or more, never silently less (it also warns). Anything else (a new matcher, a changed matching rule)
  is a new `schemaVersion` or a new kind, which older readers refuse.
- Fields from a newer release survive this release's save of the same rule.
- A rule's revision in baselines and new-rule notices (ADR 0016) is a digest of what it matches (`kind`, the
  matchers and `autoFix`); renaming, re-describing or re-grading a rule does not make it new.
