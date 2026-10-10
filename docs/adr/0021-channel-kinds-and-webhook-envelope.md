# 21. Channel kinds are a registry, channel secrets are write-only, and webhooks carry a versioned envelope

Date: 2026-10-10. Status: accepted (Phase 4c).

## Context

Five notification kinds (`webhook`, `slack`, `teams` for organisations and the deployment; `jira` and `email` for the
deployment) were known in about twenty places: a kinds slice and a switch in `notify`, a builder switch that assumed
`(kind, url, secret)`, per-kind config fields and one hand-written block per kind in `notify.New`, the API's kind
check and URL redaction, the scanner's builder call, and the frontend. The org channel API wiped the HMAC secret on
every edit and every pause (B1), re-enabled a paused channel on edit (B2) and answered "now" as the creation time
(B10); org channels received `"at":"0001-01-01T00:00:00Z"` (B3); a channel URL, the credential for Slack and Teams,
went into WARN logs and the error sink (B4); the Jira description was a plain string, which REST API v3 refuses (B5);
three predicates picked the egress policy and disagreed in local mode with accounts (B8); the channel test shared
the general rate-limit bucket and answered with transport errors (B13). The generic webhook's body had no id,
delivery id, organisation or schema version, and its signature covered the body only, so a captured request
verified forever. ADR 0016 promised `new_rules` notices to the channels in this phase.

The channel secrets are already sealed at rest with the keystore's key and AEAD (ADR 0005); moving them into the
keystore tables would add a cross-store write without a transaction and lender semantics that do not fit.

## Decision

1. **A registry, one file per kind.** `internal/notify/kinds_<id>.go` registers a `KindSpec`: its frozen `ID`, a
   `Scope` (organisation, deployment), its `Fields` (name, `Secret`, `Required`, `Check`), an `EgressSpec`, whether
   it is `Signed`, `Build` and `Public` (what the API may show). `Register` refuses an incomplete spec, a duplicate
   id, and a proxy-trusting egress without a host list. `notify.Validate`, `notify.Build` and `notify.PublicValues`
   read the spec. An organisation kind's fields beyond `url` and `secret` live in `org_channels.config` (public) and
   `.secrets` (sealed) (13); a field may belong to one scope only (`FieldSpec.Scope`: the email kind's `to` is the
   deployment's, its `recipients` an organisation's).
2. **Deployment channels come from one table in `internal/config`** (`GovernanceConfig.DeploymentChannels`), env
   names unchanged; `notify.New` builds them through the registry. The legacy `PAD_WEBHOOK_URL`/`_SECRET` alias of
   the Slack channel moved into config (`ResolveLegacyAliases`, applied on every load path as before).
3. **`service.ChannelService` owns the organisation channels**: the kind check, validation under the deployment's
   egress policy, redaction, the edit semantics, the test delivery and the delivery runner (`lifecycle.Bounded
   ("notify")`, 32), which replaces the scanner's semaphore and goroutines and the test endpoint's goroutine. The
   scanner waits for a slot (`Bounded.GoWait`) instead of dropping an alert it recorded; a channel test is refused
   with 503 when the runner is full. `internal/api` no longer imports `notify`; the four routes are typed endpoints.
