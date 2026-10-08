package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"pad-core/models"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/storage/settingsdoc"
)

// Settings tables (app_settings, user_settings, org_settings, settings_sections) carry no RLS
// policies — scoping is by primary key, and the handler supplies the caller's
// own id. They still go through b.query(ctx) or withTx rather than b.db so that a write
// joins the request's RLS transaction when one is open: a write on b.db
// committed on a separate pooled connection and survived the rollback
// rlsMiddleware applied to a request that failed afterwards. Nothing about
// visibility changes here; only atomicity.
//
// A settings document is stored per section in settings_sections, each section with its own version
// (settingsdoc, ADR 0012), and, for one release, also whole in the legacy blob row the release before reads.
// Both are written in one transaction. A section row's legacy_digest is the md5 of the section as written into
// the blob; when the blob's section no longer matches it, a release that writes only the blob (a rollback) changed
// it since, and the blob wins for that section.

// settingsScope is one kind of settings document: its settings_sections scope and its legacy blob table.
type settingsScope struct {
	name string
	// defaults is what a document that was never saved reads as.
	defaults func() (*interfaces.AppSettings, error)
	// legacyKey is the blob row's key for a scope id.
	legacyKey func(id string) any

	loadSQL       string
	saveLegacySQL string
	// lockParentSQL locks the scope's owner row (the user or the organisation) the way a delete of it does, "" for
	// the deployment.
	lockParentSQL string
}

func newSettingsScope(name, table, keyColumn, parent string, defaults func() (*interfaces.AppSettings, error), legacyKey func(string) any) settingsScope {
	lockParent := ""
	if parent != "" {
		lockParent = fmt.Sprintf(`SELECT 1 FROM %s WHERE id = $1 FOR KEY SHARE`, parent)
	}
	return settingsScope{
		name:          name,
		defaults:      defaults,
		legacyKey:     legacyKey,
		lockParentSQL: lockParent,
		// One statement, so the blob and the section rows come from one snapshot. The blob is the row whose
		// section is NULL. A section row the blob no longer matches is left out: the blob's section is newer. The
		// fields only that row holds are not lost, the next write merges onto it.
		loadSQL: fmt.Sprintf(`
			WITH legacy AS (SELECT data FROM %[1]s WHERE %[2]s = $1)
			SELECT NULL::text, 0, data FROM legacy
			UNION ALL
			SELECT s.section, s.version, s.data
			  FROM settings_sections s
			 WHERE s.scope = $2 AND s.scope_id = $3
			   AND (s.legacy_digest IS NULL
			        OR NOT EXISTS (SELECT 1 FROM legacy)
			        OR s.legacy_digest = md5(COALESCE((SELECT data FROM legacy) -> s.section, 'null'::jsonb)::text))`, table, keyColumn),
		saveLegacySQL: fmt.Sprintf(`
			INSERT INTO %[1]s (%[2]s, data, updated_at) VALUES ($1, $2, $3)
			ON CONFLICT (%[2]s) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`, table, keyColumn),
	}
}

var (
	appSettingsScope = newSettingsScope("app", "app_settings", "id", "", versionOneSettings,
		func(string) any { return 1 })
	// A user who has never saved settings gets the application defaults. This returned a bare {Version: 1},
	// whose zero values the client merged over its own defaults, so every default of true ("Save conversation
	// history", "Auto-analyze on open") showed as off for a new cloud account, and chat, which now honours the
	// history toggle, would have stopped saving their conversations.
	userSettingsScope = newSettingsScope("user", "user_settings", "user_id", "users", defaultStorageSettings,
		func(id string) any { return id })
	orgSettingsScope = newSettingsScope("org", "org_settings", "org_id", "organisations", versionOneSettings,
		func(id string) any { return id })
)

const lockSettingsSectionsSQL = `
	SELECT section, version, data FROM settings_sections
	 WHERE scope = $1 AND scope_id = $2
	   FOR UPDATE`

