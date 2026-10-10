package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The webhook's gate is one of error, warning, info and none: anything else gates as error and is answered and
// audited as "error", so a caller's text (a NUL Postgres refuses in the audit row's JSONB, a megabyte) never reaches
// the response or the audit log.
func TestCIWebhook_GateIsCanonical(t *testing.T) {
	f := newCIOrgFixture(t)
	setCIGlobalSecret(t, f.rt, "topsecret")
	for failOn, want := range map[string]string{
		"":          "error",
		" Warning ": "warning",
		"none":      "none",
		"error\x00": "error",
		"bogus":     "error",
	} {
		rr := serveCI(f.rt, ciCall(t, "topsecret", "", map[string]string{"flowId": f.orgFlow, "failOn": failOn}))
		checkStatus(t, rr, http.StatusOK)
		var body struct {
			Gate string `json:"gate"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Gate != want {
			t.Errorf("failOn %q: gate %q, want %q", failOn, body.Gate, want)
		}
	}
}