4. **Secrets stay in the row, sealed, and are write-only.** An edit keeps what it omits: the secret (`secret`
   omitted or empty), `enabled`, `name` and the creation time; `"clear": ["secret"]` removes the secret. The URL is
   write-only for admins too: the list shows `host/…` to everyone (`notify.ListedURL`). It has no scheme on
   purpose: a listing that is itself a valid `https` URL (`https://host/…`) is stored as the real URL by whatever
   re-sends it after the channel changed, a tab listing it before another admin's edit, a pasted listing, or the
   previous release, which validates https and a public host. An edit keeps the stored URL when `url` is empty or
   equals the current listing, with or without `urlUnchanged: true` (a tab of the previous release re-sends what it
   was shown). `urlUnchanged` with any other `url`, and any other `url` carrying the marker `…` (also
   percent-encoded, `%E2%80%A6`), are a 400 `INVALID_CHANNEL` on `url`: the placeholder is never stored. Logs and
   errors keep `notify.RedactURL`'s `scheme://host/…`. `hasSecret` is added to the channel, from the kind's
   declared fields (a Teams row's legacy secret column lists `false`). A secret on a `teams` or `teams-workflow`
   channel is refused (Teams cannot verify it). The fake keeps `CreatedAt` on update (B9), and the field-encryption
   backfill skips empty URLs (B11).
5. **One egress answer per owner of the URL:** `egress.ForDeployment(capability.Set)` for an organisation's
   channels, which another user configured: with accounts `BlockInternal`, without `AllowLoopback`. Channel saves,
   the scanner's deliveries and the channel test use it (B8). The deployment's own channels (`PAD_NOTIFY_*`) are the
   operator's configuration, not a tenant's input: `egress.ForOperator` allows loopback outside the cloud profile
   and refuses internal targets in it, as before. Private and link-local addresses are refused by both. Each kind has an `EgressSpec` (none restricts hosts today, so nothing changes); `TrustProxy` is only legal
   with `Hosts`, and then the proxy hook refuses any other host. The AI endpoints keep their own predicate until
   Phase 4b.
6. **The envelope, v1 additive.** Every HTTP delivery carries `X-Baki-Event`, `X-Baki-Delivery` (per event and
   channel, the same across retries), `X-Baki-Timestamp` (per attempt) and `User-Agent: baki/<version>`; a signed
   kind with a secret adds `X-Baki-Signature-V2: sha256=<hex HMAC(secret, "<ts>." + body)>`, the inbound CI
   webhook's canonical string, and keeps `X-Baki-Signature` (body only; deprecated, no removal date). The webhook
   body adds `schemaVersion: 1`, `id`, `deliveryId` and, on an organisation's channel, `orgId` to the flat event;
   `new_rules` events add `ruleIds` and `flowCount`. The event `id` is `evt_` and 24 hex characters: of the SHA-256
   of the inbox row id for an alert and of the notice id for a notice (so every channel, retry and replica sends the
   same id), random for `analysis_complete` and `test`. `at` is stamped when the event is raised (B3).
   `notify.VerifySignature` is the receiver's check; [docs/webhooks.md](../webhooks.md) documents the schema.
7. **`new_rules` reaches the channels** once per notice, at the last flush of the sweep that inserted it, with the
   sweep's full counts: an organisation's notice to its channels, the deployment's own notice (a deployment without
   accounts) to the deployment's channels. A user's notice reaches no channel. Which of its events an organisation's
   channel receives is its subscription (14).
8. **Logs and error reports never carry a URL's path or query** (B4): every error leaving `notify` is redacted
   (`RedactError`, `RedactURL`), validation errors name the redacted URL, and the channel test answers 502
   `CHANNEL_DELIVERY_FAILED` with `details.reason` (the endpoint's status, or `egress.DescribeFailure`'s fixed text)
   on its own rate-limit bucket `channel_test` (B13).
9. **Jira's description is Atlassian Document Format** (one paragraph per line of the plain body) (B5). It is not
   verified against a live Jira Cloud.
10. **Safety nets first.** A payload golden per kind and event (`internal/notify/testdata/payloads`, bytes and
    headers with a fixed clock and ids, `-update-notify-golden`), tests that failed on B1, B2 and B3 before the fix,
    and contract scenarios for the secret round trip, `enabledOnly` and an update keeping `created_at`.
