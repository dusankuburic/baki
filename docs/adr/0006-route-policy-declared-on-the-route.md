# 6. Route policy is declared on the route and resolved once, before the middleware

Date: 2026-10-07. Status: accepted.

## Context

About 15 path tables beside the middleware (auth, scopes, rate-limit buckets, RLS transaction, deadlines, metrics labels) each re-derived the route from the path string, and each drifted into a bypass at least once (path-traversal requests riding wrong buckets, RLS exemptions or the public route set). A new route meant editing up to six tables.

## Decision

1. Routes register through `route.Builder` with their policy (auth, token scopes, bucket, transaction, deadline class, capabilities). Defaults fail safe: authenticated, the method's default scope, general bucket, request transaction, default deadline. A route registered around the Builder does not boot (deny by default).
2. `route.Resolver` resolves the policy once, right inside the outer Recovery layer, via chi's own tree on `route.DispatchPath` (RawPath when set, else cleaned Path, `/api/v1` alias stripped); it records the policy on `reqinfo.Info` and never rewrites the URL. An unregistered method gets a memoized 405 policy (public only if all the path's routes are; strictest bucket; no transaction, no scope). An unmatched path under `/api/` is authenticated; the `/assets/` exemption needs both dispatch and served path under `/assets/`.
3. `route.Handler` checks the resolved policy is the endpoint's own (pointer equality); a mismatch logs ERROR, counts `pad_route_policy_guard_mismatch_total`, and in declarative mode answers 500. A route whose `Needs` the deployment lacks answers 503. At boot every route's sample path must resolve to its own policy.
4. `PAD_ROUTE_POLICY` = `legacy` | `shadow` (default) | `declarative`. Shadow counts disagreements in `pad_route_policy_mismatch_total{route,decision}`; 405/no-route policies are reviewed in `route_policy_declared.golden`. The metrics route label is always the chi pattern (`/api/library/{id}`) or `/api/other`, `/`, `/static/*`.

## Consequences

Intended changes (in `route_policy_declared.golden`): a 405 opens no RLS transaction and takes the strictest bucket; CORS preflights pay the general bucket. Labels changed for 363 of 1,249 golden requests — dashboards matching old labels must change with the deploy. Until the legacy tables are deleted, both goldens and the differential test pin identical decisions.
