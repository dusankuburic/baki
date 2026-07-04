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
// accessJTI / accessExp identify the ACCESS TOKEN that authenticated the
// ticket-issuance request (embedded in the WS ticket at issuance). The re-authz
// goroutine re-checks accessJTI against the token blacklist so a logout /
// explicit revoke disconnects the live socket, and an expiry timer closes the
// connection when the underlying access token expires — matching the SSE
// channel's semantics. Pass accessJTI="" and a zero accessExp to disable both
// (used in tests / local mode with no blacklist).
//
// The caller is responsible for extracting userID and displayName from the
// authenticated request context (e.g. from auth.ClaimsFromContext) before
// calling this handler.
func Handler(hub *Hub, userID, displayName string, allowedOrigins []string, checker FlowAccessChecker, accessJTI string, accessExp time.Time, isRevoked func(string) bool) http.HandlerFunc {
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
		// Per-user connection cap: enforced BEFORE the upgrade so a refused
		// connection never allocates goroutines, buffers, or a client slot. A
		// single account can otherwise open thousands of live sockets and
		// exhaust memory/goroutines for the whole process.
		releaseConn, ok := hub.AcquireConn(userID)
		if !ok {
			http.Error(w, "Too many connections", http.StatusServiceUnavailable)
			return
		}
		defer releaseConn()

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

		// Periodic re-authz: if the user's access is revoked (token blacklist
		// hit, flow ACL change, access-token expiry), close the connection so a
		// logged-out / revoked user stops receiving real-time broadcasts.
		//
		// Deferred so a panic anywhere below still cancels the context and stops
		// the re-authz goroutine (previously authzCancel was a plain statement
		// after client.Run(), which leaked the goroutine + context on panic).
		authzCtx, authzCancel := context.WithCancel(context.Background())
		defer authzCancel()
		if checker != nil || isRevoked != nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Warn("websocket re-authz goroutine panicked", "err", r)
					}
				}()
				// Re-check at a 2-minute cadence (matches the SSE channel) so a
				// revocation is observed promptly rather than up to 5 minutes.
				ticker := time.NewTicker(2 * time.Minute)
				defer ticker.Stop()
				// expiryTimer fires once when the underlying access token
				// expires, closing the connection — otherwise a live socket
				// would outlive the credential indefinitely.
				var expiryTimer *time.Timer
				if accessJTI != "" && !accessExp.IsZero() {
					if d := time.Until(accessExp); d > 0 {
						expiryTimer = time.AfterFunc(d, func() {
							slog.Info("websocket: disconnecting client after access token expired",
								"flowId", flowID, "userID", userID)
							_ = conn.Close()
						})
					}
				}
				if expiryTimer != nil {
					defer expiryTimer.Stop()
				}
				for {
					select {
					case <-authzCtx.Done():
						return
					case <-ticker.C:
						// Check the ACCESS token's revocation (logout, password
						// change, admin revoke) — the ticket's own JTI was only
						// blacklisted for ~30s at consumption, so re-checking it
						// would never fire after that.
						if isRevoked != nil && accessJTI != "" && isRevoked(accessJTI) {
							slog.Info("websocket: disconnecting client after token revoked",
								"flowId", flowID, "userID", userID)
							conn.Close()
							return
						}
						if checker != nil {
							if err := checker.CheckAccess(authzCtx, flowID, userID); err != nil {
								slog.Info("websocket: disconnecting client after access revoked",
									"flowId", flowID, "userID", userID)
								conn.Close()
								return
							}
						}
					}
				}
			}()
		}

		client := NewClient(hub, conn, userID, displayName, flowID)
		client.Run()
	}
}
