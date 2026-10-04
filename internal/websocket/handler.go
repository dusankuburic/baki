package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// writeError emits the API-standard error envelope ({code,message,requestId}).
// Miniature local copy of api/render's Error (importing it would cycle:
// api → websocket). Keep the field names in sync.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestId,omitempty"`
	}{Code: codeFor(status), Message: msg})
}

func codeFor(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusBadRequest:
		return "BAD_REQUEST"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusServiceUnavailable:
		return "SERVICE_UNAVAILABLE"
	default:
		return "ERROR"
	}
}

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

	// CanEdit reports whether userID may send MUTATING events for flowID —
	// the same "editor" rank the HTTP write endpoints require.
	//
	// Joining is gated at "viewer" on purpose: presence and cursor traffic is
	// what read-only collaborators are there for, but mutations must not be
	// relayable over the socket by a collaborator the HTTP write endpoints
	// would refuse. Deliberately part of the REQUIRED interface rather than an
	// optional type assertion: a new adapter that forgets it should fail to
	// compile, not silently fail open.
	//
	// The editor rank includes read access (CanEdit true implies CheckAccess
	// nil); the re-authz loop relies on this to skip the read check for a user
	// who can still edit (Client.recheckAccess).
	CanEdit(ctx context.Context, flowID, userID string) bool
}

// Handler returns an http.HandlerFunc that upgrades the connection to WebSocket
// and runs the client loop.
//
// allowedOrigins is the same allowlist used by the HTTP router; an empty list
// means only non-browser (empty Origin) connections are accepted.
//
// accessJTI / accessExp identify the ACCESS TOKEN that authenticated the
// ticket-issuance request (embedded in the WS ticket at issuance). The re-authz
// goroutine re-checks accessJTI against the token blacklist so a logout /
// revoke disconnects the live socket, and an expiry timer closes the connection
// when the access token expires — matching the SSE channel. Pass accessJTI=""
// and a zero accessExp to disable both (tests / local mode with no blacklist).
// The caller extracts userID and displayName from the authenticated request
// context (e.g. auth.ClaimsFromContext) before calling this handler.
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
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Per-user connection cap: enforced BEFORE the upgrade so a refused
		// connection never allocates goroutines, buffers, or a client slot. A
		// single account can otherwise open thousands of live sockets and
		// exhaust memory/goroutines for the whole process.
		releaseConn, ok := hub.AcquireConn(userID)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "too many connections")
			return
		}
		defer releaseConn()

		flowID := r.URL.Query().Get("flowId")
		if flowID == "" {
			writeError(w, http.StatusBadRequest, "flowId query parameter is required")
			return
		}
		if !flowIDPattern.MatchString(flowID) {
			writeError(w, http.StatusBadRequest, "invalid flowId format")
			return
		}

		if checker != nil {
			switch err := checker.CheckAccess(r.Context(), flowID, userID); {
			case err == nil:
				// allowed
			case errors.Is(err, ErrFlowNotFound):
				writeError(w, http.StatusNotFound, "flow not found")
				return
			case errors.Is(err, ErrAccessDenied):
				writeError(w, http.StatusForbidden, "forbidden")
				return
			default:
				slog.Error("websocket: flow access check failed", "flowId", flowID, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		// No checker means local/desktop mode: a single user on their own
		// machine, who is always the editor of their own flow.
		canEdit := true
		if checker != nil {
			canEdit = checker.CanEdit(r.Context(), flowID, userID)
		}

		client := NewClient(hub, conn, userID, displayName, flowID, canEdit)

		// Periodic re-authz: if the user's access is revoked (token blacklist
		// hit, flow ACL change, access-token expiry), close the connection so a
		// logged-out / revoked user stops receiving real-time broadcasts. See
		// Client.runReauthzLoop for the full mechanism.
		//
		// Deferred so a panic anywhere below still cancels the context and stops
		// the re-authz goroutine (a plain authzCancel after client.Run() leaked
		// the goroutine + context on panic).
		authzCtx, authzCancel := context.WithCancel(context.Background())
		defer authzCancel()
		if checker != nil || isRevoked != nil {
			go client.runReauthzLoop(authzCtx, checker, accessJTI, accessExp, isRevoked)
		}

		client.Run()
	}
}
