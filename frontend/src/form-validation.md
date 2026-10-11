# Form validation policy

How the frontend validates user input. The shared pieces were extracted from
the Azure settings modals' hand-rolled pattern; every form added or touched
since uses them.

## The pieces

| Piece | Where | What it owns |
|---|---|---|
| `useFormValidation` | `hooks/useFormValidation.ts` | The reveal state machine: errors stay hidden until a field is blurred or the form is submitted, then appear and recompute live. `fieldProps()` spreads error + onBlur onto a shared input; `submitValidate()` returns the first invalid field for focus. |
| `emailErrorKey` / `httpUrlErrorKey` | `lib/validators.ts` | Pure field checks returning common-namespace i18n keys (`t(key, {ns: 'common'})`). |
| Constraint constants | `lib/constraints.ts` | Client mirrors of backend limits (email ≤254 bytes, avatar URL ≤2048, token expiry ≤365 days, …). Each names the Go file it MUST match. |
| `apiErrorField` | `lib/apiErrorField.ts` | Reads `details.field` / `details.reason` — the backend envelope's only per-field channel — for mapping a 400 onto a form field. |
| `PasswordRuleList` | `components/auth/PasswordRuleList.tsx` | The live server-policy checklist (12–72 bytes, 3 of 4 classes via `lib/password`). |
| `Input`/`Select`/`Textarea`/`SecretField`/`NumberField` | `components/shared/` | `error` + `hint` props wire `aria-invalid`/`aria-describedby` for free. A raw `<input>` must do that wiring itself — prefer the shared widgets. |

## The contract

1. **The server stays authoritative.** Client checks catch obvious mistakes
   early; they are deliberately lighter than the Go rules and every mirrored
   constant/regex names its Go counterpart in a comment. When the server
   refuses, map its verdict onto the field (`apiErrorField`, or a code→field
   map like `modelErrorTarget`) instead of showing a generic failure.
2. **Values stay in the form; the hook owns only validation display.** Forms
   keep their `useState` fields and pass a pure `validate()` closure over
   them. No controlled-input migration, no form library, no zod for forms
   (zod is response-validation only and stays out of the eager chunk).
3. **Reveal on blur or submit, never while typing.** `errorFor(field)` returns
   undefined until the field was touched or `submitValidate()` ran. A failed
   submit reveals everything and the caller focuses `submitValidate()`'s
   return value (field order = the `fields` array).
4. **Validation is inline and persistent; toasts are for transient
   operation failures.** Never report a validation miss as a toast — it
   vanishes and is not aria-linked to the field.
5. **Server field errors compose ahead of `errorFor()`**, held in the form's
   own state so an edit can clear them per field (see `ChannelForm`'s
   `fieldError` + `errorOf`).
6. **Use `noValidate`** and rely on the above; HTML5 `required`/`type=email`
   attributes may stay as progressive hints but must not be the mechanism.
7. **Tolerance is a product decision, per flow.** Login deliberately does not
   gate on email format (the server answers a bad email with the same generic
   401); register does gate — on format and the live password policy.
8. **Field ids can collide with prototype keys.** The hook reads errors by
   own property only; if you bypass it with a plain record, remember
   `errors['constructor']` reads through `Object.prototype` (the credential
   form has such a field name, and a test pins the behavior).

## Copy

Validation strings come from i18n keys, not literals: the shared vocabulary is
`common.errors.*` in `src/i18n/en.ts`; form-specific messages live in the
owning namespace. `src/i18n/coverage.test.ts` enforces this for the completed
directories.

## Tests

- Pure validators get colocated unit tests (`lib/validators.test.ts` is the
  model — including the byte-vs-code-unit length cases).
- Component tests assert the reveal semantics: error hidden while typing,
  shown on blur/submit, aria-wired, submit disabled or blocked with no API
  call while invalid.
- The auth forms' end-to-end flows (blur-reveal, live policy, invalid form
  sends nothing) are locked in by `e2e/form-validation.spec.ts`.
