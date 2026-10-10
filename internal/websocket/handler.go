package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"pad-analyzer/internal/auth"
	platformdrain "pad-analyzer/internal/platform/drain"
	"pad-core/logger"
)

// draining is the server's draining state the upgrade consults; nil is the process's (drain.Process), tests set
// their own. (The package is imported under another name: this package's tests declare a drain helper.)
var draining *platformdrain.State

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

// FlowAccessChecker decides what a socket's actor may do in a flow's room. The
// actor is the one the connect ticket stands for (auth.ActorFromWSTicket), so
// a scoped API token is judged with its scopes, as on the HTTP routes.
type FlowAccessChecker interface {
	// CheckAccess returns who flowID belongs to and nil if actor may join
	// flowID's room, ErrFlowNotFound if the flow doesn't exist, and
	// ErrAccessDenied if the actor lacks read access to it.
	CheckAccess(ctx context.Context, flowID string, actor *auth.Actor) (FlowOwner, error)

	// CanEdit reports whether actor may send MUTATING events for flowID —
	// the same "editor" rank and write scope the HTTP write endpoints require.
	//
	// Joining is gated at "viewer" on purpose: presence and cursor traffic is
	// what read-only collaborators are there for, but mutations must not be
	// relayable over the socket by a collaborator the HTTP write endpoints
	// would refuse. Deliberately part of the REQUIRED interface rather than an
	// optional type assertion: a new adapter that forgets it should fail to
	// compile, not silently fail open.
	//
	// The editor rank includes read access (CanEdit true implies CheckAccess
	// nil); the re-authz loop relies on this to skip the read check for an
	// actor who can still edit (Client.recheckAccess).
	CanEdit(ctx context.Context, flowID string, actor *auth.Actor) bool
}

// FlowOwner is who a flow belongs to, as CheckAccess read it at connect. A
// socket keeps its flow's, so the hub finds the sockets an organisation's
// deletion or an account's erasure reaches without reading storage again
// (Hub.RecheckOrg, Hub.RecheckOwner). Both are fixed while the flow exists,
// except that deleting its organisation detaches it, which is the change the
// org id is kept for.
type FlowOwner struct {
	UserID string
	OrgID  string
}

// Identity is who a socket connects as, taken from its connect ticket.
type Identity struct {
	// Actor is the credential the ticket was issued from (auth.ActorFromWSTicket):
	// its user, and its scopes when it is a scoped API token. Never modified.
	Actor *auth.Actor
	// DisplayName is the name peers see in presence events.
	DisplayName string
	// AccessJTI / AccessExp identify the ACCESS TOKEN (or PAT) that
	// authenticated the ticket-issuance request. The re-authz goroutine
	// re-checks AccessJTI against the token blacklist so a logout / revoke
	// disconnects the live socket, and an expiry timer closes the connection
	// when the credential expires — matching the SSE channel. Empty and zero
	// disable both (tests / local mode with no blacklist).
	AccessJTI string
	AccessExp time.Time
}

// Handler returns an http.HandlerFunc that upgrades the connection to WebSocket
// and runs the client loop for id.
//
// allowedOrigins is the same allowlist used by the HTTP router; an empty list
// means only non-browser (empty Origin) connections are accepted.
//
// The caller authenticates the request (the connect ticket) and builds id from
// it before calling this handler.
func Handler(hub *Hub, id Identity, allowedOrigins []string, checker FlowAccessChecker, isRevoked func(string) bool) http.HandlerFunc {
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
		// A stopping server takes no new socket (the hub's shutdown closes the open ones): the client reconnects to
		// another replica, or to this one restarted.
		if draining.Draining() {
			platformdrain.Refuse(w)
			return
		}
		if id.Actor == nil || id.Actor.UserID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		userID := id.Actor.UserID
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

		// An access change that commits from here on and finds this socket in
		// no room yet is caught up after the join (Client.Run).
		admittedAt := hub.accessChanges.Load()
		var owner FlowOwner
		if checker != nil {
			var err error
			owner, err = checker.CheckAccess(r.Context(), flowID, id.Actor)
			switch {
			case err == nil:
				// allowed
			case errors.Is(err, ErrFlowNotFound):
				writeError(w, http.StatusNotFound, "flow not found")
				return
			case errors.Is(err, ErrAccessDenied):
				writeError(w, http.StatusForbidden, "forbidden")
				return
			default:
				logger.FromContext(r.Context()).Error("websocket: flow access check failed", "flowId", flowID, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		// No checker means a deployment without storage: a single user on
		// their own machine, who is always the editor of their own flow. Only
		// a storage-backed deployment has API tokens, but the actor's scopes
		// still decide here, so a scoped actor reaching this path fails closed.
		canEdit := id.Actor.Allows(auth.ScopeWrite)
		if checker != nil {
			canEdit = checker.CanEdit(r.Context(), flowID, id.Actor)
		}

		client := NewClient(hub, conn, id.Actor, id.DisplayName, flowID, canEdit)
		client.flowOwner, client.accessJTI, client.admittedAt = owner, id.AccessJTI, admittedAt

		// Re-authz, every 2 minutes and at once when an access change reaches
		// this socket (Hub.RecheckFlow and the rest): if the user's access is
		// revoked (token blacklist hit, flow ACL change, access-token expiry),
		// close the connection so a logged-out / revoked user stops receiving
		// real-time broadcasts. See Client.runReauthzLoop for the full mechanism.
		//
		// Deferred so a panic anywhere below still cancels the context and stops
		// the re-authz goroutine (a plain authzCancel after client.Run() leaked
		// the goroutine + context on panic).
		authzCtx, authzCancel := context.WithCancel(context.Background())
		defer authzCancel()
		if checker != nil || isRevoked != nil {
			go client.runReauthzLoop(authzCtx, checker, id.AccessJTI, id.AccessExp, isRevoked)
		}

		client.Run()
	}
}
