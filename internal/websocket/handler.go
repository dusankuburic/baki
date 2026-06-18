package websocket

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var flowIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// ErrAccessDenied is returned by a FlowAccessChecker when the user is not
// allowed to join the flow's room. Kept package-local (rather than reusing
// service errors) so this package stays decoupled from the service layer;
// adapters translate their domain errors into this sentinel.
var ErrAccessDenied = errors.New("flow access denied")

// ErrFlowNotFound is returned by a FlowAccessChecker when the flow does not
// exist.
var ErrFlowNotFound = errors.New("flow not found")

type FlowAccessChecker interface {
	// CheckAccess returns nil if userID may join flowID's room,
	// ErrFlowNotFound if the flow doesn't exist, and ErrAccessDenied if the
	// user lacks read access to it.
	CheckAccess(ctx context.Context, flowID, userID string) error
}

// Handler returns an http.HandlerFunc that upgrades the connection to WebSocket
// and runs the client loop.
//
// allowedOrigins is the same allowlist used by the HTTP router. An empty list
// means only non-browser (empty Origin) connections are accepted.
//
// The caller is responsible for extracting userID and displayName from the
// authenticated request context (e.g. from auth.ClaimsFromContext) before
// calling this handler.
func Handler(hub *Hub, userID, displayName string, allowedOrigins []string, checker FlowAccessChecker, jti string, isRevoked func(string) bool) http.HandlerFunc {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // non-browser callers (curl, tests) are fine
			}
			for _, o := range allowedOrigins {
				if strings.EqualFold(o, origin) {
					return true
				}
			}
			return false
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		flowID := r.URL.Query().Get("flowId")
		if flowID == "" {
			http.Error(w, "flowId query parameter is required", http.StatusBadRequest)
			return
		}
		if !flowIDPattern.MatchString(flowID) {
			http.Error(w, "invalid flowId format", http.StatusBadRequest)
			return
		}

		if checker != nil {
			switch err := checker.CheckAccess(r.Context(), flowID, userID); {
			case err == nil:
				// allowed
			case errors.Is(err, ErrFlowNotFound):
				http.Error(w, "flow not found", http.StatusNotFound)
				return
			case errors.Is(err, ErrAccessDenied):
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			default:
				slog.Error("websocket: flow access check failed", "flowId", flowID, "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		// Periodic re-authz: if the user's access to this flow is revoked
		// mid-session (org membership removed, collaborator deleted, account
		// disabled), close the connection so they stop receiving broadcasts.
		authzCtx, authzCancel := context.WithCancel(context.Background())
		if checker != nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Warn("websocket re-authz goroutine panicked", "err", r)
					}
				}()
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-authzCtx.Done():
						return
					case <-ticker.C:
						// Check token blacklist first — a logged-out / explicitly
						// revoked session disconnects on the next tick even if
						// flow access is still technically valid. (Password
						// change and refresh-replay revoke only refresh tokens,
						// so those sessions end when the access token expires.)
						if isRevoked != nil && jti != "" && isRevoked(jti) {
							slog.Info("websocket: disconnecting client after token revoked",
								"flowId", flowID, "userID", userID)
							conn.Close()
							return
						}
						if err := checker.CheckAccess(authzCtx, flowID, userID); err != nil {
							slog.Info("websocket: disconnecting client after access revoked",
								"flowId", flowID, "userID", userID)
							conn.Close()
							return
						}
					}
				}
			}()
		}

		client := NewClient(hub, conn, userID, displayName, flowID)
		client.Run()
		authzCancel()
	}
}
