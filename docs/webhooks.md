# Webhooks and notification channels

The receiver's reference: channel kinds, events, the request body and headers, signature verification, retries and where a channel may point. Design: [ADR 0021](adr/0021-channel-kinds-and-webhook-envelope.md).

## Channel kinds

- `webhook` — `PAD_NOTIFY_WEBHOOK_URL` + `PAD_NOTIFY_WEBHOOK_SECRET`: the event as JSON (below), signed with the secret.
- `slack` — `PAD_NOTIFY_SLACK_URL` + `PAD_NOTIFY_SLACK_SECRET`: a Slack incoming-webhook attachment, signed with the secret.
- `teams` — `PAD_NOTIFY_TEAMS_URL`: an Office 365 `MessageCard`, never signed; Microsoft is retiring it, use `teams-workflow`.
- `teams-workflow` — `PAD_NOTIFY_TEAMS_WORKFLOW_URL`: an Adaptive Card, never signed.
- `jira` — `PAD_NOTIFY_JIRA_BASE_URL` / `_EMAIL` / `_API_TOKEN` / `_PROJECT`: a Jira Cloud issue per event (REST API v3).
- `email` — `PAD_NOTIFY_EMAIL_TO` + the SMTP settings: one email per event (per recipient on an org channel).

Every kind also exists as an organisation channel (Settings > Organisations). **The deployment's channels receive every tenant's events**, flow names included; an organisation's channels receive only its own.

## Events

`drift` (the scan finds findings a flow's accepted baseline lacks; org + deployment channels) · `health_regression` (a flow's health score drops below its last recorded score; org + deployment) · `new_rules` (rules newer than the baselines report findings, once per org and rule set; org channels, plus the deployment's own notice) · `analysis_complete` (an interactive analysis finishes; deployment channels only) · `test` (an admin presses Test; the tested channel only). An organisation channel chooses among `drift`, `health_regression` and `new_rules` (`events`); one that never chose receives all three. Each alert is sent once — the inbox row decides, across restarts and replicas.

## The request

HTTP `POST`, JSON body. Headers on every `webhook`, `slack`, `teams`, `teams-workflow` and `jira` request:

| Header | Value |
|---|---|
| `Content-Type` / `User-Agent` | `application/json` / `baki/<version>` |
| `X-Baki-Event` | the event `type` |
| `X-Baki-Delivery` | a UUID per event and channel, the same on every retry |
| `X-Baki-Timestamp` | the attempt's time, Unix seconds |
| `X-Baki-Signature-V2` | `sha256=<hex HMAC-SHA256(secret, "<X-Baki-Timestamp>." + body)>` — `webhook`/`slack` with a secret |
| `X-Baki-Signature` | `sha256=<hex HMAC-SHA256(secret, body)>` — same channels; **deprecated** (covers no time), still sent |

`webhook` body (schema version 1):

```json
{"schemaVersion": 1, "id": "evt_8b26b52dca786c5f56fe0430",
 "deliveryId": "6f1d2a6e-0f7a-4c8e-9a51-2a4f0b3c9d10",
 "orgId": "c0ffee00-1111-2222-3333-444455556666", "type": "drift",
 "flowId": "flow-1", "flowName": "Invoice Bot", "title": "New findings in Invoice Bot",
 "message": "2 new finding(s) since baseline (1 error, 1 warning).",
 "newErrors": 1, "newWarnings": 1, "at": "2026-10-01T12:00:00Z"}
```

- `id`: the event's id, the same on every channel, retry and replica — **dedupe on it**; `deliveryId` equals `X-Baki-Delivery`; `orgId` on organisation channels; `type`, `title`, `message`, `at` (RFC 3339, UTC) always; `flowId` is `""` for `test`, `new_rules` and `analysis_complete`; `flowName` when known.
- `newErrors`/`newWarnings` (`drift`, non-zero only); `healthScore`/`prevHealth` (`health_regression`); `analysis` `{errors, warnings, info, healthScore?}` (`analysis_complete`); `ruleIds`/`flowCount` (`new_rules`). Fields are flat — ignore unknown ones; within version 1 fields are only ever added, a breaking change gets a new version, opt-in per channel.

## Verifying a delivery

1. Read the raw body bytes, before any JSON parsing.
2. Refuse an `X-Baki-Timestamp` more than five minutes from your clock.
3. Compare `sha256=` + hex HMAC-SHA256 of `<timestamp>.<body>` with `X-Baki-Signature-V2` in constant time, then dedupe on the body's `id`. In Go, `notify.VerifySignature(secret, timestamp, signature, body, time.Now())` does the same. A Power Automate HTTP trigger cannot check an HMAC itself — keep its URL secret (it is the credential) or verify in an Azure Function in front.

## Retries and timing

Up to three attempts (0.5 s, then 1 s apart); a network error, `429` or `5xx` retries, anything else is final (`Retry-After` is not read); 10 s per attempt and 10 s per whole delivery (channel test 15 s); redirects are not followed. At most once, in process: an event raised just before the server stops may not be sent; no outbox, no delivery log.

## Where a channel may point

- `https://` only (loopback `http://` only on local deployments); a host resolving to a private, loopback or link-local address is refused at save/boot and again when dialled. Behind `HTTPS_PROXY` on a private address, list channel hosts in `NO_PROXY`.
- `PAD_NOTIFY_ALLOWED_HOSTS` (exact hosts or `*.suffix` globs) restricts organisation channels: another host fails save with `400 INVALID_CHANNEL` (`details.field: "url"`), and a saved channel whose host left the list is not delivered to; a glob over a public suffix or multi-tenant zone, or an IP literal, fails the boot. Per-tenant hosts (`<tenant>.webhook.office.com`) can be listed exactly; a shared host (`hooks.slack.com`) restricts the service, not the workspace. The deployment's own channels are not restricted. An organisation email channel has no URL — it mails chosen members at their verified addresses only.

## An organisation's channels through the API

`GET /api/orgs/{id}/channels` (members), `POST` (admins: create, or edit with `id`), `DELETE …/{channelId}`, `POST …/{channelId}/test`, `GET …/options` (the kinds this deployment delivers). URL and secret are write-only: the list shows `host/…`; an edit keeps a stored URL when `url` is empty or the listed form (enter the full URL to change it) and keeps the secret when omitted (`"clear": ["secret"]` removes it); `hasSecret`/`hasToken` report set-ness. `jira` takes `url`/`email`/`token`/`project`; `email` takes `recipients` (member ids with verified addresses, at most 50); `events` chooses among `drift`, `health_regression`, `new_rules` (a create without it receives all three). A channel whose URL cannot be decrypted lists `unreadable: true`, is not delivered to and tests `400 CHANNEL_UNREADABLE`; a failed test answers `502 CHANNEL_DELIVERY_FAILED` with `details.reason`.

## Other kinds' payloads

- **Slack**: `{"text": title, "attachments": [{"color", "text": message, "fields": […], "footer", "ts"}]}`.
- **Teams**: a `MessageCard` (title, message, facts). **Teams Workflows**: `{"type": "message", "attachments": [{"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": null, "content": <Adaptive Card 1.4>}]}` — every text is a `TextRun` of a `RichTextBlock`, so a flow name never renders as a link; the URL's `sig` parameter is the credential.
- **Jira**: `POST {base}/rest/api/3/issue`, Basic auth (email + API token), title as summary, `Bug` when the event has errors else `Task`, description in Atlassian Document Format. **Email**: `[TYPE] title` subject, plain-text and HTML body (`governance_alert` template); per-recipient on an organisation channel.
