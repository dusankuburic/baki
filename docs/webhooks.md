# Webhooks and notification channels

Baki sends governance events to notification channels: an organisation's own channels, which its admins configure in
Settings > Organisations, and the deployment's channels, which the operator configures with `PAD_NOTIFY_*`. This page
is the receiver's reference: which events are sent, what each request carries, how to verify the signature and how
deliveries are retried. The design is [ADR 0021](adr/0021-channel-kinds-and-webhook-envelope.md).

## Channel kinds

| Kind | Organisation | Deployment | Sends |
|---|---|---|---|
| `webhook` | yes | `PAD_NOTIFY_WEBHOOK_URL`, `PAD_NOTIFY_WEBHOOK_SECRET` | the event as JSON (below), signed when the channel has a secret |
| `slack` | yes | `PAD_NOTIFY_SLACK_URL`, `PAD_NOTIFY_SLACK_SECRET` | a Slack incoming-webhook attachment, which Discord and Mattermost render too; signed when the channel has a secret |
| `teams` | yes | `PAD_NOTIFY_TEAMS_URL` | an Office 365 `MessageCard` for a Teams incoming webhook; never signed. Microsoft is retiring these connectors: use `teams-workflow` |
| `teams-workflow` | yes | `PAD_NOTIFY_TEAMS_WORKFLOW_URL` | an Adaptive Card message for a Teams Workflows webhook; never signed |
| `jira` | yes: base URL, account email, API token, project key | `PAD_NOTIFY_JIRA_BASE_URL`, `_EMAIL`, `_API_TOKEN`, `_PROJECT` | a Jira Cloud issue per event (REST API v3) |
| `email` | yes: members the admin chooses, at their verified addresses | `PAD_NOTIFY_EMAIL_TO` with the SMTP settings | one email per event (per recipient on an organisation's channel) |

**The deployment's channels receive every tenant's events**, flow names included: every organisation's and every
user's drift and regression alerts, and the `analysis_complete` event of every interactive analysis. Point them at
the operator's own sinks. An organisation's channels receive that organisation's events only.

## Events

| `type` | Sent when | Organisation channels | Deployment channels |
|---|---|---|---|
| `drift` | the periodic scan finds findings that a flow's accepted baseline does not have | yes | yes |
| `health_regression` | a flow's health score drops below its last recorded score | yes | yes |
| `new_rules` | rules newer than the flows' baselines report findings: one notice per organisation and rule set, sent once, at the end of the scan that raised it | yes | only the deployment's own notice, on a deployment without accounts |
| `analysis_complete` | an interactive analysis finishes | no | yes |
| `test` | an admin presses Test on the channel | the tested channel only | no |

Each inbox alert is sent once: across restarts and replicas, the inbox row decides. A drift or regression whose
counts return to an earlier value is not sent again.

An organisation's channel can choose which of `drift`, `health_regression` and `new_rules` it receives (`events`,
below); one that never chose receives all three, as every channel did before the choice existed. A newer event type
is one a channel opts into. The `test` event reaches every channel.

## The request

Every channel receives an HTTP `POST` with a JSON body. These headers are on every request of the `webhook`,
`slack`, `teams`, `teams-workflow` and `jira` kinds:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `baki/<version>` |
| `X-Baki-Event` | the event `type` |
| `X-Baki-Delivery` | a UUID per event and channel, the same on every retry of that delivery |
| `X-Baki-Timestamp` | the attempt's time, Unix seconds |
| `X-Baki-Signature-V2` | `sha256=<hex HMAC-SHA256(secret, "<X-Baki-Timestamp>." + body)>`: `webhook` and `slack` channels with a secret |
| `X-Baki-Signature` | `sha256=<hex HMAC-SHA256(secret, body)>`: the same channels. **Deprecated**: it does not cover the time, so a captured request verifies forever. It is still sent; no removal date is set |

### The `webhook` body (schema version 1)

```json
{
  "schemaVersion": 1,
  "id": "evt_8b26b52dca786c5f56fe0430",
  "deliveryId": "6f1d2a6e-0f7a-4c8e-9a51-2a4f0b3c9d10",
  "orgId": "c0ffee00-1111-2222-3333-444455556666",
  "type": "drift",
  "flowId": "flow-1",
  "flowName": "Invoice Bot",
  "title": "New findings in Invoice Bot",
  "message": "2 new finding(s) since baseline (1 error, 1 warning).",
  "newErrors": 1,
  "newWarnings": 1,
  "at": "2026-10-01T12:00:00Z"
}
```

