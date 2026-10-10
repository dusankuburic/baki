package audit

import (
	"context"
	"os"
	"path/filepath"

	"go.uber.org/fx"

	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Provide builds the deployment's recorder for fx (no store in desktop mode: it records nothing). The pool
// starts with the app and drains when the app stops. internal/app constructs it from an early fx.Invoke (initAuditTrail), so its stop hook runs after the HTTP server's, whose
// drain of the background runners still records.
func Provide(lc fx.Lifecycle, cfg *config.Config, store storageif.StorageBackend) *Recorder {
	var rc Config
	switch dir := cfg.Governance.AuditSpillDir; dir {
	case "off":
	case "":
		rc.SpillDir = filepath.Join(os.TempDir(), "baki-audit-spill")
	default:
		rc.SpillDir = dir
	}
	var as storageif.AuditStore
	if store != nil {
		as = store
	}
	r := New(as, rc)
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			r.Start()
			return nil
		},
		OnStop: func(context.Context) error {
			r.Stop()
			return nil
		},
	})
	return r
}
