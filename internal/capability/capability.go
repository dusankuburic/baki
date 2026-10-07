// Package capability names what a running backend can offer, derived once from the validated config. Features
// declare the capabilities they need instead of probing for a nil storage backend or comparing the deployment
// mode by hand (about 90 nil-backend checks and 60 "cloud mode required" answers when this package was added).
// Phase 1a's route policy gates routes on it; docs/architecture/modes.md shows the matrix.
package capability

import "pad-analyzer/internal/config"

// Profile is the deployment profile of the process.
type Profile string

const (
	// Desktop is the Tauri sidecar: PAD_MODE=local, no server-side store, a single local user.
	Desktop Profile = "desktop"
	// LocalDB is local mode on Postgres (PAD_STORAGE=database): a development setup with the cloud store but
	// without multi-tenant enforcement.
	LocalDB Profile = "local+db"
	// Cloud is the multi-tenant server: authentication enforced, Postgres, row-level security.
	Cloud Profile = "cloud"
)

// Capability is one thing a feature can require.
type Capability string

const (
	// ServerStore: a persistent server-side store (Postgres) backs library, orgs, sharing, analytics, audit.
	ServerStore Capability = "server-store"
	// Accounts: multiple users authenticate (JWT sessions, API tokens, SSO); without it there is one local user.
	Accounts Capability = "accounts"
	// RowLevelSecurity: requests run inside a per-user RLS transaction.
	RowLevelSecurity Capability = "row-level-security"
	// SharedState: a Redis backplane shares rate limits, presence and stream resume across replicas.
	SharedState Capability = "shared-state"
)

// Set is the capabilities of one process.
type Set struct {
	profile Profile
	has     map[Capability]bool
}

// Of derives the profile and capabilities from a validated config.
func Of(cfg *config.Config) Set {
	s := Set{profile: Desktop, has: map[Capability]bool{}}
	store := cfg.Storage.Backend == config.StorageDatabase
	switch {
	case cfg.Mode == config.ModeCloud:
		s.profile = Cloud
	case store:
		s.profile = LocalDB
	}
	s.has[ServerStore] = store
	s.has[Accounts] = cfg.Auth.Enabled
	// The RLS middleware needs both a store and authenticated callers to scope to.
	s.has[RowLevelSecurity] = store && cfg.Auth.Enabled
	s.has[SharedState] = cfg.Redis.URL != ""
	return s
}

// NewSet returns the Set of a profile with exactly caps. Production derives its Set from the config (Of); tests use
// this to give a router the capabilities of a deployment their config does not describe (test routers run on a
// filesystem backend with the default config).
func NewSet(profile Profile, caps ...Capability) Set {
	s := Set{profile: profile, has: make(map[Capability]bool, len(caps))}
	for _, c := range caps {
		s.has[c] = true
	}
	return s
}

// Profile is the deployment profile.
func (s Set) Profile() Profile { return s.profile }

// Has reports whether every given capability is available.
func (s Set) Has(caps ...Capability) bool {
	for _, c := range caps {
		if !s.has[c] {
			return false
		}
	}
	return true
}