11. **An operator host allow-list for organisation channels**, `PAD_NOTIFY_ALLOWED_HOSTS`: exact hosts or `*.suffix`
    globs, validated at boot by the rules of `PAD_AZURE_FOUNDRY_ALLOWED_HOSTS` (no glob over a public suffix or a
    multi-tenant hosting zone, where any tenant admin could stand up a receiver; no IP literal; nothing URL-shaped).
    It is `EgressSpec.Allowed`, the deployment's half of the kind's `Hosts`: a target must be on both, so it narrows a
    kind and never widens one. `ChannelService` passes it to `notify.Validate` at save (400 `INVALID_CHANNEL` on
    `url`, checked before the host is resolved, naming the host but not the list, which may name other customers'
    hosts) and to `notify.Build` at every delivery (the scanner skips a channel whose host left the list, the test
    answers 400), and the client of the class refuses an off-list host in its `Proxy` hook, which `net/http` runs for
    every request before it dials (the hook the proxy-trusting kinds already used, now `restrictHosts` for any list).
    Hosts are compared in the form the transport dials (IDNA-mapped when not ASCII). It does not change the proxy
    trust: an organisation channel stays on the strict guard. The deployment's own channels are the operator's and are
    not restricted; unset is the behaviour before it. Admins see the list only when the operator opts in (15).
12. **A `teams-workflow` kind** (org and deployment, `PAD_NOTIFY_TEAMS_WORKFLOW_URL`), because Microsoft is retiring
    the Office 365 connectors behind Teams incoming webhooks, which the `teams` kind posts to. It posts `{"type":
    "message", "attachments": [<an Adaptive Card 1.4>]}`, what the Workflows template "Post to a channel when a
    webhook request is received" posts, carrying what the `MessageCard` carries (`teamsFacts` is shared). The id is
    singular like the other kinds' (a channel is one workflow) and matches the variable. Unsigned, https only, no
    kind host list: the triggers live on regional Logic Apps hosts, per-environment Power Platform hosts and their
    sovereign-cloud counterparts, a list would lock tenants out, and a host every Azure customer shares would restrict
    nothing (operators pin hosts with `PAD_NOTIFY_ALLOWED_HOSTS`). Every text is a `TextRun`: Teams renders Markdown
    in a `TextBlock` and in a `FactSet`, honours backslash escapes unevenly across clients, and a flow name is a
    tenant's text, so `[click](https://evil.example)` would become a link; a `TextRun` is never Markdown. Not verified
    against a live Teams tenant.
