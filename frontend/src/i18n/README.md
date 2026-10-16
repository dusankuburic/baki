# i18n Policy

`SUPPORTED` has a single locale (`en`; keys in `en.ts`); ~22 of ~198 components use `useTranslation` (essentially `auth/`, `findings/`, `settings/*`). Infrastructure is complete: synchronous init, `<html lang>` sync, locale persistence.

**Policy (incremental): new or modified user-facing strings use `t()` with keys in `en.ts`.** No mass migration of existing hardcoded strings; migrating a component's literals is encouraged when already editing it for another reason. A second locale requires auditing un-migrated components anyway — sweep then, not now.

Conventions:

- Keys are namespaced per area: `shell.*`, `auth.*`, `findings.*`, `settings.*`, `chat.*`, …
- Interpolation via the second `t()` argument; never concatenate translated fragments.
- The type-safe key registry lives in `i18next.d.ts`; run `npm test` after adding keys — `i18n.test.ts` validates key completeness against `en.ts`.