| Field | Present | Meaning |
|---|---|---|
| `schemaVersion` | always | `1`. A field is only ever added to version 1; a breaking change gets a new version, opt-in per channel |
| `id` | always | the event's id: the same on every channel, retry and replica. **Dedupe on it** |
| `deliveryId` | always | equals `X-Baki-Delivery` |
| `orgId` | organisation channels | the organisation the channel belongs to |
| `type`, `title`, `message`, `at` | always | `at` is when the event was raised (RFC 3339, UTC) |
| `flowId` | always | `""` for `test` and `new_rules` events, and for `analysis_complete` |
| `flowName` | when known | |
| `newErrors`, `newWarnings` | `drift` | non-zero counts only |
| `healthScore`, `prevHealth` | `health_regression` | |
| `analysis` | `analysis_complete` | `{errors, warnings, info, healthScore?}` |
| `ruleIds`, `flowCount` | `new_rules` | the rules that report findings, and in how many flows |

Fields are flat for compatibility with receivers written before the envelope: ignore the ones you do not know.

## Verifying a delivery

Configure a secret on the channel, then on each request:

1. Read the raw body bytes, before any JSON parsing.
2. Refuse a `X-Baki-Timestamp` more than five minutes from your clock.
3. Compute `sha256=` + hex HMAC-SHA256 of `<timestamp>.<body>` with the secret, and compare it with
   `X-Baki-Signature-V2` in constant time.

```python
import hashlib, hmac, time

def verify(secret: bytes, headers, body: bytes) -> bool:
    ts = headers["X-Baki-Timestamp"]
    if abs(time.time() - int(ts)) > 300:
        return False
    want = "sha256=" + hmac.new(secret, ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, headers["X-Baki-Signature-V2"])
```

In Go, `notify.VerifySignature(secret, timestamp, signature, body, time.Now())` does the same. Dedupe on the body's
`id` to drop an event you already processed.

A Power Automate "When an HTTP request is received" trigger cannot check an HMAC by itself; keep its URL secret (it
is the credential) or verify in an Azure Function in front of the flow.

## Retries and timing

- Each delivery makes up to three attempts, 0.5 s and then 1 s apart. A network error, a `429` or a `5xx` is
  retried; any other status is final. `Retry-After` is not read.
- Each attempt times out after 10 s, and the whole delivery, retries included, has 10 s (the channel test 15 s).
- Redirects are not followed: a `3xx` is a failed delivery.
- Delivery is at most once and in process: an event raised just before the server stops may not be sent. There is
  no outbox and no delivery log; failures are in the server log and its error reporting, never with the URL's path
  or query.

## Where a channel may point

- `https://` only, except loopback: `http://localhost` or a literal loopback address is accepted for an
  organisation's channels only on a deployment without accounts, and for the deployment's own channels
  (`PAD_NOTIFY_*`) outside cloud mode.
- Any other host that resolves to a private, loopback or link-local address is refused, in every mode, when the
  channel is saved (or the server boots) and again when it is dialled.
- Behind `HTTPS_PROXY` a delivery fails closed unless the proxy is on a public address; behind a proxy on a private
  address, list the channel hosts in `NO_PROXY`.
- `PAD_NOTIFY_ALLOWED_HOSTS` (exact hosts or `*.suffix` globs, comma-separated) restricts an organisation's channels
  to those hosts: a save to another host is refused with `400 INVALID_CHANNEL` (`details.field: "url"`, the message
  names the host and not the list), and a saved channel whose host left the list is not delivered to (the scanner
  skips it and logs it, the test answers `400`, and the client refuses the host before it dials). It is checked before
  the host is resolved. Unset, any public host is allowed. The deployment's own channels (`PAD_NOTIFY_*`) are the
  operator's and are not restricted. It narrows, never widens: a target must also pass the rules above and a kind's
  own host list (no kind has one today). A shared host such as `hooks.slack.com` or
  `prod-NN.<region>.logic.azure.com` receives for every customer of that service, so listing it restricts the
  service, not the workspace; per-tenant hosts (`<tenant>.webhook.office.com`,
  `<env>.environment.api.powerplatform.com`) can be listed exactly. A glob over a public suffix or a multi-tenant
  hosting zone, or an IP literal, fails the boot. Organisation admins are told that a list applies; they see the
  list itself only with `PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE=true`, since it may name other customers' own hosts.
