package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"pad-analyzer/internal/platform/hmacsig"
	"pad-analyzer/internal/service"
)

// An organisation CI credential (baki_ci_<keyId>_<secret>) sends its key id in X-Baki-Key-Id; any other secret
// sends none. The signature does not change: the whole string is the HMAC key, as for the deployment secret.
func TestCLI_ReportSendsTheCredentialKeyID(t *testing.T) {
	const credential = "baki_ci_abcdefghijklmn23_c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3Jl"
	type seen struct{ keyID, signature, timestamp, body string }
	got := make(chan seen, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{r.Header.Get("X-Baki-Key-Id"), r.Header.Get("X-Baki-Signature"), r.Header.Get("X-Baki-Timestamp"), string(body)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"flowId":"f","passed":true,"gate":"error"}`)
	}))
	t.Cleanup(srv.Close)

	for _, c := range []struct{ secret, wantKeyID string }{
		{credential, "abcdefghijklmn23"},
		{"topsecret", ""},
	} {
		if _, stderr, code := runBakicli(t, "report", "-endpoint", srv.URL, "-secret", c.secret, "-flow-id", "f"); code != 0 {
			t.Fatalf("secret %q: exit %d (%s)", c.secret, code, stderr)
		}
		s := <-got
		if s.keyID != c.wantKeyID {
			t.Errorf("secret %q sent X-Baki-Key-Id %q, want %q", c.secret, s.keyID, c.wantKeyID)
		}
		ts, err := strconv.ParseInt(s.timestamp, 10, 64)
		if err != nil {
			t.Fatalf("timestamp %q", s.timestamp)
		}
		mac := hmac.New(sha256.New, []byte(c.secret))
		mac.Write([]byte(s.timestamp + "." + s.body))
		if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); s.signature != want || !hmacsig.Verify([]byte(c.secret), ts, []byte(s.body), s.signature) {
			t.Errorf("secret %q: signature %s, want %s over the whole secret", c.secret, s.signature, want)
		}
	}
}

func TestNewSignedCIRequest_Headers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"flowId":"f"}`)
	plain, err := newSignedCIRequest(context.Background(), "https://baki.example.com/api/integrations/ci", "topsecret", body, now)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := newSignedCIRequest(context.Background(), "https://baki.example.com/api/integrations/ci", "baki_ci_abcdefghijklmnop_xyz", body, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.Header["X-Baki-Key-Id"]; ok {
		t.Error("a plain secret sends X-Baki-Key-Id")
	}
	if cred.Header.Get("X-Baki-Key-Id") != "abcdefghijklmnop" {
		t.Errorf("X-Baki-Key-Id = %q", cred.Header.Get("X-Baki-Key-Id"))
	}
	if cred.Header.Get("X-Baki-Signature") != hmacsig.Sign([]byte("baki_ci_abcdefghijklmnop_xyz"), now.Unix(), body) ||
		plain.Header.Get("X-Baki-Signature") != hmacsig.Sign([]byte("topsecret"), now.Unix(), body) {
		t.Error("the signature is not the canonical one over the whole secret")
	}
}

// bakicli's own reading of a credential agrees with the server's on every well-formed credential, and finds none in
// a plain secret.
func TestCIKeyID_AgreesWithTheServer(t *testing.T) {
	for _, s := range []string{
		"baki_ci_abcdefghijklmnop_c2VjcmV0",
		"baki_ci_abcdefghijklmn23_with_underscores-and-dashes",
		"topsecret",
		"baki_ci_",
		"xbaki_ci_abcdefghijklmnop_x",
	} {
		if got, want := ciKeyID(s), service.CIKeyID(s); got != want {
			t.Errorf("ciKeyID(%q) = %q, the server reads %q", s, got, want)
		}
	}
}
