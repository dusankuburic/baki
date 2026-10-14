# API conventions

This page covers how an HTTP endpoint takes its input, reports its errors and pages its lists. New endpoints
follow it. Existing ones move over when they are touched and keep their wire shape: the error codes stay the same
by construction, and a list keeps its envelope until its client moves.

## Typed endpoints

An endpoint is a function from a typed request to a typed response, and `api.JSON` or `api.Query` turns it into
the route's handler:

```go
type renameFlowRequest struct {
	FlowID string `json:"-" path:"id"`
	Name   string `json:"name"`
}

func (r renameFlowRequest) FlowRef() string { return r.FlowID }

func (r renameFlowRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return apperr.Invalid("NAME_REQUIRED", "name is required").WithDetails(map[string]any{"field": "name"})
	}
	return nil
}

func (h *LibraryHandler) renameFlow(ctx context.Context, req renameFlowRequest) (libraryFlow, error) {
	header := AuthorizedFlow(ctx) // the stored header AuthorizeFlow loaded; nil in local mode
	// …
}

func (h *LibraryHandler) Mount(rb *route.Builder) {
	rb.Post("/api/library/{id}/rename", JSON(h.renameFlow, AuthorizeFlow(h.security, h.authz, service.FlowEdit)))
}
```

`JSON` takes a JSON body. `Query` is for endpoints without a body (GET, DELETE) and reads only the URL. A request
goes through these steps in order:

1. **Body** (`JSON` only). The body is decoded strictly: an unknown field, an empty body or data after the value
   is a 400 `BAD_REQUEST` that carries the decoder's message. A body over the router's 10 MiB limit gets the 400
   `decodeBody` has always answered.
2. **URL parameters.** Fields tagged `path:"name"` are filled from the route and fields tagged `query:"name"` from
   the query string.
   - A parameter is a string, a bool, an int or a slice of one of those.
   - A slice takes comma-separated and repeated values alike (`?tags=a,b&tags=c`).
   - An empty or absent value leaves the zero value. A malformed one is a 400 that names the parameter, e.g.
     `invalid "limit" parameter: must be an integer`.
   - Unknown query parameters are ignored.
   - Give path fields `json:"-"` so the body schema leaves them out; the route's value wins anyway.
   - A path tag must name one of the route's parameters. The adapter never sees the route's pattern, so it cannot
     check this when it builds the handler; a request to such an endpoint is a 500 before the function runs.