- An organisation's email channel has no URL: it goes to members of the organisation the admin chose, each at the
  verified address of their account, never to a typed address.

## An organisation's channels through the API

`GET /api/orgs/{id}/channels` (members), `POST /api/orgs/{id}/channels` (admins: create, or edit with `id`),
`DELETE /api/orgs/{id}/channels/{channelId}`, `POST /api/orgs/{id}/channels/{channelId}/test` and
`GET /api/orgs/{id}/channels/options` (admins: the kinds this deployment delivers, `email` only with mail settings;
`events` and `defaultEvents`; `hostsRestricted`, and `allowedHosts` when the operator shows the list).

- The URL is write-only: the list shows `host/…` (no scheme, so it is never a valid URL itself) to everyone, admins
  included. An edit keeps the stored URL when `url` is empty or equals that listed form, with or without
  `urlUnchanged: true`. `urlUnchanged` with any other `url`, and any other `url` containing `…` (or `%E2%80%A6`), are
  refused with `400 INVALID_CHANNEL` (`details.field: "url"`): enter the full URL to change it.
- The secret is write-only: `hasSecret` says whether one is set (always `false` on `teams` and `teams-workflow`,
  which sign nothing). An edit keeps it when `secret` is omitted or empty;
  `"clear": ["secret"]` removes it.
- An edit keeps `name` and `enabled` when they are omitted.
- `jira` takes `url` (the site, `https://<site>.atlassian.net`), `email` (the account the API token belongs to),
  `token` (write-only like the secret: `hasToken` says it is set, an edit keeps it when omitted or empty; it is
  required, so `clear` refuses it) and `project` (the project key, `PAD`). An edit keeps `email` and `project` when
  omitted.
- `email` takes `recipients`: user ids of members whose account address is verified, at most 50 (another id is
  refused on `recipients`, alike whether the user is not a member or not verified). An edit keeps them when omitted.
  A recipient who leaves the organisation stays listed and is not mailed; an erased account leaves every list. The
  test answers once the messages are queued.
- `events` chooses what the channel receives, one or more of `drift`, `health_regression` and `new_rules`; an edit
  keeps the choice when omitted, a create without it receives all three. The list shows the effective `events`.
- A channel whose stored URL cannot be decrypted (the encryption key changed) is listed with `unreadable: true` and
  an empty `url`, is not delivered to, and its test answers `400 CHANNEL_UNREADABLE`; remove it, or save it with the
  URL (and any required secret) entered again.
- A failed test answers `502` with `code: CHANNEL_DELIVERY_FAILED` and `details.reason` (and `details.status`, the
  endpoint's HTTP status); the test has its own rate limit (`PAD_RATE_LIMIT_CHANNEL_TEST_*`).

## Other kinds' payloads

- **Slack**: `{"text": title, "attachments": [{"color", "text": message, "fields": [...], "footer", "ts"}]}`.
- **Teams**: a `MessageCard` with the title, the message and facts. Microsoft is retiring the Office 365 connectors
  behind Teams incoming webhooks in favour of Workflows: create a `teams-workflow` channel and remove the `teams` one.
- **Teams Workflows**: `{"type": "message", "attachments": [{"contentType":
  "application/vnd.microsoft.card.adaptive", "contentUrl": null, "content": <an Adaptive Card 1.4>}]}`, what the Teams
  "Post to a channel when a webhook request is received" workflow posts (create it from the channel's Workflows
  menu and paste its URL). The card says what the `MessageCard` says: the title (red with errors, amber otherwise),
  the message, and one `name: value` line per fact. Every text is a `TextRun` of a `RichTextBlock`, which Adaptive
  Cards never render as Markdown: a flow named `[click](https://evil.example)` shows as typed, not as a link (a
  `TextBlock` or a `FactSet` would render it, and backslash escapes are not honoured alike on every Teams client).
  The URL's `sig` parameter is the credential. Not verified against a live Teams tenant.
- **Jira**: `POST {base}/rest/api/3/issue` with Basic auth (email and API token), the title as the summary, a `Bug`
  for an event with errors and a `Task` otherwise, and the description in Atlassian Document Format (one paragraph
  per line). Not verified against a live Jira Cloud.
- **Email**: `[TYPE] title` as the subject, a plain-text and an HTML body (the `governance_alert` template); an
  organisation's channel sends each recipient their own message, paced by the mail runner.
