package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
)

// TestProvideMigrationRunner_OpensTheDataDirReadOnly: the migration source used
// to be the filesystem backend, whose constructor created a missing PAD_DATA_DIR,
// so a mistyped path enabled a migration of an empty directory. The read-only
// source refuses it and the runner stays disabled.
func TestProvideMigrationRunner_OpensTheDataDirReadOnly(t *testing.T) {
	cfg := &config.Config{Mode: config.ModeCloud}
	dst := testutil.NewFakeBackend()

	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "mistyped")
	if provideMigrationRunner(cfg, dst, nil).Enabled() {
		t.Error("a missing data directory enabled the migration")
	}
	if _, err := os.Stat(cfg.Storage.DataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the missing data directory was created (stat: %v)", err)
	}

	cfg.Storage.DataDir = t.TempDir()
	if !provideMigrationRunner(cfg, dst, nil).Enabled() {
		t.Error("an existing data directory should enable the migration")
	}
}
