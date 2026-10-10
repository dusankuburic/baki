package capability

import (
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
)

// TestOf pins the matrix in docs/architecture/modes.md.
func TestOf(t *testing.T) {
	desktop := config.Default()

	localDB := config.Default()
	localDB.Storage.Backend = config.StorageDatabase

	cloud := config.Default()
	cloud.Mode = config.ModeCloud
	cloud.Auth.Enabled = true
	cloud.Storage.Backend = config.StorageDatabase

	cloudRedis := config.Default()
	*cloudRedis = *cloud
	cloudRedis.Redis.URL = secret.New("redis://cache:6379/0")

	cases := []struct {
		name    string
		cfg     *config.Config
		profile Profile
		has     []Capability
		lacks   []Capability
	}{
		{"desktop", desktop, Desktop, nil, []Capability{ServerStore, Accounts, RowLevelSecurity, SharedState}},
		{"local+db", localDB, LocalDB, []Capability{ServerStore}, []Capability{Accounts, RowLevelSecurity, SharedState}},
		{"cloud", cloud, Cloud, []Capability{ServerStore, Accounts, RowLevelSecurity}, []Capability{SharedState}},
		{"cloud with redis", cloudRedis, Cloud, []Capability{ServerStore, Accounts, RowLevelSecurity, SharedState}, nil},
	}
	for _, tc := range cases {
		s := Of(tc.cfg)
		if s.Profile() != tc.profile {
			t.Errorf("%s: profile %q, want %q", tc.name, s.Profile(), tc.profile)
		}
		if !s.Has(tc.has...) {
			t.Errorf("%s: lacks one of %v", tc.name, tc.has)
		}
		for _, c := range tc.lacks {
			if s.Has(c) {
				t.Errorf("%s: unexpectedly has %q", tc.name, c)
			}
		}
	}
}

func TestNewSet(t *testing.T) {
	s := NewSet(Cloud, ServerStore, Accounts)
	if s.Profile() != Cloud || !s.Has(ServerStore, Accounts) || s.Has(RowLevelSecurity) || s.Has(ServerStore, SharedState) {
		t.Errorf("NewSet(Cloud, ServerStore, Accounts) = %+v", s)
	}
	if !NewSet(Desktop).Has() {
		t.Error("every Set has the empty requirement")
	}
}
