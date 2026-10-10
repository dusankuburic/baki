package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// Per-org notification channels (R2-3). RLS mirrors the knowledge tables
// (org-member visibility; see migration v17) — the HTTP layer additionally
// requires org admin for mutations.

// SaveOrgChannel upserts one channel, keyed on its id.
//
// The conflict branch is scoped to the row's OWNING org. Without that predicate
// the id alone decided the update, and the handler takes the id from the
// request body while authorizing against the org in the URL — so an admin of
// org A could POST org B's channel id to /api/orgs/A/channels and replace B's
// webhook url + secret with their own. org_id is not in the SET list, so the
// row kept B's ownership while quietly delivering B's governance alerts to the
// attacker. RLS rejects that too, but RLS is off whenever the app connects as a
// superuser/BYPASSRLS role, so this clause has to hold on its own.
//
// A conflicting id under a different org returns ErrNotFound rather than
// succeeding silently: the WHERE alone would make both the attack AND a
// legitimate failed save look like success. ErrNotFound (not a permission
// error) so the response doesn't confirm that the id exists.
func (b *PostgresStorageBackend) SaveOrgChannel(ctx context.Context, ch *storageif.OrgChannel) error {
	if ch == nil || ch.ID == "" || ch.OrgID == "" {
		return errors.New("org channel requires id and orgId")
	}
	// The URL (for Slack and Teams, the credential), the signing secret and the other secret fields are encrypted at
	// rest (field_crypto.go).
	url, err := b.sealField("org_channels", "url", ch.ID, ch.URL)
	if err != nil {
		return err
	}
	secret, err := b.sealField("org_channels", "secret", ch.ID, ch.Secret)
	if err != nil {
		return err
	}
	config, secrets, events, err := b.encodeChannelFields(ch)
	if err != nil {
		return err
	}
	var savedID string
	err = b.query(ctx).QueryRowContext(ctx, `
		INSERT INTO org_channels (id, org_id, name, kind, url, secret, enabled, created_at, config, secrets, events)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11::jsonb)
		ON CONFLICT (id) DO UPDATE SET
			name    = EXCLUDED.name,
			kind    = EXCLUDED.kind,
			url     = EXCLUDED.url,
			secret  = EXCLUDED.secret,
			enabled = EXCLUDED.enabled,
			config  = EXCLUDED.config,
			secrets = EXCLUDED.secrets,
			events  = EXCLUDED.events
		WHERE org_channels.org_id = EXCLUDED.org_id
		RETURNING id`,
		ch.ID, ch.OrgID, ch.Name, ch.Kind, url, secret, ch.Enabled, ch.CreatedAt, config, secrets, events,
	).Scan(&savedID)
	if errors.Is(err, sql.ErrNoRows) {
		return storageif.ErrNotFound
	}
	return err
}

func (b *PostgresStorageBackend) DeleteOrgChannel(ctx context.Context, orgID, id string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM org_channels WHERE id = $1 AND org_id = $2`, id, orgID)
	if err == nil {
		return nil
	}
	return err
}

func (b *PostgresStorageBackend) ListOrgChannels(ctx context.Context, orgID string, enabledOnly bool) ([]*storageif.OrgChannel, error) {
	q := `SELECT id, org_id, name, kind, url, secret, enabled, created_at, config::text, secrets, events::text
	      FROM org_channels WHERE org_id = $1`
	if enabledOnly {
		q += ` AND enabled`
	}
	q += ` ORDER BY created_at`
	rows, err := b.query(ctx).QueryContext(ctx, q, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*storageif.OrgChannel{}
	for rows.Next() {
		var ch storageif.OrgChannel
		var config, secrets string
		var events sql.NullString
		if err := rows.Scan(&ch.ID, &ch.OrgID, &ch.Name, &ch.Kind, &ch.URL, &ch.Secret, &ch.Enabled, &ch.CreatedAt,
			&config, &secrets, &events); err != nil {
			return nil, err
		}
		ch.Config, ch.Events = decodeChannelConfig(config), decodeChannelEvents(events)
		b.openChannel(&ch, secrets)
		out = append(out, &ch)
	}
	return out, rows.Err()
}

// openChannel opens ch's sealed values: URL, Secret and the secrets column. When one opens with no configured key
// (the encryption key changed without the old one in PAD_ENCRYPTION_KEY_PREVIOUS, or a process without a key), ch
// is Unreadable with every sealed value empty. Such a row used to fail its organisation's whole list, so its admin
// could neither see nor delete it, and none of the organisation's channels was delivered to. The service skips it
// for delivery and asks for its URL again; the re-seal report names it.
func (b *PostgresStorageBackend) openChannel(ch *storageif.OrgChannel, secrets string) {
	url, errURL := b.openField("org_channels", "url", ch.ID, ch.URL)
	secret, errSecret := b.openField("org_channels", "secret", ch.ID, ch.Secret)
	opened, errSecrets := b.openField("org_channels", "secrets", ch.ID, secrets)
	var values map[string]string
	if errSecrets == nil && opened != "" {
		errSecrets = json.Unmarshal([]byte(opened), &values)
	}
	if errURL != nil || errSecret != nil || errSecrets != nil {
		ch.URL, ch.Secret, ch.Secrets, ch.Unreadable = "", "", nil, true
		return
	}
	ch.URL, ch.Secret, ch.Secrets = url, secret, values
}

// encodeChannelFields is how ch's fields beyond url and secret are stored: config a JSON object of strings (never
// NULL), secrets one sealed JSON object ("" when there is none), events a JSON array, or NULL for the default set.
func (b *PostgresStorageBackend) encodeChannelFields(ch *storageif.OrgChannel) (config, secrets string, events any, err error) {
	raw, err := json.Marshal(nonEmptyValues(ch.Config))
	if err != nil {
		return "", "", nil, err
	}
	config = string(raw)
	if s := nonEmptyValues(ch.Secrets); len(s) > 0 {
		plain, err := json.Marshal(s)
		if err != nil {
			return "", "", nil, err
		}
		if secrets, err = b.sealField("org_channels", "secrets", ch.ID, string(plain)); err != nil {
			return "", "", nil, err
		}
	}
	if ch.Events != nil {
		raw, err := json.Marshal(ch.Events)
		if err != nil {
			return "", "", nil, err
		}
		events = string(raw)
	}
	return config, secrets, events, nil
}

// decodeChannelConfig reads the config column; a value that is not an object of strings (written by hand) reads as
// no config, which fails the kind's validation instead of the list.
func decodeChannelConfig(raw string) map[string]string {
	var config map[string]string
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return nil
	}
	return config
}

// decodeChannelEvents reads the events column: NULL (or a value that is not an array of strings) is the default set.
func decodeChannelEvents(raw sql.NullString) []string {
	var events []string
	if !raw.Valid || json.Unmarshal([]byte(raw.String), &events) != nil {
		return nil
	}
	return events
}

// nonEmptyValues is m without its empty values, never nil (stored as an empty object, not null).
func nonEmptyValues(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out
}
