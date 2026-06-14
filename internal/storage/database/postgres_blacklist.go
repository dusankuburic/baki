package database

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"pad-analyzer/internal/auth"
)

// PostgresBlacklist implements auth.BlacklistStore using a Postgres table.
// This ensures token revocation is immediately consistent across all API
// replicas in a horizontally-scaled deployment (the in-memory TokenBlacklist
// is per-process and cannot share state between replicas).
type PostgresBlacklist struct {
	db       *sql.DB
	stop     chan struct{}
	stopOnce sync.Once
}

func NewPostgresBlacklist(db *sql.DB) *PostgresBlacklist {
	bl := &PostgresBlacklist{db: db, stop: make(chan struct{})}
	go bl.cleanup()
	return bl
}

func (bl *PostgresBlacklist) Add(jti string, ttl time.Duration) {
	expiresAt := time.Now().Add(ttl).UTC()
	if _, err := bl.db.ExecContext(context.Background(),
		`INSERT INTO token_blacklist (jti, expires_at) VALUES ($1, $2)
		 ON CONFLICT (jti) DO UPDATE SET expires_at = GREATEST(token_blacklist.expires_at, EXCLUDED.expires_at)`,
		jti, expiresAt); err != nil {
		slog.Warn("failed to add token to blacklist", "jti", jti, "err", err)
	}
}

func (bl *PostgresBlacklist) IsRevoked(jti string) bool {
	var tmp int
	err := bl.db.QueryRowContext(context.Background(),
		`SELECT 1 FROM token_blacklist WHERE jti = $1 AND expires_at > NOW()`, jti).Scan(&tmp)
	if err != nil && err != sql.ErrNoRows {
		slog.Warn("blacklist check failed — treating as not revoked", "jti", jti, "err", err)
	}
	return err == nil
}

func (bl *PostgresBlacklist) AddIfAbsent(jti string, ttl time.Duration) bool {
	expiresAt := time.Now().Add(ttl).UTC()
	cmd, err := bl.db.ExecContext(context.Background(),
		`INSERT INTO token_blacklist (jti, expires_at) VALUES ($1, $2)
		 ON CONFLICT (jti) DO UPDATE SET expires_at = EXCLUDED.expires_at
		 WHERE token_blacklist.expires_at < NOW()`,
		jti, expiresAt)
	if err != nil {
		return false
	}
	rows, _ := cmd.RowsAffected()
	return rows > 0
}

func (bl *PostgresBlacklist) Stop() {
	bl.stopOnce.Do(func() { close(bl.stop) })
}

func (bl *PostgresBlacklist) cleanup() {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("blacklist cleanup goroutine panicked", "err", r)
		}
	}()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-bl.stop:
			return
		case <-ticker.C:
			_, _ = bl.db.ExecContext(context.Background(),
				`DELETE FROM token_blacklist WHERE expires_at < NOW()`)
		}
	}
}

var _ auth.BlacklistStore = (*PostgresBlacklist)(nil)
