package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/drain"
)

// While the server drains (ADR 0024), /ws is refused with 503 SERVER_RESTARTING before its single-use ticket is
// redeemed: the refused upgrade leaves the ticket unspent, so the client's reconnect can still present it (to another
// replica through the shared blacklist, within the ticket's life).
func TestWS_DrainingRefusesBeforeRedeemingTheTicket(t *testing.T) {
	rt := newJWTTestRouter(t)
	ticket, _, err := rt.security.AuthMgr.IssueWSTicket(auth.WSTicketSource{UserID: "user-1", Email: "alice@example.com", Role: auth.RoleAdmin})
	if err != nil {
		t.Fatalf("IssueWSTicket: %v", err)
	}
	upgrade := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ws?flowId=f1&ticket="+ticket, nil))
		return rr
	}

	st := drain.New()
	rt.draining = st
	st.Begin()
	checkServerRestarting(t, upgrade())

	// The server the reconnect reaches is not draining: the ticket still passes authentication (the upgrade itself
	// then fails, httptest cannot hijack; TestWS_TicketIsSingleUse).
	rt.draining = drain.New()
	if rr := upgrade(); rr.Code == http.StatusUnauthorized {
		t.Fatalf("the refused upgrade spent its ticket: the next one answered 401 %s", rr.Body.String())
	}
}