const upsertSettingsSectionsSQL = `
	INSERT INTO settings_sections (scope, scope_id, section, version, data, legacy_digest, updated_at)
	SELECT $1, $2, t.section, t.version, t.data::jsonb, md5(COALESCE($6::jsonb -> t.section, 'null'::jsonb)::text), $7
	  FROM unnest($3::text[], $4::int[], $5::text[]) AS t(section, version, data)
	ON CONFLICT (scope, scope_id, section) DO UPDATE
	   SET version = EXCLUDED.version, data = EXCLUDED.data,
	       legacy_digest = EXCLUDED.legacy_digest, updated_at = EXCLUDED.updated_at`

// SaveSettings upserts the deployment's settings.
func (b *PostgresStorageBackend) SaveSettings(ctx context.Context, settings *interfaces.AppSettings) error {
	return b.saveSettingsDoc(ctx, appSettingsScope, "", settings)
}

// LoadSettings retrieves the deployment's settings.
func (b *PostgresStorageBackend) LoadSettings(ctx context.Context) (*interfaces.AppSettings, error) {
	return b.loadSettingsDoc(ctx, appSettingsScope, "")
}

// SaveUserSettings upserts settings for a specific user.
func (b *PostgresStorageBackend) SaveUserSettings(ctx context.Context, userID string, settings *interfaces.AppSettings) error {
	return b.saveSettingsDoc(ctx, userSettingsScope, userID, settings)
}

// LoadUserSettings retrieves settings for a specific user.
func (b *PostgresStorageBackend) LoadUserSettings(ctx context.Context, userID string) (*interfaces.AppSettings, error) {
	return b.loadSettingsDoc(ctx, userSettingsScope, userID)
}

// SaveOrgSettings upserts settings for a specific organisation.
func (b *PostgresStorageBackend) SaveOrgSettings(ctx context.Context, orgID string, settings *interfaces.AppSettings) error {
	return b.saveSettingsDoc(ctx, orgSettingsScope, orgID, settings)
}

// LoadOrgSettings retrieves settings for a specific organisation.
func (b *PostgresStorageBackend) LoadOrgSettings(ctx context.Context, orgID string) (*interfaces.AppSettings, error) {
	return b.loadSettingsDoc(ctx, orgSettingsScope, orgID)
}

func (b *PostgresStorageBackend) sections() *settingsdoc.Registry {
	if b.settingsSections != nil {
		return b.settingsSections
	}
	return settingsdoc.Default
}

// saveSettingsDoc writes the document's sections, each merged onto its stored row (settingsdoc.Encode), and the
// legacy blob exactly as the release before wrote it.
func (b *PostgresStorageBackend) saveSettingsDoc(ctx context.Context, sc settingsScope, id string, settings *interfaces.AppSettings) error {
	blob, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal %s settings: %w", sc.name, err)
	}
	return b.withTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		// The owner row, the blob row, then the section rows: the order in which deleting the user or organisation
		// locks them (its own row, then the cascade). A save that inserted a section row while holding the blob
		// row waited on the owner row for the foreign key check, which the delete held while it waited on the
		// blob: a deadlock, as was locking the sections first.
		if sc.lockParentSQL != "" {
			if _, err := tx.ExecContext(txCtx, sc.lockParentSQL, id); err != nil {
				return fmt.Errorf("lock %s settings owner: %w", sc.name, err)
			}
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(txCtx, sc.saveLegacySQL, sc.legacyKey(id), blob, now); err != nil {
			return fmt.Errorf("save %s settings: %w", sc.name, err)
		}
		stored, err := lockSettingsSections(txCtx, tx, sc, id)
		if err != nil {
			return err
		}
		sections, err := b.sections().Encode(settings, stored)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(sections))
		for k := range sections {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		versions := make([]int, len(keys))
		datas := make([]string, len(keys))
		for i, k := range keys {
			versions[i] = sections[k].Version
			datas[i] = string(sections[k].Data)
		}
		if _, err := tx.ExecContext(txCtx, upsertSettingsSectionsSQL, sc.name, id, keys, versions, datas, string(blob), now); err != nil {
			return fmt.Errorf("save %s settings sections: %w", sc.name, err)
		}
		return nil
	})
}

