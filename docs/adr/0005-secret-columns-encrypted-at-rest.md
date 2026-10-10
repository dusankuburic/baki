# 5. Secret columns are encrypted at rest with the keystore key

Date: 2026-10-07. Status: accepted. Amended 2026-10-10: key rotation.

## Context

Provider API keys live in an encrypted keystore (AES-256-GCM). Org notification channels kept their webhook
URL and HMAC signing secret in plaintext `TEXT` columns. For Slack and Teams the URL is itself the credential,
so any database dump or backup handed out every org's channels.

## Decision

- The Postgres backend encrypts secret columns itself (`field_crypto.go`), so every reader (API handlers, the
  scanner's dispatch) gets plaintext without knowing about it.
- **Key:** the same one as the keystore (`PAD_ENCRYPTION_KEY`, or an operator-provided `PAD_AUTH_SECRET` as the
  legacy fallback). A per-process secret never qualifies.
- **Format:** `enc:v1:` + base64(nonce ‖ ciphertext), with table, column and row id as associated data, so a
  ciphertext copied to another row or column does not decrypt.
- **Startup:** encryption is enabled before the server starts.
- **Backfill:** a one-shot job running once per boot under a cross-replica advisory lock seals rows written in
  plaintext. It is idempotent, and its compare-and-set never overwrites a concurrent save.
- **Legacy rows:** plaintext rows stay readable until the backfill reaches them.

### Key rotation (2026-10-10)

Sealed values today: `org_channels.url`, `.secret` and `.secrets`, `org_ci_credentials.secret`,
`idempotency_keys.body` (field encryption), and the keystore's `provider_keys.ciphertext` and
`org_provider_keys.ciphertext` (provider keys and
organisation credentials, the Azure registry's credential documents among them). Desktop mode stores provider keys
in the OS keychain, which has no key of ours, and has no server store.

- **Configuration.** The current key stays `PAD_ENCRYPTION_KEY`. `PAD_ENCRYPTION_KEY_PREVIOUS` lists the keys it
  replaced, comma-separated, newest first. Each entry is held to the current key's cloud rule (at least 32
  characters, not a known placeholder, not low-entropy). The list needs a dedicated `PAD_ENCRYPTION_KEY` (moving off
  the `PAD_AUTH_SECRET` fallback lists that secret as the previous key) and may not repeat the current key or itself.
  Errors name an entry by position. Neither the keys nor the list are logged; the keystore's boot line logs the
  count.
- **Reads** try the current key, then each previous key in order. **Writes** always seal with the current key.
- **No format change.** GCM authentication tells which key fits: a wrong key fails the tag check. `enc:v1:` and the
  keystore's bare base64 stay the only formats, so the release before reads every value its key sealed. A key
  identifier (`enc:v2:<kid>:…`) was rejected. The release that writes it could not be rolled back even without a
  rotation, because the previous release would take `enc:v2:` for plaintext: a channel URL of `enc:v2:…`, and its
  backfill sealing that string. It would also save little. A failed open costs what a successful one does, one pass
  over the value: about 0.3 µs for a channel URL, about 0.15 ms for the largest stored response (256 KiB). It is paid
  only for values still under a previous key, once per previous key ahead of the right one. A replay, a channel list
  or a keystore read during a rotation pays at most that, and nothing after the re-seal pass. An identifier would let
  the pass select only the values under a previous key. Without one it reads every sealed value. It does that only
  while a rotation is configured, once per boot, off the boot path, over tables of a few rows per user or
  organisation (idempotency records live 24 hours).
- **Re-seal pass.** With previous keys configured, every boot runs it in the background (`lifecycle.Bounded`
  `secret_reseal`, invoked after the server so its stop hook cancels it before the shutdown drain, 30 min ceiling).
  It runs on one replica at a time, under the advisory lock `bakikrot`; a replica finding it held skips. It walks
  each sealed column keyset-paged by primary key (500 rows a page, 64 for the idempotency bodies, which are at most
  256 KiB each; expired idempotency records are skipped). It moves each value a previous key opens to the current
  key with a compare-and-set on the stored value, so a concurrent save, delete or second pass is never overwritten.
  A lost compare-and-set re-reads the value and judges it again. The keystore keeps `updated_at`: the value is the
  same, and that stamp is the compare-and-swap token of org credential edits and the orphan sweep's clock. Every
  rewrite is per row, so the pass is idempotent and resumable: a stopped pass leaves every value readable, and the
  next boot carries on.
- **Unreadable values** (no configured key opens them) are counted, named by table, column and row id (never the
  value; at most 20 named), and left as stored. They never fail the pass or the boot.
- **Report.** The pass logs `checked`, `resealed`, `remaining` (written under a previous key while the pass ran, by
  a replica still on the old configuration) and `unreadable`. With nothing remaining and nothing unreadable it logs
  `secret re-seal complete: every sealed value opens with PAD_ENCRYPTION_KEY`. The job's
  `pad_job_duration_seconds{job="secret_reseal"}` times it (ADR 0023). No new metric family: the line is a
  one-off per rotation.
- **Restores.** An organisation credential snapshot put back after a failed registry write
  (`RestoreOrgIfUnchanged`) is sealed again with the current key when a previous key had sealed it. The snapshot
  may predate the pass moving the row, and the restore must not undo the pass.
- **Coverage.** `TestReseal_CoversEverySealedColumn` fails when a `sealField` call or a keystore ciphertext write
  names a column the pass does not walk.

### Secrets in configuration (2026-10-10)

The keys above, and every other credential the configuration holds, are `secret.Value`
(`internal/platform/secret`) in `config.Config`, not strings: `Auth.Secret`, `EncryptionKey`,
`EncryptionKeyPrevious` (a slice), `SSO.ClientSecret`, `Server.MetricsToken`, `Storage.DatabaseURL` and
`AzureBlobConnectionString` (a DSN and a connection string carry a password and an account key), `Redis.URL`,
`Email.Password`, the governance signing secrets, the Jira API token, `InboundWebhookSecret`, and the Teams, Slack
and generic webhook URLs (the URL is the credential). A `secret.Value` prints, formats, marshals and logs as
`[redacted]` in every form (`%v`, `%+v`, `%#v`, `%q`, `json.Marshal`, slog, an error wrapping it); an unset one
prints as empty, so a dump still tells set from unset. `Reveal()` is the one way to the plain text, called at the
boundary that needs it (the Postgres DSN, `redis.ParseURL`, the keystore and field-encryption keys, the SMTP
client, the OIDC exchange, the notify channel values, the session-key file, the CI webhook's HMAC), never to log.
The downstream types that hold a secret for the life of the process (`api.SecurityConfig.Token`,
`service.CIOptions.GlobalSecret`, the metrics guard) hold the `secret.Value` and compare with its constant-time
`Equal`. `internal/config`'s `TestConfig_FormatAndJSONNeverShowSecrets` proves the configuration shows none of
them, and `TestConfig_SecretFieldsAreSecretValues` keeps its list of secret fields complete.

Sources are unchanged: a `PAD_*` variable holds the plain value, and a config file holds a JSON string under the
same field name. Both refuse the literal `[redacted]`, so a redacted log line pasted back as configuration fails
at boot instead of becoming the secret. `Auth.SecretEphemeral` is no longer a file setting (`json:"-"`): a file
that set it beside a real secret made the keystore refuse that secret.

### Operator procedure

1. Rolling deploys only, where old and new replicas overlap: stage the new key first. Keep `PAD_ENCRYPTION_KEY`,
   set `PAD_ENCRYPTION_KEY_PREVIOUS=<new>`, and deploy. Every replica then opens what the new key will seal, so the
   next step has no window where an old-configuration replica meets a value it cannot open.
2. Set `PAD_ENCRYPTION_KEY=<new>` and `PAD_ENCRYPTION_KEY_PREVIOUS=<old>`, and deploy.
3. Wait for the `secret re-seal complete` line from a pass that started after the rollout finished. `remaining` above
   0 means a replica on the old configuration wrote during the rollout: restart one replica. `unreadable` above 0
   names values sealed with a key not listed: add that key, or have the owners re-enter those secrets.
4. Remove `PAD_ENCRYPTION_KEY_PREVIOUS` and deploy.

## Consequences

The data is useless without the key. A process without the key refuses to read sealed values
(`ErrFieldKeyMissing`) rather than returning ciphertext as a URL; it lists an organisation's sealed channels as
unreadable. The key can be rotated without losing a stored
secret, and every boot of a rotating deployment pays one read of every sealed value. Future secret columns use
`sealField`/`openField`, add themselves to the backfill, and add themselves to `fieldSealedColumns` (the coverage
test enforces the last).

**Rollback.** The release before this change knows one key and ignores `PAD_ENCRYPTION_KEY_PREVIOUS`. The stored
format is unchanged, so deploying this release without rotating rolls back cleanly. After step 2 every write, and
every value the pass has moved, is sealed with the new key:

- Rolling the image back while keeping `PAD_ENCRYPTION_KEY=<new>` is safe once step 3's line has been logged. Before
  that, the values still under the old key are unreadable to it until the image returns.
- Rolling back to `PAD_ENCRYPTION_KEY=<old>` alone after step 2 makes the new key's values unreadable. Provider keys
  and organisation credentials answer `ErrSecretDecryptFailed`, so users re-enter them. An org's channels the new key
  sealed are listed unreadable and skipped, the others keep working (ADR 0021, 16). CI credentials fail the webhook.
  Idempotent replays fail.
- Safe order to undo a rotation: with this release, rotate back (`PAD_ENCRYPTION_KEY=<old>`,
  `PAD_ENCRYPTION_KEY_PREVIOUS=<new>`), wait for step 3's line, then deploy the release before.

Not done:

- A restart-free way to re-run the pass: a value written under a previous key during the rollout waits for the next
  boot.

Done since: an unreadable channel no longer fails its organisation's channel list. `ListOrgChannels` lists it with
`Unreadable` set and no sealed value, the scanner skips it (logged and reported by channel id), its test answers
`400 CHANNEL_UNREADABLE`, and its admin removes it or enters its URL again, which seals it under the current key
(ADR 0021, 16). The re-seal report still names it.
