package di

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"
	"pad-analyzer/internal/api"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/sso"
	storageif "pad-analyzer/internal/storage/interfaces"
	wshub "pad-analyzer/internal/websocket"
)

// authzFlowChecker gates WebSocket room joins on read access to the flow,
// using the same AuthzService policy as the HTTP API. It translates
// service-layer errors into the websocket package's sentinels so that package
// stays decoupled from the service layer.
type authzFlowChecker struct {
	authz *service.AuthzService
}

func (c *authzFlowChecker) CheckAccess(ctx context.Context, flowID, userID string) error {
	err := c.authz.CheckFlowAccessByID(ctx, flowID, userID, "viewer")
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storageif.ErrNotFound):
		return wshub.ErrFlowNotFound
	case errors.Is(err, service.ErrPermissionDenied):
		return wshub.ErrAccessDenied
	default:
		return err
	}
}

// CanEdit reports whether the user holds the "editor" rank on the flow — the
// same rank the HTTP write endpoints require. Room joins are deliberately
// gated lower (at "viewer"), so this is what keeps a read-only collaborator
// from sending block mutations over the socket.
func (c *authzFlowChecker) CanEdit(ctx context.Context, flowID, userID string) bool {
	return c.authz.CheckFlowAccessByID(ctx, flowID, userID, "editor") == nil
}

func ProvideFlowAccessChecker(authz *service.AuthzService, backend storageif.StorageBackend) wshub.FlowAccessChecker {
	if backend == nil {
		return nil // local mode: single desktop user, no room authz needed
	}
	return &authzFlowChecker{authz: authz}
}

// ProvideRedisPinger adapts the optional *redis.Client to the readiness-probe
// interface. Returns nil when Redis isn't configured so SystemHandler skips
// the check entirely in single-replica / local mode.
func ProvideRedisPinger(client *redis.Client) api.RedisPinger {
	if client == nil {
		return nil
	}
	return redisPingerAdapter{client: client}
}

type redisPingerAdapter struct{ client *redis.Client }

func (a redisPingerAdapter) Ping(ctx context.Context) error {
	return a.client.Ping(ctx).Err()
}

func ProvideSecurityConfig(cfg *config.Config, authMgr *auth.Manager, backend storageif.StorageBackend, orgSvc *collaboration.OrgService) *api.SecurityConfig {
	return &api.SecurityConfig{
		JWTEnabled:     cfg.Auth.Enabled,
		LocalUserID:    "local",
		LocalName:      "You",
		Token:          cfg.Auth.Secret,
		AuthMgr:        authMgr,
		Backend:        backend,
		OrgSvc:         orgSvc,
		TrustedProxies: cfg.Server.TrustedProxies,
		Features: api.FeatureFlags{
			DisableSignUp:       cfg.Features.DisableSignUp,
			LocalConfigEndpoint: cfg.Server.LocalConfigEndpoint,
		},
	}
}

func ProvideHandlers(
	sys *api.SystemHandler,
	flow *api.FlowHandler,
	lib *api.LibraryHandler,
	chat *api.ChatHandler,
	analysis *api.AnalysisHandler,
	dashboard *api.DashboardHandler,
	export *api.ExportHandler,
	authH *api.AuthHandler,
	admin *api.AdminHandler,
	provider *api.ProviderHandler,
	org *api.OrgHandler,
	sharing *api.SharingHandler,
) api.Handlers {
	return api.Handlers{
		Sys:       sys,
		Flow:      flow,
		Library:   lib,
		Chat:      chat,
		Analysis:  analysis,
		Dashboard: dashboard,
		Export:    export,
		Auth:      authH,
		Admin:     admin,
		Provider:  provider,
		Org:       org,
		Sharing:   sharing,
	}
}

var APIModule = fx.Options(
	fx.Provide(
		ProvideSecurityConfig,
		ProvideHandlers,
		ProvideFlowAccessChecker,
		api.NewEventManager,
		// Adapter: *redis.Client (nil when PAD_REDIS_URL is unset) →
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
		func(cfg *config.Config) *mail.Service { return mail.NewService(cfg.Email, cfg.Mode) },
		// CIWebhookSecret is sourced from PAD_CI_WEBHOOK_SECRET; empty disables
		// the inbound /api/integrations/ci endpoint (it returns 503).
		func(cfg *config.Config) api.CIWebhookSecret {
			return api.CIWebhookSecret(cfg.Governance.InboundWebhookSecret)
		},
		api.NewAdminHandler,
		api.NewProviderHandler,
		api.NewOrgHandler,
		api.NewSharingHandler,
		func(backend storageif.StorageBackend) api.RefreshTokenStore {
			if ts, ok := backend.(api.RefreshTokenStore); ok {
				return ts
			}
			return nil
		},
		// SSO wiring: both are nil unless OIDC is configured and the backend
		// can persist identity links (Postgres). The auth handler treats a nil
		// pair as "SSO disabled".
		func(cfg *config.Config) api.SSOClient {
			if !cfg.Auth.SSO.Enabled() {
				return nil
			}
			return sso.NewClient(cfg.Auth.SSO)
		},
		func(backend storageif.StorageBackend) api.IdentityStore {
			if is, ok := backend.(api.IdentityStore); ok {
				return is
			}
			return nil
		},
		api.NewRouter,
	),
)
