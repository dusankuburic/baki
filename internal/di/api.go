package di

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"
	"pad-analyzer/internal/api"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/reqtx"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/sso"
	storageif "pad-analyzer/internal/storage/interfaces"
	wshub "pad-analyzer/internal/websocket"
)

// authzFlowChecker gates WebSocket room joins on read access to the flow,
// using the same AuthzService policy as the HTTP API, for the socket's actor
// (its ticket's credential, scopes included). It translates service-layer
// errors into the websocket package's sentinels so that package stays
// decoupled from the service layer.
type authzFlowChecker struct {
	authz *service.AuthzService
}

// CheckAccess is Can(FlowRead) through AuthorizeFlow, whose header names the flow's owner and organisation for the
// hub's access-change rechecks.
func (c *authzFlowChecker) CheckAccess(ctx context.Context, flowID string, actor *auth.Actor) (wshub.FlowOwner, error) {
	h, err := c.authz.AuthorizeFlow(ctx, actor, service.FlowRead, flowID)
	switch {
	case err == nil:
		return wshub.FlowOwner{UserID: h.OwnerID, OrgID: h.OrganizationID}, nil
	case errors.Is(err, storageif.ErrNotFound):
		return wshub.FlowOwner{}, wshub.ErrFlowNotFound
	case errors.Is(err, service.ErrPermissionDenied):
		return wshub.FlowOwner{}, wshub.ErrAccessDenied
	default:
		return wshub.FlowOwner{}, err
	}
}

// CanEdit reports whether the actor may edit the flow (FlowEdit): the
// "editor" rank and the write scope the HTTP write endpoints require. Room
// joins are deliberately gated lower (at "viewer"), so this is what keeps a
// read-only collaborator or a read-scoped token from sending block mutations
// over the socket. A scoped token without write is refused before any read.
func (c *authzFlowChecker) CanEdit(ctx context.Context, flowID string, actor *auth.Actor) bool {
	return c.authz.Can(ctx, actor, service.FlowEdit, service.FlowByID(flowID)) == nil
}

func ProvideFlowAccessChecker(authz *service.AuthzService, backend storageif.StorageBackend) wshub.FlowAccessChecker {
	if backend == nil {
		return nil // local mode: single desktop user, no room authz needed
	}
	return &authzFlowChecker{authz: authz}
}

// ProvideRedisPinger hands the Redis health to the system handler as the
// probe interface. Returns nil when Redis isn't configured so SystemHandler
// reports nothing about it in single-replica / local mode.
func ProvideRedisPinger(health *redisx.Health) api.RedisPinger {
	if !health.Configured() {
		return nil
	}
	return health
}

// ProvideSecurityConfig: requestTx and idem are the database's request transaction and Idempotency-Key store,
// nil without row-level security.
func ProvideSecurityConfig(cfg *config.Config, authMgr *auth.Manager, backend storageif.StorageBackend, requestTx reqtx.Opener, idem storageif.IdempotencyStore, orgSvc *collaboration.OrgService) *api.SecurityConfig {
	return &api.SecurityConfig{
		JWTEnabled:     cfg.Auth.Enabled,
		LocalUserID:    "local",
		LocalName:      "You",
		Token:          cfg.Auth.Secret,
		AuthMgr:        authMgr,
		Backend:        backend,
		RequestTx:      requestTx,
		Idempotency:    idem,
		OrgSvc:         orgSvc,
		TrustedProxies: cfg.Server.TrustedProxies,
		Features: api.FeatureFlags{
			DisableSignUp:       cfg.Features.DisableSignUp,
			LocalConfigEndpoint: cfg.Server.LocalConfigEndpoint,
		},
	}
}

// routeModule contributes handler H to the router's "routes" group as an api.Module; H stays injectable as
// its concrete type.
func routeModule[H api.Module]() any {
	return fx.Annotate(func(h H) api.Module { return h }, fx.ResultTags(`group:"routes"`))
}

