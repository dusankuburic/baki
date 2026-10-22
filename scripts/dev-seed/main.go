// Command dev-seed prints the SQL that creates (or resets) a local development account.
//
// LOCAL DEVELOPMENT ONLY. It exists because the compose stack has no mail provider: a fresh
// sign-up stays unverified, and "forgot password" has nowhere to send a link, so a dev database
// can end up with no account anybody can log in as. The account it seeds is an admin with a
// verified address and a known password, both overridable:
//
//	go run ./scripts/dev-seed | docker compose exec -T db psql -U postgres -d baki
//	DEV_SEED_EMAIL=me@example.com DEV_SEED_PASSWORD=... go run ./scripts/dev-seed | ...
//
// Never point this at anything but a local database; the credentials below are public knowledge
// by definition, since they live in the repository.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The defaults every developer can rely on after a database reset.
const (
	defaultEmail    = "dev@baki.local"
	defaultPassword = "baki-dev-2026"
	defaultRole     = "admin"
	// Matches the cost the application hashes with (internal/auth); a different cost still
	// verifies, this only keeps the stored hashes uniform.
	bcryptCost = 12
)

func main() {
	email := envOr("DEV_SEED_EMAIL", defaultEmail)
	password := envOr("DEV_SEED_PASSWORD", defaultPassword)
	role := envOr("DEV_SEED_ROLE", defaultRole)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dev-seed: hashing failed:", err)
		os.Exit(1)
	}

	id := fmt.Sprintf("usr_devseed_%d", time.Now().UnixNano())

	// An existing row keeps its id (foreign keys point at it) and gets the password, role and a
	// verified address; a missing one is created. Either way the account can log in afterwards.
	fmt.Printf(`BEGIN;

INSERT INTO users (id, email, password, role, email_verified, display_name, created_at, updated_at)
VALUES ('%s', '%s', '%s', '%s', true, 'Dev', now(), now())
ON CONFLICT (email) DO UPDATE
SET password = EXCLUDED.password,
    role = EXCLUDED.role,
    email_verified = true,
    failed_login_attempts = 0,
    locked_until = NULL,
    updated_at = now();

COMMIT;

\echo 'dev-seed: %s can now sign in'
`, id, sqlEscape(email), sqlEscape(string(hash)), sqlEscape(role), sqlEscape(email))
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
