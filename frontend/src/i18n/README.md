# i18n Policy

## Current state

- `SUPPORTED` contains a single locale (`en`); `en.ts` holds the keys.
- ~22 of ~198 components use `useTranslation` — essentially `auth/`,
  `findings/`, and `settings/*` panels. The rest of the app is hardcoded
  English.
- Infrastructure is complete and production-shape: synchronous init,
  `<html lang>` sync, persistence of the chosen locale.

## Policy (decision: incremental)

**New or modified user-facing strings use `t()` with keys in `en.ts`.**

- No mass migration of existing hardcoded strings — the churn is not worth
  it until a second locale is actually planned; a sweep would touch most
  components and invalidate in-flight PRs for zero user-visible gain today.
- When you are ALREADY editing a component for another reason and it has a
  user-facing literal, migrating its strings to keys in the same change is
  encouraged (leave the file better than you found it).
- Adding a second locale later requires auditing the un-migrated components
  anyway (context, plurals, date formats) — that audit is unavoidable and is
  the right moment for the sweep, not now.

## Conventions

- Keys are namespaced per area: `shell.*`, `auth.*`, `findings.*`,
  `settings.*`, `chat.*`, …
- Interpolation via the second `t()` argument; never concatenate translated
  fragments.
- The type-safe key registry lives in `i18next.d.ts` — run `npm test` after
  adding keys; `i18n.test.ts` validates key completeness against `en.ts`.
