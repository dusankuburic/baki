package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/sso"
)

// TestSSOLink_ForgedFlowCookieCannotRetargetTheLink: the flow cookie used to be plain base64 JSON, so a browser
// could rewrite its link target to any user id, finish the round trip with its own IdP identity and have that
// identity linked to the victim's account, which it could then sign in to. The cookie is signed now: an
// unsigned, re-signed or tampered one is refused and nothing is linked.
func TestSSOLink_ForgedFlowCookieCannotRetargetTheLink(t *testing.T) {
	rt, client, ids := newSSOTestRig(t)
	login(t, rt, "victim@example.com", "StrongPass123!")
	victim, err := rt.security.Backend.LoadUserByEmail(context.Background(), "victim@example.com")
	if err != nil {
		t.Fatalf("load victim: %v", err)
	}

	// The attacker starts an ordinary login and reads the flow out of their own cookie.
	cookie, state := startSSOFlow(t, rt)
	segs := strings.Split(cookie.Value, ".")
	if len(segs) != 3 {
		t.Fatalf("flow cookie is not a signed token: %q", cookie.Value)
	}
	payload, err := base64.RawURLEncoding.DecodeString(segs[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	flow := auth.SSOFlow{State: claims["s"].(string), Nonce: claims["n"].(string), Verifier: claims["v"].(string), LinkUserID: victim.ID}
	if flow.State != state {
		t.Fatalf("cookie state %q, redirect state %q", flow.State, state)
	}

	retargeted, _ := json.Marshal(map[string]any{"s": flow.State, "n": flow.Nonce, "v": flow.Verifier, "l": victim.ID})
	claims["l"] = victim.ID
	tamperedPayload, _ := json.Marshal(claims)
	resigned, err := auth.NewManager("not-this-deployment's-secret", nil).IssueSSOFlow(flow)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"unsigned (the old format)": base64.RawURLEncoding.EncodeToString(retargeted),
		"tampered payload":          segs[0] + "." + base64.RawURLEncoding.EncodeToString(tamperedPayload) + "." + segs[2],
		"signed with another key":   resigned,
	} {
		client.identities["code-attacker"] = &sso.Identity{Subject: "attacker-sub", Email: "attacker@example.com", EmailVerified: true}
		rr := completeCallback(t, rt, &http.Cookie{Name: ssoFlowCookie, Value: value}, state, "code-attacker")
		checkStatus(t, rr, http.StatusFound)
		if loc := rr.Header().Get("Location"); !strings.Contains(loc, "ssoError=") {
			t.Errorf("%s: callback must refuse the forged flow, redirected to %q", name, loc)
		}
		if link, ok := ids.links["fake-idp\x00attacker-sub"]; ok {
			t.Fatalf("%s: the attacker's identity got linked to %q", name, link.UserID)
		}
	}
}
