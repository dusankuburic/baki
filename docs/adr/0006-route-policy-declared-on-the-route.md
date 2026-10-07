# 6. Route policy is declared on the route and resolved once, before the middleware

Date: 2026-10-07. Status: accepted.

## Context

Every cross-cutting decision about a request was a table of paths kept beside the middleware that made it:

- **Authentication:** `publicRoutes`.
- **Token scopes:** prefix rules plus special cases (`auth.RequiredScope`, `RouteAllowed`).
- **Rate-limit buckets:** `authRateLimitPaths`, `expensiveGetPaths`, the POST prefix rules, the Azure outbound
  routes and the `/assets/` exemption.
- **The RLS transaction:** streaming prefixes, about 40 heavy exact paths, and four segment matchers for routes
  that carry ids.
- **Deadlines:** path suffixes.
- **Metrics labels:** a walk of the static patterns plus hand-written collapses of the dynamic ones.

That is about 15 tables. Each re-derived the route from the path string, so each had to predict what chi would
dispatch to, and each prediction drifted into a bypass at least once:

- `POST /api/v1/auth/login` and `/api/auth/./login` rode the general bucket instead of the auth bucket.
- `/api/system/settings/org/..%2F..%2Fhealth` read as the public `/api/health` while chi ran the protected route.
- `/api/library/..%2Fchat%2Fx` read as an RLS-exempt chat path while chi ran a transactional library route.
- An escaped `.../versions/1%2F..%2F..%2Fapi%2Fevents` read as the SSE bus and ran with no deadline.
- Unknown `/api/` paths, and invite credentials, reached the metrics label.

A new route meant editing up to six of these tables by hand. A renamed route fell out of them silently, and a
test had to check the exemption list for stale entries.

## Decision

1. **Declared policy.** Routes register through `route.Builder` with their policy beside the handler. The policy
   holds who may call the route, the token scopes, the bucket, the transaction, the deadline class and the
   capabilities it needs. Defaults fail safe: authentication, the method's default scope, the general bucket,
   the request transaction and the default deadline. `Build` refuses a route registered around the Builder, so
   a route without a policy does not boot (deny by default).
2. **Resolved once, at the outer layer.** Right inside the outer Recovery and request info, `route.Resolver`
   finds the request's route with chi's own tree. It calls `mux.Find` on `route.DispatchPath`: RawPath when set,
   else the cleaned Path, with the `/api/v1` alias stripped. It records the policy on `reqinfo.Info` and never
   rewrites the URL. Every layer below reads that policy.
   - A method the path does not register gets a 405 policy built from the path's other routes. It is public only
     if they all are, it takes the strictest of their buckets (a CORS preflight stays on general unless that is
     the auth bucket), and it has no transaction and no scope. These policies are memoized, so pointers compare.
   - A path no route matches gets one of a few singletons. Under `/api/` it is authenticated. The `/assets/`
     exemption applies only when both the dispatch path and the served path are under `/assets/`. The share
     shell rides the analysis bucket whichever path names it.
3. **Guard.** Before the handler runs, `route.Handler` checks that the resolved policy is the endpoint's own
   (pointer equality). The NotFound and MethodNotAllowed handlers check the policy's kind.
   - On a mismatch it logs at ERROR and counts `pad_route_policy_guard_mismatch_total`. In declarative mode it
     answers 500 before the handler.
   - A route whose `Needs` the deployment lacks answers 503 in every mode.
   - At boot, every route's sample path must resolve to its own policy, or the router does not start.
4. **Shadow rollout.** `PAD_ROUTE_POLICY` is `legacy`, `shadow` (the default) or `declarative`.
   - In shadow mode the legacy tables decide. For routes, the declared decision is computed alongside, and each
     disagreement is counted in `pad_route_policy_mismatch_total{route,decision}` and logged once.
   - 405 and no-route policies differ from the tables by design. They are reviewed in
     `route_policy_declared.golden` instead of being counted on every preflight.
   - After a soak with no mismatch, the flip is a config-only revision. The tables are deleted one release later.
5. **Labels are patterns.** In every mode the metrics route label is the policy's label: the chi pattern
   (`/api/library/{id}`), or `/api/other`, `/` or `/static/*` when nothing matches.

## Consequences

A route is one registration with its options: there are no tables to keep in step, and the guard catches a
resolver that disagrees with chi before any handler runs.

**Changed by design** (recorded in `route_policy_declared.golden`):

- A 405 no longer opens an RLS transaction or asks for a scope.
- A 405 takes the strictest bucket of its path, so changing the method buys no looser bucket.
- CORS preflights pay the general bucket.
- `/x%2F..%2Fshared` rides the analysis bucket, as the share shell it serves does.

**Labels changed** for 363 of the golden's 1,249 requests, all from a collapse or `/api/other` to the matched
pattern:

- Parameters are named as registered: `:id` becomes `{id}`, `{connectionId}`, `{flowId}` and so on.
- Routes that a family collapse used to absorb get labels of their own (`/api/library/portfolio`,
  `/api/orgs/{id}/channels`).
- Dashboards and alerts outside the repository that match the old labels must change with the deploy.

**Cost:** one tree lookup per request, and up to eight more on a 405 (measured in `BenchmarkBuildHandler`).

Until the tables are deleted, both goldens (`route_policy.golden` for the legacy tables, `route_policy_declared.golden`
for the declarations) and the differential test pin that routes decide the same either way.
