package app

import (
	"context"
	"fmt"
	"os"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/platform/lock"
	"pad-analyzer/internal/scanner"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// provideNotifier builds the deployment's channels (PAD_NOTIFY_*, config.DeploymentChannels) through the notify
// kind registry, under the operator's egress policy (egress.ForOperator: they are not a tenant's input).
func provideNotifier(cfg *config.Config, mailer *mail.Service) (*notify.Dispatcher, error) {
	var channels []notify.ChannelConfig
	for _, ch := range cfg.Governance.DeploymentChannels() {
		channels = append(channels, notify.ChannelConfig{Kind: ch.Kind, Values: ch.Values})
	}
	// The email channel only fires with a real SMTP sender: mailer is always non-nil (log-only fallback), and the
	// email kind is left out when the sender reports disabled.
	var sender notify.EmailSender
	if mailer != nil {
		sender = mailSvcAdapter{svc: mailer}
	}
	d, err := notify.New(notify.Config{
		Policy:   egress.ForOperator(capability.Of(cfg)),
		Channels: channels,
		Mailer:   sender,
		Envelope: notify.Envelope{UserAgent: notify.UserAgent(service.Version)},
	})
	if err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	return d, nil
}

// mailSvcAdapter bridges *mail.Service to notify.EmailSender, hiding the concrete
// type and honouring Enabled() (the log-only mailer reports disabled).
type mailSvcAdapter struct{ svc *mail.Service }

func (a mailSvcAdapter) Enabled() bool { return a.svc != nil && a.svc.Enabled() }

func (a mailSvcAdapter) SendAlert(ctx context.Context, to, subject, plainBody, htmlBody string) error {
	return a.svc.SendAlert(ctx, to, subject, plainBody, htmlBody)
}

// provideScanner wires the periodic flow scanner. A zero interval (the default;
// config.Load refuses a negative or malformed one) or a missing backend leaves
// it disabled (opt-in; it needs the server store). The env-level notifier is optional: alerts
// always record in-app and route to each org's own channels; PAD_NOTIFY_* only
// adds a deployment-wide sink. The sweeps take the database's lease (nil without
// one: the process sweeps alone), so a sweep whose lock is lost is cancelled.
// With more than one replica (PAD_MAX_REPLICAS > 1) the flows' last complete
// scans are shared through Redis, so a sweep skips what another replica's
// sweep already analyzed; a single replica keeps them in memory only.
func provideScanner(cfg *config.Config, backend storageif.StorageBackend, leaser lock.Leaser, analysisSvc *service.AnalysisService, notifier *notify.Dispatcher, channels *service.ChannelService, eventMgr *api.EventManager, ruleProfiles *service.RuleProfileResolver, dashboard *service.DashboardService, redisClient *redis.Client) *scanner.Scanner {
	var analyze scanner.AnalyzeFunc
	if analysisSvc != nil {
		// Background, not AnalyzeFlow: the sweep's context carries no user, so
		// AnalyzeFlow's progress events went to EmitTo("") — a cross-tenant
		// broadcast of every scanned flow's progress.
		analyze = analysisSvc.AnalyzeFlowBackground
	}
	// A nil service stays a nil interface: the scanner then delivers to no organisation's channels.
	var orgChannels scanner.OrgChannels
	if channels != nil {
		orgChannels = channels
	}
	opts := []scanner.Option{scanner.WithLeaser(leaser)}
	if cfg.Storage.MaxReplicas > 1 {
		opts = append(opts, scanner.WithSharedScans(redisClient))
	}
	s := scanner.New(backend, nil, analyze, notifier, cfg.Governance.ScanInterval.Std(), orgChannels, opts...)
	// Real-time SSE push: a newly-detected alert pings connected clients who
	// can see the flow. EventManager is always non-nil in the fx graph; safe
	// even in local mode.
	s.SetEventNotifier(eventMgr)
	// Lets a sweep skip flows whose scan inputs are unchanged. The digest is
	// of the profile the AnalysisService itself analyzes with, so a rule
	// change is seen as a changed input.
	if ruleProfiles != nil {
		s.SetRuleSetDigest(func(ctx context.Context, orgID string) string {
			p := ruleProfiles.Resolve(ctx, orgID)
			return scanner.DigestRuleSet(p.Settings, p.Rules)
		})
	}
	// Without accounts every flow is the one local user's: its "new rules"
	// notices are the deployment's, not a user row's.
	s.SetSingleTenant(!capability.Of(cfg).Has(capability.Accounts))
	// A legacy health snapshot (before rule sets) is re-recorded by the sweep, as an interactive analysis would.
	if dashboard != nil {
		s.SetHealthRecorder(dashboard.RecordAnalysis)
	}
	return s
}

// initScanner starts the scanner on app start and stops it on shutdown; Start
// is a no-op when scanning is disabled.
func initScanner(lc fx.Lifecycle, s *scanner.Scanner) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { s.Start(); return nil },
		OnStop:  func(context.Context) error { s.Stop(); return nil },
	})
}

// provideMigrationRunner wires the optional local→cloud data migration runner:
// it needs BOTH a source (PAD_DATA_DIR at a legacy filesystem-backed server's
// data directory, opened read-only) and the server store (Postgres) as its
// destination, in cloud or local+db. With either absent the runner is disabled
// and the admin endpoint reports 503.
func provideMigrationRunner(cfg *config.Config, dst storageif.StorageBackend, leaser lock.Leaser) *api.MigrationRunner {
	if !capability.Of(cfg).Has(capability.ServerStore) || cfg.Storage.DataDir == "" {
		return api.NewMigrationRunner(nil)
	}
	src, err := filesystem.OpenLegacySource(cfg.Storage.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: migration source unavailable (%s): %v; migration disabled\n", cfg.Storage.DataDir, err)
		return api.NewMigrationRunner(nil)
	}
	runner := api.NewMigrationRunner(migration.New(src, dst))
	// Cross-replica single-run guard: without the Postgres advisory lock two
	// pods could both pass the in-process guard and run the same migration. The
	// lease also cancels a run whose lock was lost.
	// The run and its result go to job_runs, so any replica answers the status endpoint for it.
	return runner.WithLeaser(leaser).WithJobStore(dst)
}

// provideScanNowFunc wires the admin "scan now" action to the scanner wherever
// there is a server store to sweep (cloud and local+db, as the scheduled sweep);
// without one it returns nil so the endpoint 503s. ScanNow starts the sweep and
// says whether it did, so the endpoint reports a sweep that could not start.
func provideScanNowFunc(s *scanner.Scanner, cfg *config.Config) api.ScanNowFunc {
	if s == nil || !capability.Of(cfg).Has(capability.ServerStore) {
		return nil
	}
	return s.ScanNow
}