var APIModule = fx.Options(
	// Each handler also joins the router's "routes" group; NewRouter mounts whatever the group holds.
	fx.Provide(
		routeModule[*api.SystemHandler](),
		routeModule[*api.FlowHandler](),
		routeModule[*api.LibraryHandler](),
		routeModule[*api.ChatHandler](),
		routeModule[*api.AnalysisHandler](),
		routeModule[*api.DashboardHandler](),
		routeModule[*api.ExportHandler](),
		routeModule[*api.AuthHandler](),
		routeModule[*api.AdminHandler](),
		routeModule[*api.ProviderHandler](),
		routeModule[*api.OrgHandler](),
		routeModule[*api.SharingHandler](),
	),
	fx.Provide(
		ProvideSecurityConfig,
		ProvideFlowAccessChecker,
		ProvideEventManager,
		// Adapter: *redisx.Health (unconfigured when PAD_REDIS_URL is unset) →
		// api.RedisPinger (see ProvideRedisPinger above).
		ProvideRedisPinger,
		api.NewSystemHandler,
		api.NewFlowHandler,
		api.NewLibraryHandler,
		api.NewChatHandler,
		api.NewAnalysisHandler,
		api.NewDashboardHandler,
		api.NewExportHandler,
		api.NewAuthHandler,
		func(cfg *config.Config) *mail.Service { return mail.NewService(cfg.Email, capability.Of(cfg)) },
		api.NewAdminHandler,
		api.NewProviderHandler,
		api.NewOrgHandler,
		api.NewSharingHandler,
		// The session and identity-link stores are roles of the backend (nil on the desktop), which fx only
		// provides whole.
		func(backend storageif.StorageBackend) storageif.RefreshTokenStore { return backend },
		func(backend storageif.StorageBackend) storageif.IdentityLinkStore { return backend },
		// SSO is off (a nil client) unless OIDC is configured.
		func(cfg *config.Config) api.SSOClient {
			if !cfg.Auth.SSO.Enabled() {
				return nil
			}
			return sso.NewClient(cfg.Auth.SSO)
		},
		api.NewRouter,
	),
	// A member who stops being an admin takes the org credentials they lent along, after the change commits.
	fx.Invoke(api.SubscribeLentCredentialRevocation),
	fx.Invoke(SubscribeRealtime),
)

// ProvideEventManager is the SSE hub; EmitToOrg resolves an organisation's members from the store (none without
// one: the desktop has no organisations). A deployment with the shared Redis (capability shared-state) also gets the
// fan-out: events, event ids with Last-Event-ID replay and subscriber presence across replicas.
func ProvideEventManager(lc fx.Lifecycle, cfg *config.Config, shutdownCh chan struct{}, backend storageif.StorageBackend, client *redis.Client, health *redisx.Health) *api.EventManager {
	var opts []api.EventManagerOption
	if backend != nil {
		opts = append(opts, api.WithOrgRoster(api.OrgRosterOf(backend)))
	}
	fanout := client != nil && capability.Of(cfg).Has(capability.SharedState)
	if fanout {
		opts = append(opts, api.WithFanout(redisx.NewSSEBackplane(client, health, redisx.BootOrigin(cfg.Runtime.InstanceID))))
	}
	m := api.NewEventManager(shutdownCh, opts...)
	if !fanout {
		return m
	}
	// Stopped after what emits through it (it is constructed first, so fx stops it last): the terminal events of
	// the chat streams that end at shutdown are still published to the other replicas.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() { defer close(done); m.RunFanout(ctx) }()
			return nil
		},
		OnStop: func(stop context.Context) error {
			cancel()
			select {
			case <-done:
			case <-stop.Done():
			}
			return nil
		},
	})
	return m
}

// SubscribeRealtime subscribes what tells clients and people that something changed: all async, so each runs
// once the write committed (a client refetching earlier read the old row) and never for a rolled-back one.
func SubscribeRealtime(bus *eventbus.Bus, router *api.Router, analysis *api.AnalysisHandler, sse *api.EventManager) {
	eventbus.Subscribe(bus, eventbus.Async, "ws-flow-changed", service.NotifyFlowViewers(router.FlowViewers()),
		eventbus.Local("the hub broadcasts through the WebSocket room backplane, which already reaches every replica's viewers when Redis is configured"))
	// An open socket whose access may have shrunk re-checks it at once, not at its 2-minute pass.
	wshub.SubscribeAccessRechecks(bus, router.WebSocketHub())
	// So does an event stream of a revoked token (a logout, a deleted API token): it ends at once.
	sse.SubscribeRevocations(bus)
	sse.SubscribeRemoteRevocations(bus)
	// The bell rows of a comment or an assignment must not be lost: when the bus's runner is full, the hand-off to
	// the handler's own alert runner runs inline instead.
	eventbus.Subscribe(bus, eventbus.Async, "finding-comment-fanout", analysis.OnFindingCommented, eventbus.InlineWhenSaturated(),
		eventbus.Local("sends an email and writes bell rows: a side effect that must happen once, and the comment text stays on this replica"))
	eventbus.Subscribe(bus, eventbus.Async, "finding-assignment-fanout", analysis.OnFindingAssigned, eventbus.InlineWhenSaturated(),
		eventbus.Local("sends an email and writes a bell row: a side effect that must happen once"))
}
