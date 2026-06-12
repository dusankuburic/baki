package websocket

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/gorilla/websocket"
)

var flowIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type FlowAccessChecker interface {
	FlowExists(ctx context.Context, flowID string) (bool, error)
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
func Handler(hub *Hub, userID, displayName string, allowedOrigins []string, checker FlowAccessChecker) http.HandlerFunc {
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
			exists, err := checker.FlowExists(r.Context(), flowID)
			if err != nil {
				slog.Error("websocket: flow access check failed", "flowId", flowID, "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			if !exists {
				http.Error(w, "flow not found", http.StatusNotFound)
				return
			}
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// upgrader has already written an error response
			return
		}

		client := NewClient(hub, conn, userID, displayName, flowID)
		// Run blocks until the connection is closed.
		client.Run()
	}
}
