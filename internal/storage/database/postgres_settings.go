package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// SaveSettings upserts the single-row app settings record.
func (b *PostgresStorageBackend) SaveSettings(ctx context.Context, settings *interfaces.AppSettings) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	_, err = b.db.ExecContext(ctx, `
		INSERT INTO app_settings (id, data, updated_at) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`,
		data, time.Now().UTC())
	return err
}

// LoadSettings retrieves the app settings.
func (b *PostgresStorageBackend) LoadSettings(ctx context.Context) (*interfaces.AppSettings, error) {
	var data []byte
	err := b.db.QueryRowContext(ctx, `SELECT data FROM app_settings WHERE id = 1`).Scan(&data)
	if err == sql.ErrNoRows {
		return &interfaces.AppSettings{Version: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	var s interfaces.AppSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal settings: %w", err)
	}
	return &s, nil
}

// SaveUserSettings upserts settings for a specific user.
func (b *PostgresStorageBackend) SaveUserSettings(ctx context.Context, userID string, settings *interfaces.AppSettings) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal user settings: %w", err)
	}
	_, err = b.db.ExecContext(ctx, `
		INSERT INTO user_settings (user_id, data, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`,
		userID, data, time.Now().UTC())
	return err
}

// LoadUserSettings retrieves settings for a specific user.
func (b *PostgresStorageBackend) LoadUserSettings(ctx context.Context, userID string) (*interfaces.AppSettings, error) {
	var data []byte
	err := b.db.QueryRowContext(ctx, `SELECT data FROM user_settings WHERE user_id = $1`, userID).Scan(&data)
	if err == sql.ErrNoRows {
		// Return default settings if none found
		return &interfaces.AppSettings{Version: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load user settings: %w", err)
	}
	var s interfaces.AppSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal user settings: %w", err)
	}
	return &s, nil
}

// SaveOrgSettings upserts settings for a specific organisation.
func (b *PostgresStorageBackend) SaveOrgSettings(ctx context.Context, orgID string, settings *interfaces.AppSettings) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal org settings: %w", err)
	}
	_, err = b.db.ExecContext(ctx, `
		INSERT INTO org_settings (org_id, data, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (org_id) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`,
		orgID, data, time.Now().UTC())
	return err
}

// LoadOrgSettings retrieves settings for a specific organisation.
func (b *PostgresStorageBackend) LoadOrgSettings(ctx context.Context, orgID string) (*interfaces.AppSettings, error) {
	var data []byte
	err := b.db.QueryRowContext(ctx, `SELECT data FROM org_settings WHERE org_id = $1`, orgID).Scan(&data)
	if err == sql.ErrNoRows {
		// Return default settings if none found
		return &interfaces.AppSettings{Version: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load org settings: %w", err)
	}
	var s interfaces.AppSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal org settings: %w", err)
	}
	return &s, nil
}
