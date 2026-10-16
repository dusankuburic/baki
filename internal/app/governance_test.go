package app

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/scanner"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// TestProvideScanner_SweepsUnderTheLease: the scanner's sweep takes the database's lease, whose heartbeat ends a
// sweep whose lock was lost, not the plain lock through the adapter that cannot tell.
func TestProvideScanner_SweepsUnderTheLease(t *testing.T) {
	analysis, err := service.NewAnalysisService(nil, nil, nil, service.AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l := &leaseLocker{held: true} // another replica sweeps: the attempt asks, then skips
	s := provideScanner(serverStoreConfig(config.ModeCloud, true), testutil.NewFakeBackend(), l, analysis, nil, nil, nil, nil, nil, nil)
	defer s.Stop()
	s.ScanOnce(context.Background())
	onlyLeases(t, l, jobKey(t, "scanner"))
}

// TestProvideMigrationRunner_StartsUnderTheLease: the legacy migration takes the database's lease, which cancels a
// run whose lock was lost.
func TestProvideMigrationRunner_StartsUnderTheLease(t *testing.T) {
	cfg := serverStoreConfig(config.ModeCloud, true)
	cfg.Storage.DataDir = t.TempDir()
	l := &leaseLocker{held: true} // a migration runs on another replica
	r := provideMigrationRunner(cfg, testutil.NewFakeBackend(), l)
	defer r.Stop()
	if err := r.Start(); !errors.Is(err, api.ErrMigrationRunning) {
		t.Fatalf("Start while another replica holds the lock = %v, want ErrMigrationRunning", err)
	}
	onlyLeases(t, l, jobKey(t, "legacy_migration"))
}

// deployments are the profiles the governance wiring is asked about, with whether each has a server store.
var deployments = []struct {
	name  string
	cfg   func() *config.Config
	store bool
}{
	{"desktop", config.Default, false},
	{"local+db without accounts", func() *config.Config { return serverStoreConfig(config.ModeLocal, false) }, true},
	{"local+db with accounts", func() *config.Config { return serverStoreConfig(config.ModeLocal, true) }, true},
	{"cloud", func() *config.Config { return serverStoreConfig(config.ModeCloud, true) }, true},
}

// TestProvideScanNowFunc_WhereverTheServerStoreIs (R20): the admin "scan now" is a manual run of the sweep, which
// runs wherever there is a server store to sweep (PAD_SCAN_INTERVAL schedules it there). It used to be cloud-only, so
// local+db, where the scheduled sweep and the retention purge run, answered 503.
func TestProvideScanNowFunc_WhereverTheServerStoreIs(t *testing.T) {
	for _, d := range deployments {
		t.Run(d.name, func(t *testing.T) {
			s := scanner.New(testutil.NewFakeBackend(), nil, nil, nil, 0, nil)
			defer s.Stop()
			if got := provideScanNowFunc(s, d.cfg()) != nil; got != d.store {
				t.Errorf("scan-now wired = %v, want %v (a server store to sweep)", got, d.store)
			}
		})
	}
}

// TestProvideMigrationRunner_WhereverTheServerStoreIs (R20): the legacy migration copies PAD_DATA_DIR into the server
// store, so it is available wherever there is one, not only in cloud mode. Without accounts the local user adopts
// the migrated flows.
func TestProvideMigrationRunner_WhereverTheServerStoreIs(t *testing.T) {
	for _, d := range deployments {
		t.Run(d.name, func(t *testing.T) {
			cfg := d.cfg()
			cfg.Storage.DataDir = t.TempDir()
			r := provideMigrationRunner(cfg, testutil.NewFakeBackend(), nil)
			defer r.Stop()
			if got := r.Enabled(); got != d.store {
				t.Errorf("migration enabled = %v, want %v (a server store to copy into)", got, d.store)
			}
		})
	}
}