13. **Organisation `jira` and `email` kinds**, on an expand migration (`org_channel_fields`) adding three columns to
    `org_channels`: `config` (JSONB object of strings, default `{}`: the public fields beyond `url`), `secrets` (TEXT,
    default `''`: the secret fields beyond `secret` as one JSON object, sealed with the field encryption key like
    `url` and `secret`, walked by the at-rest backfill and the key rotation's re-seal pass) and `events` (14). One
    pair of generic columns instead of per-field ones: a new organisation kind needs no migration, the store stays
    ignorant of kinds, and one sealed value per row keeps one AAD and one re-seal entry. `url` and `secret` keep their
    columns, so every existing row and the previous release read and write them as before. The kinds' fields are
    unchanged and the same specs serve both scopes, so the payload is the deployment's (the goldens are unchanged).
    - **Jira**: `url` (the base URL in the `url` column, checked like every channel URL: https, no internal target,
      `PAD_NOTIFY_ALLOWED_HOSTS`), `email` (the Basic-auth user: no colon, no control character), `token` (the API
      token, secret: write-only, kept when an edit omits it, never listed; `hasToken` says it is set; it is required,
      so `clear` refuses it, and an admin replaces it instead) and `project` (a Jira project key). The previous
      release's channels never had these fields, so `jira` stays one kind, now in both scopes.
    - **Email: recipients are member ids, not addresses and not a role.** `recipients` is a list of the
      organisation's user ids (at most 50). A save accepts only ids of members whose account address is verified
      (an outsider and an unverified member get the same 400 on `recipients`, so the answer tells nothing about
      accounts outside the organisation). Every delivery resolves the ids again to the verified addresses of those
      still members: a member who left, was erased or whose address is no longer verified is not mailed, with no
      write to the channel. Addresses are never typed, so an admin cannot make the server mail anyone outside the
      organisation (a relay for any text a tenant can put in a flow name), and an organisation cannot even mail its
      own members at an address they did not prove. Ids rather than "all admins"/"all members": an admin chooses who
      triages (an organisation's whole roster per event would load the mail runner with its size, and a role does not
      say who wants mail); the cost is user ids in organisation configuration, which erasure removes
      (`lifecycle_orgs.go`: `org_channels` is now erased by a step that takes the user out of every list), and a
      member who rejoins is mailed again if the admin had chosen them. Each recipient gets their own message (no
      member sees another's address) rendered with the `governance_alert` template and sent with `SendAlert`, which
      waits for a slot of the mail runner's notification share (pacing), within the delivery's timeout; one
      recipient's failure does not stop the others. Without an enabled mail sender the kind is not offered (400 on
      `kind` at create, left out of the options) and an existing email channel is skipped and its test is a 400.
14. **Per-channel event subscriptions.** `org_channels.events` (JSONB array, NULL) holds the event types a channel
    receives, chosen from `drift`, `health_regression` and `new_rules` (what organisation channels receive;
    `analysis_complete` goes to the deployment's channels only), at least one; the test event reaches every channel.
    NULL is the default set, frozen as the events every organisation channel received before (all three), so no
    existing or new channel changes until an admin chooses; an event type added later is opt-in. The API takes
    `events` (null keeps the choice) and lists the effective set. `ChannelService` wraps each built notifier
    (`notify.Subscribed`), whose `Notify` drops an event the channel does not receive, and the scanner checks
    `notify.Receives` before it takes a delivery slot.
15. **The allow-list, shown to admins only when the operator opts in.** `GET /api/orgs/{id}/channels/options`
    (organisation admins) answers the kinds the deployment delivers, the events and the default set, and
    `hostsRestricted` (whether `PAD_NOTIFY_ALLOWED_HOSTS` is set), so the form says a list applies before a save
    fails. The list itself (`allowedHosts`) is added only with `PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE=true`: it is the
    operator's list for every tenant and may name another customer's own host (a tenant's Teams or Jira host), which
    would tell every organisation admin who else uses the deployment; an operator whose list names only shared
    service hosts can show it. A "would this host pass" check was rejected: the save already answers that, one guessed
    host at a time, and a second oracle adds nothing but a route. The save's refusal still names the host, not the
    list.
16. **An unreadable channel no longer fails its organisation's list.** A row whose `url`, `secret` or `secrets` no
    configured key opens (the encryption key changed without the old one in `PAD_ENCRYPTION_KEY_PREVIOUS`, or a
    process without a key) failed `ListOrgChannels`, so the admin could neither see nor delete it, and the scanner
    delivered to none of the organisation's channels. The store now lists it with `Unreadable` set and every sealed
    value empty (never the ciphertext); the API lists it with `unreadable: true` and no URL; the scanner skips it with
    a WARN and an error report naming the channel id; its test is a 400 `CHANNEL_UNREADABLE` (`details.field: "url"`)
    without dialling; an edit must enter the URL (and any required secret) again, which reseals it under the current
    key, and an optional secret that could not be read is dropped; it can be deleted.

## Consequences

- A new kind is one file, an organisation kind with fields beyond `url` and `secret` too (`config` and `secrets` hold
  them).
- Organisation admins can file Jira issues and mail chosen members. The members' addresses are never shown to other
  members through the channel (the list carries user ids, which the roster names) and never typed.
- A channel can stop receiving an event type; the default stays everything organisation channels received before.
- An encryption key change no longer takes an organisation's channels down: the unreadable ones are listed and
  skipped, the others deliver.
- Receivers that parse the flat body keep working; receivers can now dedupe on `id` and refuse replays with the V2
  signature. The Slack and Teams cards of a `new_rules` event show the number of flows instead of an empty flow.
- Admins no longer see a channel's full URL. To change it they enter a new one.
- Tenant text (flow names) shows as typed in every chat format: Slack's `&`, `<` and `>` are escaped, so a flow name
  can no longer form a link or an `@channel` mention; the connector card's sections set `"markdown": false`; the
  Workflows card uses `TextRun`s. This matters most on the deployment's channels, which receive every tenant's
  events.
- On a local deployment with accounts, the scanner's deliveries to organisation channels refuse internal targets
  now, as saves did. The deployment channels keep loopback there: refusing a `PAD_NOTIFY_*` URL the previous
  release accepted would fail the boot after the migration ran.
- The channel list needs the server store (503 on the desktop, which has no organisations' channels).
- With `PAD_NOTIFY_ALLOWED_HOSTS` set, organisation channels on other hosts stop receiving events (logged per sweep)
  and cannot be tested until an admin points them at a listed host; a host shared by every customer of a service
  (`hooks.slack.com`, `prod-NN.<region>.logic.azure.com`) restricts the service, not the workspace.
- Teams users move to `teams-workflow` channels before Microsoft retires the connectors; the `teams` kind stays for
  as long as its webhooks deliver.
- Rolling back to the previous release: rows are unchanged (no migration). The previous server wipes a secret on
  edit again (B1), and sends org events with a zero `at`. A tab of this release talking to the previous server sends
  `urlUnchanged` and `clear` (ignored by the previous decoder) and re-sends the URL it was shown: the full URL when
  that server listed it, which keeps it; the `host/…` listing when the tab was opened before the rollback, which
  the previous validation refuses (no scheme), so the edit fails instead of storing the placeholder.
- Rolling back the allow-list and `teams-workflow` (no migration): the previous release ignores
  `PAD_NOTIFY_ALLOWED_HOSTS` (organisation channels may point at any public host again, and a channel saved off the
  list before is delivered to) and `PAD_NOTIFY_TEAMS_WORKFLOW_URL` (the deployment's Workflows channel stops), and
  lists a `teams-workflow` row as a kind it does not know: shown, removable, not delivered to (skipped and logged),
  its test a 400. A tab of the previous release lists such a channel the same way; a tab of this release offers
  `teams-workflow` to the previous server, which refuses it on `kind`.
- Rolling back the organisation `jira` and `email` kinds, the subscriptions, the options route and the unreadable
  listing (13-16). The migration is `expand`: the previous release never names `config`, `secrets` or `events`, so
  its INSERT takes their defaults (no config, no secrets, the default events) and its upsert's SET list leaves them
  alone, and it keeps reading and writing `url` and `secret` as before. It lists a `jira` or `email` organisation row
  through its deployment-only spec (the URL redacted, no fields), refuses to save one (on `kind`, so it cannot be
  paused or edited there, only removed), skips it at delivery (logged) and answers its test with a 400; it delivers
  every event to every channel again, ignoring a subscription, which takes effect again after a roll-forward; it
  ignores `PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE`; and an unreadable channel fails its organisation's list again. A tab of
  this release talking to the previous server finds no options route (405, `DELETE …/channels/{channelId}` matches
  the path) and offers the four URL kinds, as before; the fields it may send (`events`, `email`, ...) are ignored. A
  tab of the previous release talking to this server lists Jira and email channels as kinds it does not know
  (tested and removed, not edited) and sends no `events`, which keeps the choice. The down migration drops the three
  columns and deletes the organisation `jira` and `email` rows, which cannot work without them.

## Not done

An opt-in nested v2 body, retiring the body-only signature, and a delivery log or outbox. (The keystore and the field
encryption now share one key set, `sealKeys`, with the key rotation of ADR 0005.) Retiring the `teams` kind once Microsoft's connectors stop delivering (and moving
its rows); trusting the operator's proxy for allow-listed organisation hosts. Email recipients by role ("every
admin"), and removing a member from the lists when they leave the organisation (delivery skips them; erasure removes
them). `teams-workflow` is not verified against a live Teams tenant, nor the organisation Jira channel against a live
Jira Cloud.
