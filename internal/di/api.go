package di

import (
	"context"

	"go.uber.org/fx"
	"pad-analyzer/internal/api"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	wshub "pad-analyzer/internal/websocket"
)

type storageFlowChecker struct {
	backend storageif.StorageBackend
}

func (c *storageFlowChecker) FlowExists(ctx context.Context, flowID string) (bool, error) {
	_, err := c.backend.LoadFlow(ctx, flowID)
	if err == storageif.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func ProvideFlowAccessChecker(backend storageif.StorageBackend) wshub.FlowAccessChecker {
	if backend == nil {
		return nil
	}
	return &storageFlowChecker{backend: backend}
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
	}
}

func ProvideHandlers(
	sys *api.SystemHandler,
	flow *api.FlowHandler,
	lib *api.LibraryHandler,
	chat *api.ChatHandler,
	analysis *api.AnalysisHandler,
	export *api.ExportHandler,
	authH *api.AuthHandler,
	admin *api.AdminHandler,
	provider *api.ProviderHandler,
	org *api.OrgHandler,
	sharing *api.SharingHandler,
) api.Handlers {
	return api.Handlers{
		Sys:      sys,
		Flow:     flow,
		Library:  lib,
		Chat:     chat,
		Analysis: analysis,
		Export:   export,
		Auth:     authH,
		Admin:    admin,
		Provider: provider,
		Org:      org,
		Sharing:  sharing,
	}
}

var APIModule = fx.Options(
	fx.Provide(
		ProvideSecurityConfig,
		ProvideHandlers,
		ProvideFlowAccessChecker,
		api.NewEventManager,
		api.NewSystemHandler,
		api.NewFlowHandler,
		api.NewLibraryHandler,
		api.NewChatHandler,
		api.NewAnalysisHandler,
		api.NewExportHandler,
		api.NewAuthHandler,
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
		api.NewRouter,
	),
)
