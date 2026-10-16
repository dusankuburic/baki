# Form validation policy

How the frontend validates user input, via the shared pieces extracted from the Azure settings modals (used by every form added or touched since).

| Piece | Where | What it owns |
|---|---|---|
| `useFormValidation` | `hooks/useFormValidation.ts` | Reveal state machine: errors hidden until blur or submit, then live. `fieldProps()` spreads error + onBlur; `submitValidate()` returns the first invalid field for focus. |
| `emailErrorKey` / `httpUrlErrorKey` | `lib/validators.ts` | Pure checks returning common-namespace i18n keys (`t(key, {ns: 'common'})`). |
| Constraint constants | `lib/constraints.ts` | Client mirrors of backend limits (email ≤254 bytes, avatar URL ≤2048, token expiry ≤365 days, …); each names the Go file it MUST match. |
| `apiErrorField` | `lib/apiErrorField.ts` | Reads `details.field` / `details.reason` to map a 400 onto a form field. |
| `PasswordRuleList` | `components/auth/PasswordRuleList.tsx` | Live server-policy checklist (12–72 bytes, 3 of 4 classes via `lib/password`). |
| `Input`/`Select`/`Textarea`/`SecretField`/`NumberField` | `components/shared/` | `error` + `hint` props wire `aria-invalid`/`aria-describedby`; prefer them over a raw `<input>`. |

## The contract

1. **The server stays authoritative.** Client checks are deliberately lighter; mirrored constants/regexes name their Go counterpart. Map a server refusal onto the field (`apiErrorField` or a code→field map), never a generic failure.
2. **Values stay in the form; the hook owns only validation display.** Forms keep `useState` fields and pass a pure `validate()` closure. No controlled-input migration, no form library, no zod for forms (zod is response-validation only, out of the eager chunk).
3. **Reveal on blur or submit, never while typing.** `errorFor(field)` is undefined until touched or `submitValidate()` ran; a failed submit reveals everything and the caller focuses `submitValidate()`'s return (field order = the `fields` array).
4. **Validation is inline and persistent; toasts are for transient operation failures.** A validation miss as a toast vanishes and is not aria-linked to the field.
5. **Server field errors compose ahead of `errorFor()`**, held in the form's own state so an edit clears them per field (see `ChannelForm`'s `fieldError` + `errorOf`).
6. **Use `noValidate`.** HTML5 `required`/`type=email` may stay as progressive hints but must not be the mechanism.
7. **Tolerance is a product decision, per flow.** Login does not gate on email format (same generic 401); register gates on format and the live password policy.
8. **Field ids can collide with prototype keys.** The hook reads errors by own property only; with a plain record, `errors['constructor']` reads through `Object.prototype` (the credential form has such a field name; a test pins it).

## Copy and tests

Validation strings come from i18n keys, not literals: shared vocabulary `common.errors.*` in `src/i18n/en.ts`, form-specific messages in the owning namespace; `src/i18n/coverage.test.ts` enforces this for completed directories. Pure validators get colocated unit tests (`lib/validators.test.ts`, including byte-vs-code-unit length cases); component tests assert the reveal semantics (hidden while typing, shown on blur/submit, aria-wired, invalid form sends nothing); `e2e/form-validation.spec.ts` locks the auth forms' end-to-end flows.
