package app

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/redisx"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
)

func provideAuthManager(lc fx.Lifecycle, cfg *config.Config, pool storagedb.SQLProvider, redisClient *redis.Client) (*auth.Manager, error) {
	token := cfg.Auth.Secret
	if !token.IsSet() {
		// loadConfig resolves it (config.ResolveRuntimeSecrets); empty means a caller skipped that, so fail
		// closed rather than mint a secret nothing else knows about.
		return nil, fmt.Errorf("auth secret unresolved: run config.ResolveRuntimeSecrets before building the auth manager")
	}

	var blacklist auth.BlacklistStore
	if cfg.Auth.Enabled {
		if pool != nil {
			// The Postgres table stays the shared source of truth; the cache
			// answers the per-request revocation check from a local copy
			// instead of a SELECT each. With the Redis backplane a revocation
			// fans out to the other replicas at once; the periodic reload is
			// the safety net. Until the first load, and whenever the
			// subscription is down, misses go to the table.
			opts := auth.CachedBlacklistOptions{}
			if redisClient != nil {
				ps := redisx.NewPubSub(redisClient)
				opts.Publisher, opts.Subscriber = ps, ps
			}
			cached := auth.NewCachedBlacklist(storagedb.NewPostgresBlacklist(pool.DB()), opts)
			blacklist = cached
			// Lifetime context for the cache's loops (fx's OnStart ctx only
			// bounds the start); OnStop cancels it then stops the cache, which
			// stops the Postgres store's own cleanup loop too.
			loopCtx, cancelLoops := context.WithCancel(context.Background())
			lc.Append(fx.Hook{
				OnStart: func(context.Context) error {
					cached.Start(loopCtx)
					return nil
				},
				OnStop: func(context.Context) error {
					cancelLoops()
					cached.Stop()
					return nil
				},
			})
		} else {
			memBl := auth.NewTokenBlacklist()
			blacklist = memBl
			lc.Append(fx.Hook{
				OnStop: func(ctx context.Context) error {
					memBl.Stop()
					return nil
				},
			})
		}
	}

	return auth.NewManager(token.Reveal(), blacklist), nil
}

// provideOrgService keeps organisations in the server store; without one (the desktop) in memory.
func provideOrgService(cfg *config.Config, backend storageif.StorageBackend, bus *eventbus.Bus) *collaboration.OrgService {
	orgStore := collaboration.NewMemOrgStore()
	if capability.Of(cfg).Has(capability.ServerStore) {
		orgStore = backend
	}
	return collaboration.NewOrgService(orgStore, collaboration.WithEventBus(bus))
}