// lockSettingsSections reads the scope's section rows and locks them for the rest of the transaction, so two
// writers merge one after the other.
func lockSettingsSections(ctx context.Context, tx *sql.Tx, sc settingsScope, id string) (map[string]settingsdoc.Stored, error) {
	rows, err := tx.QueryContext(ctx, lockSettingsSectionsSQL, sc.name, id)
	if err != nil {
		return nil, fmt.Errorf("lock %s settings sections: %w", sc.name, err)
	}
	defer rows.Close()
	stored := map[string]settingsdoc.Stored{}
	for rows.Next() {
		var key string
		var version int
		var data []byte
		if err := rows.Scan(&key, &version, &data); err != nil {
			return nil, fmt.Errorf("scan %s settings section: %w", sc.name, err)
		}
		stored[key] = settingsdoc.Stored{Version: version, Data: data}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s settings sections: %w", sc.name, err)
	}
	return stored, nil
}

// loadSettingsDoc reads the document per section: a section row the blob still matches, else the blob's section
// (at settingsdoc.LegacyVersion), else the scope's defaults. Older sections are upgraded on the way.
func (b *PostgresStorageBackend) loadSettingsDoc(ctx context.Context, sc settingsScope, id string) (*interfaces.AppSettings, error) {
	rows, err := b.query(ctx).QueryContext(ctx, sc.loadSQL, sc.legacyKey(id), sc.name, id)
	if err != nil {
		return nil, fmt.Errorf("load %s settings: %w", sc.name, err)
	}
	defer rows.Close()
	var legacy []byte
	current := map[string]settingsdoc.Stored{}
	for rows.Next() {
		var key sql.NullString
		var version int
		var data []byte
		if err := rows.Scan(&key, &version, &data); err != nil {
			return nil, fmt.Errorf("scan %s settings: %w", sc.name, err)
		}
		if key.Valid {
			current[key.String] = settingsdoc.Stored{Version: version, Data: data}
		} else {
			legacy = data
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load %s settings: %w", sc.name, err)
	}

	doc := &interfaces.AppSettings{}
	sections := map[string]settingsdoc.Stored{}
	switch {
	case legacy != nil:
		var top map[string]json.RawMessage
		if err := json.Unmarshal(legacy, &top); err != nil {
			return nil, fmt.Errorf("unmarshal %s settings: %w", sc.name, err)
		}
		if v, ok := top[settingsdoc.DocVersionKey]; ok {
			if err := json.Unmarshal(v, &doc.Version); err != nil {
				return nil, fmt.Errorf("unmarshal %s settings: %w", sc.name, err)
			}
		}
		for k, data := range top {
			if k != settingsdoc.DocVersionKey {
				sections[k] = settingsdoc.Stored{Version: settingsdoc.LegacyVersion, Data: data}
			}
		}
	case len(current) == 0:
		return sc.defaults()
	default:
		// Section rows without a blob: what the release that stops writing the blob leaves for a new document.
		if doc, err = sc.defaults(); err != nil {
			return nil, err
		}
	}
	for k, s := range current {
		sections[k] = s
	}
	if _, err := b.sections().Decode(sections, doc); err != nil {
		return nil, fmt.Errorf("load %s settings: %w", sc.name, err)
	}
	return doc, nil
}

func versionOneSettings() (*interfaces.AppSettings, error) {
	return &interfaces.AppSettings{Version: 1}, nil
}

// defaultStorageSettings converts the application defaults (pad-core models,
// the single source of truth) into the storage settings type. The two types
// share JSON tags field for field, so the round trip is lossless.
func defaultStorageSettings() (*interfaces.AppSettings, error) {
	data, err := json.Marshal(models.DefaultSettings())
	if err != nil {
		return nil, fmt.Errorf("marshal default settings: %w", err)
	}
	var s interfaces.AppSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal default settings: %w", err)
	}
	return &s, nil
}
