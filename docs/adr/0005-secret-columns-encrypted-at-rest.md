# 5. Secret columns are encrypted at rest with the keystore key

Date: 2026-10-07. Status: accepted.

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

## Consequences

The data is useless without the key. A process without the key refuses to read sealed values
(`ErrFieldKeyMissing`) rather than returning ciphertext as a URL. Rotating `PAD_ENCRYPTION_KEY` now needs a
re-encryption step for these columns as well as for the keystore. Neither exists yet; both should be added
together. Future secret columns use `sealField`/`openField` and add themselves to the backfill.