3. **Page bounds** for a request that embeds `PageRequest` (see [Lists](#lists)).
4. **`Validate() error`**, when the request has one. A plain error is a 400 `BAD_REQUEST` with its message; an
   `*apperr.Error` is written as it is.
5. **Flow authorization**, when the request names a flow (see below).
6. **The function**, on the request's own context, so `auth.ActorFromContext`, the RLS transaction and `reqinfo`
   are the request's. It returns before anything is written, so the transaction commits before the first byte
   without the endpoint doing anything for it (ADR 0004).
7. **The answer.** The response is written as JSON with the endpoint's `Status` option: 200 by default,
   `Status(http.StatusCreated)` for a create, and `Status(http.StatusNoContent)` for no body. An error goes
   through `render.Error` (see [Errors](#errors)).

`JSON` and `Query` check the declaration when they build the handler, and panic when the request and the options
do not fit:

- a request that names a flow without `AuthorizeFlow`, or `AuthorizeFlow` on one that names none;
- a tagged field the decoder cannot fill, including one in a struct embedded by pointer (embed it by value);
- `PageLimits` without an embedded `PageRequest`;
- a `Status` outside 2xx.

Routes are mounted when the router is built, so these misdeclarations fail at boot and never serve.

**Flow authorization.** A request that implements `FlowRef() string` names the flow it acts on, and its endpoint
declares `AuthorizeFlow(sec, authz, action)`. Before the function runs, the adapter calls
`AuthzService.AuthorizeFlow` for the request's actor and action:

- a missing flow is 404 `NOT_FOUND`;
- a refusal is 403 `FORBIDDEN`, including a scoped API token that lacks the action's scope even though the route
  admits it;
- the function reads the stored header the check loaded with `AuthorizedFlow(ctx)`.

Local mode has no accounts and one user, whom the flow checks have always let through. There nothing is checked
and `AuthorizedFlow` is nil.

**What stays hand-written.** Some routes do not fit the adapter: streams (SSE, chat), multipart uploads, file and
blob downloads, the WebSocket, and webhooks that read the raw body. `TestArch_RoutesUseTheTypedAdapter` in
`internal/archtest` pins the routes registered with a hand-written handler, per file. A new file starts at zero,
and a file's number only goes down. `auth_avatar.go` holds two: the avatar upload (multipart) and the serving route
(a blob download, which the frontend fetches with the bearer and renders from an object URL, so the SPA's `img-src`
needs `blob:`); the removal beside them is a typed `Query` endpoint.

## Errors

Every error response is the standard envelope, `render.ErrorResponse`:

```json
{"code": "FLOW_LOCKED", "message": "the flow is locked", "requestId": "4f1c…", "details": {"lockedBy": "u-2"}}
```

- **`code`** is stable and SCREAMING_SNAKE, and it is what clients switch on.
- **`message`** is for people and may be reworded. Every 5xx masks it as `internal server error`.
- **`requestId`** is the request's `X-Request-ID`.
- **`details`** holds structured fields the client needs to recover, such as a field name or a resume cursor. It is
  never masked, so it only carries what may be shown.

An endpoint returns its error and `render.Error` writes it:

- **An `*apperr.Error` anywhere in the chain** decides the status (from its kind), the code and the details. The
  wire carries its `Message` only. Its cause (`Wrap`) and any `fmt.Errorf` context are for the logs.
- **A service sentinel** maps as it always has:
  - `ErrNotFound` is 404;
  - `ErrPermissionDenied`, `ErrScopeDenied` included, is 403;
  - `ErrInvalidInput` is 400;
  - `ErrConflict` and `ErrVersionConflict` are 409;
  - `ErrRLSUnavailable` is 503.
- **Any other error** is a 500 `INTERNAL_ERROR`.

A 5xx is logged at ERROR once, cause included, as before; `render.ErrorUnlogged` writes the same bytes without
the log line.

The server's drain refusal is the exception (ADR 0024): `drain.Refuse` answers 503 `SERVER_RESTARTING` with its
fixed message unmasked, `details.retryAfterSeconds` and a `Retry-After` header, and logs nothing. A stopping
server refuses every new chat stream, event stream and socket, and one ERROR per refusal would flood the log at
every deploy.

| Kind | Constructor | Status | Default code |
|---|---|---|---|
| `KindInvalid` | `apperr.Invalid` | 400 | `BAD_REQUEST` |
| `KindUnauthenticated` | `apperr.Unauthenticated` | 401 | `UNAUTHORIZED` |
| `KindForbidden` | `apperr.Forbidden` | 403 | `FORBIDDEN` |
| `KindNotFound` | `apperr.NotFound` | 404 | `NOT_FOUND` |
| `KindConflict` | `apperr.Conflict` | 409 | `CONFLICT` |
| `KindUnprocessable` | `apperr.Unprocessable` | 422 | `UNPROCESSABLE` |
| `KindRateLimited` | `apperr.RateLimited` | 429 | `RATE_LIMITED` |
| `KindInternal` (the zero kind) | `apperr.Internal` | 500 | `INTERNAL_ERROR` |
| `KindNotImplemented` | `apperr.NotImplemented` | 501 | `NOT_IMPLEMENTED` |
| `KindUnavailable` | `apperr.Unavailable` | 503 | `SERVICE_UNAVAILABLE` |

- The default codes are the ones render has always derived from the status, so moving an endpoint onto `apperr`
  keeps its codes, and render's tests pin that.
- Give a specific code when a client must tell a case apart, e.g. `apperr.Conflict("FLOW_LOCKED", …)`, and list it
  in the endpoint's `@Failure` line.
- A package-level `Error` may be shared between requests: `WithDetails` and `Wrap` return copies, and `errors.Is`
  matches a copy by kind and code.
- `apperr` lives in `internal/platform/apperr`, so services may return an `Error` where they know the kind and the
  code the client should see. Where a sentinel already maps right (`ErrNotFound`, `ErrPermissionDenied`, …), keep
  it.

### Provider codes (ADR 0025)

| Code | Where | Meaning |
|---|---|---|
| `PROVIDER_UNKNOWN` | 400 on `POST /api/keys/{save,has,delete}`, `POST /api/chat/stream` and `POST /api/chat/preview-context`; `{ok: false, code}` (200) on `POST /api/providers/test`; the `error` event's `code` on a stream | the provider id is not in the registry (`ai.Specs()`): a typo or a client newer than the server, never an outage |
| `USE_PROVIDER_CONFIG` | 400 on `POST /api/keys/save` | the provider stores a document, not a bare key: use its own config route |
| `PROVIDER_NOT_ALLOWED` | the `error` event's `code` on a stream; `{ok: false, code}` (200) on `POST /api/providers/test`, the org test and the Azure AI Foundry resource/model tests; 400 on the knowledge routes (`upload`, `reindex`) and on `POST /api/chat/preview-context` | the provider policy refused the provider before any key was read (ADR 0028): disabled in the deployment's AI settings, or off the organisation's `allowedProviders` list; the message says which and what to do |
| `PROVIDER_POLICY_UNAVAILABLE` | `{ok: false, code}` (200) on the org test and the Azure AI Foundry tests; 503 on the knowledge routes (`upload`, `reindex`) | the provider policy could not be read (the organisation's settings document failed to load): nothing was admitted or refused; the message is fixed copy, the cause is in the log (ADR 0028) |
| `AI_BUDGET_EXCEEDED`, `AI_BUDGET_UNAVAILABLE` | `{ok: false, code}` (200) on `POST /api/providers/test` and the org test (the stream codes are unchanged) | the daily AI budget gate in the provider chain refused the test's billed call (ADR 0028); nothing was sent |

`ProviderTestResult.code` is optional and empty for the free-text failures the test has always reported.

### Conversation codes (ADR 0031)

| Code | Where | Meaning |
|---|---|---|
| `CONVERSATION_CONFLICT` | 409 on `POST /api/chat/save` and `POST /api/chat/clear` that send `baseVersion` | the stored conversation is no longer at `baseVersion` (another tab, another replica or the server's own turn changed it); nothing was written. `details.version` is the current version. The client reloads with `POST /api/chat/get`, merges by message id and sends again, once |

`POST /api/chat/get` returns `version` (0 for a conversation not stored yet; absent where conversations are files),
and a versioned save answers `{status, version}`. A save or clear without `baseVersion` is unconditional, as before.

A turn the server stores itself (cloud mode, the request carried no history) stores its answer **before** it emits
`done`, and the `done` and `error` events of the stream then also carry `conversationVersion` (the version after the
server's own writes) and `conversationBase` (the version the turn began from), additive fields that older clients
ignore. They are absent for a client-owned turn, in file storage, when "Save conversation history" is off, and when
another writer changed the thread between the question and the answer. A client still holding `conversationBase`
adopts `conversationVersion` and has nothing to save; otherwise it keeps its version and the next write conflicts
and reconciles (ADR 0031, addendum).

### Stream command codes (ADR 0033)

| Code | Where | Meaning |
|---|---|---|
| `REPLICA_UNREACHABLE` | 503 on `POST /api/chat/cancel` and `POST /api/chat/fix-decision`; `Retry-After` and `details.retryAfterSeconds` say when | the stream runs on another server of a multi-server deployment and this one could not get an answer from it (Redis is down, the owner is gone or did not answer in time). Nothing is known to have been stopped or decided; the request is safe to send again. A stop or decision for a stream on the server that got the request never answers it. A stop for a stream mirrored by a server of the release before multi-server stop (its snapshot names no server) answers 403 `STREAM_NOT_FOUND` instead, which clients take as gone. The message is masked like any 5xx; switch on the code |

## Lists

**The response is `api.Page[T]`:**

```json
{"items": [{"id": "f-1", "name": "Invoices"}], "nextCursor": "eyJ1IjoiMjAyNi0xMC0wNyIsImlkIjoiZi0xIn0", "total": 120}
```

- `items` is never null.
- `nextCursor` is the opaque position of the next page, and it is absent on the last page.
- `total` is the size of the whole filtered list. It is absent when the endpoint does not count, because counting
  costs a second query, so count only for a UI that shows it.

**The request embeds `api.PageRequest`:**

```go
type listFlowsRequest struct {
	PageRequest
	OrgID string   `query:"orgId"`
	Tags  []string `query:"tags"`
	Sort  string   `query:"sort"`
}
```

- `?limit=` is the page size. An absent limit, or one below 1, is the default (50), and one above the maximum (200)
  is the maximum. `PageLimits(defaultLimit, maxLimit)` sets an endpoint's own. Out-of-range limits are clamped
  rather than refused, as the existing lists do.
- `?cursor=` is the previous page's `nextCursor`.

**Cursors:**

- A cursor is opaque to the client: base64url of the position in the endpoint's order, which is the last item's
  sort key with its id as the tie-breaker (keyset paging). Clients never build or parse one.
- A cursor is valid only with the filters and sort it was issued for. A malformed one is a 400 `BAD_REQUEST`.
- New endpoints take no offsets. An offset re-reads every skipped row, and it skips or repeats items when the list
  changes between pages.

**Sort:**

- `?sort=field` sorts ascending and `?sort=-field` descending, on one field; the id is always the tie-breaker.
- The field names are the item's JSON names (`sort=-updatedAt`, `sort=name`).
- The endpoint documents the fields it accepts and its default order, which is usually newest first. Any other
  field is a 400 `BAD_REQUEST`.

**Filters:**

- There is one query parameter per filter, named like the item's JSON field (`orgId`, `role`, `action`).
- Several values of one filter mean any of them, comma-separated or repeated (`?tags=a,b`, `?tags=a&tags=b`).
- `q` is the free-text search, bounded in length.
- A range is `from` and `to`. Dates are `YYYY-MM-DD`, both inclusive, as the analytics filters take them.
- A filter's values are validated: an unknown value of an enumerated filter is a 400, as `role` on the admin user
  list is.

**Today's paged endpoints** keep their shapes until they are touched together with their client.
`render.PagedResponse` stays for the endpoints that return it, and new lists return `Page[T]`.

- `GET /api/admin/users/list`
  - Today: `render.PagedResponse` (`{items, total, offset, limit}`), newest first. `limit` is 50 by default and
    at most 100, with an `offset`. The filters are `q` and `role`.
  - Maps to: `Page[T]` with `total`, `PageLimits(50, 100)` and a cursor on (created, id). The filters stay.
- `GET /api/admin/audit`
  - Today: a bare array, newest first, with `limit` and `offset`. The admin page asks for one row more than it
    shows to learn whether another page exists.
  - Maps to: `Page[T]` without `total`, where `nextCursor` replaces the extra row. The `action` and `userId`
    filters stay.
- `GET /api/library`
  - Today: `render.PagedResponse`. `limit` is 200 by default and at most 200 (`clampListLimit`), with an
    `offset`. `sort` is `updated_desc`, `updated_asc`, `name_asc`, `name_desc` or `blocks_desc`, and an unknown
    value means `updated_desc`. The filters are `q`, `orgId`, `scope` and `tags` (comma-separated).
  - Maps to: `Page[T]` with `total`. `sort` becomes `-updatedAt`, `updatedAt`, `name`, `-name` or `-blockCount`,
    and an unknown field is a 400.
- `GET /api/governance/alerts` and `GET /api/orgs/{id}/knowledge`
  - Today: bare arrays, with `limit` and `offset` (`clampListLimit`).
  - Maps to: `Page[T]`.
- `GET /api/flows/{flowId}/collaborators` and `GET /api/library/{id}/versions`
  - Today: bare arrays cut at `limit`, without a next page.
  - Maps to: `Page[T]`, once they page.
- `GET /api/library/portfolio`
  - Today: an object of entries and rollups. `sort` is `risk`, `health`, `errors`, `name` or `analyzed`, and
    `limit` trims the entries.
  - Maps to: nothing. It is not a page, because the rollups cover every flow, so it keeps its shape.

## Swagger

Annotate the function the adapter wraps, and name the request and response types themselves:

```go
// @Summary      Rename a flow
// @Tags         library
// @Accept       json
// @Produce      json
// @Param        id      path string            true "Flow ID"
// @Param        request body renameFlowRequest true "The new name"
// @Success      200 {object} libraryFlow
// @Failure      400 {object} render.ErrorResponse "BAD_REQUEST, or NAME_REQUIRED with details.field"
// @Failure      403 {object} render.ErrorResponse "FORBIDDEN"
// @Failure      404 {object} render.ErrorResponse "NOT_FOUND"
// @Router       /api/library/{id}/rename [post]
func (h *LibraryHandler) renameFlow(ctx context.Context, req renameFlowRequest) (libraryFlow, error) {
```

- **Body:** `@Param request body <Req> true`. Fields tagged `json:"-"`, such as path parameters, stay out of the
  schema.
- **Response:** `@Success 200 {object} <Resp>`. A page is `{object} api.Page[api.<Item>]`, which swag names
  `api.Page-api_<Item>`, with `items`, `nextCursor` and `total`.
- **Errors:** `{object} render.ErrorResponse`, with the codes listed in the description.
- **Query and path parameters:** one `@Param` line each, e.g. `@Param limit query int false "Page size (default 50,
  at most 200)"`; a list takes `collectionFormat(csv)`. swag can expand a request struct
  (`@Param request query <Req> false`), but it names the parameters after their `json` or `form` tags or their
  field names, never their `query` tags. A field `OrgID` tagged `query:"orgId"` would be documented as `orgID`.

These annotations were checked against a sample endpoint with swag v1.16.4, writing outside the tree:
`swag init -g main.go -o "$TEMP/swag_verify" --parseDependencyLevel 1`. The output directory's name must be a Go
identifier. Never edit `docs/swagger.*` by hand: the OpenAPI Freshness CI job regenerates them, and a changed
annotation means regenerating and committing them.
