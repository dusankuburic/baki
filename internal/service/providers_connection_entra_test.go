package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/testutil"
)

// rewriteTransport sends every request the Azure AI Foundry runtime makes to
// one plain-HTTP fake listener (the endpoint must still be https on an
// allow-listed name, so the documents name azureTestHost). It is the
// ai.AzureFoundryOptions.Transport test seam.
type rewriteTransport struct {
	target string
	base   *http.Transport
	calls  atomic.Int32
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls.Add(1)
	out := req.Clone(req.Context())
	out.URL.Scheme = "http"
	out.URL.Host = rt.target
	out.Host = req.URL.Host
	return rt.base.RoundTrip(out)
}

// configureAzureWithFake installs the transport seam over a fake Foundry
// whose every answer is status/body, and returns the seam (for its call
// count) and the requested paths.
func configureAzureWithFake(t *testing.T, status int, body string) (*rewriteTransport, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	rt := &rewriteTransport{target: strings.TrimPrefix(srv.URL, "http://"), base: &http.Transport{}}
	t.Cleanup(rt.base.CloseIdleConnections)
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{Egress: egress.AllowLoopback, ExtraAllowedHosts: []string{azureTestHost}, Transport: rt})
	ai.ResetAzureFoundryCaches()
	t.Cleanup(func() {
		ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{})
		ai.ResetAzureFoundryCaches()
	})
	return rt, &paths
}

// An API key against a Claude Mythos deployment is refused by Foundry
// (Mythos is Entra ID only). The connection test reports the Entra-only
// explanation, not "invalid API key", and stops at that first candidate.
func TestTestProviderConnection_ClaudeMythosKeyNamesEntraOnly(t *testing.T) {
	_, paths := configureAzureWithFake(t, http.StatusUnauthorized,
		`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "azure-foundry",
		`{"endpoint":"https://`+azureTestHost+`","auth":{"mode":"api_key","key":"k1"},"claudeDeployments":["claude-mythos-5-1"]}`)
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	res, err := svc.TestProviderConnection(context.Background(), "u1", ai.AzureFoundryProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ok {
		t.Fatalf("result = %+v, want a failure", res)
	}
	if res.Error == "invalid API key" {
		t.Fatalf("error = %q: the misleading invalid-key text the Entra-only refusal replaces", res.Error)
	}
	for _, want := range []string{"claude-mythos-5-1", "Microsoft Entra ID only", "switch the Azure AI Foundry credential to Entra ID"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error = %q, want it to contain %q", res.Error, want)
		}
	}
	if len(*paths) != 1 || !strings.HasSuffix((*paths)[0], "/messages") {
		t.Errorf("fake Foundry saw %v, want the one Claude messages call (no retries, no other candidates)", *paths)
	}
}

// An entra_cli document on a machine whose Azure CLI is missing (or not
// signed in) reports the fix — install it and run `az login` — instead of a
// generic token failure. The machine's PATH is emptied so the test never
// finds (or uses) a real az; no request reaches the resource.
func TestTestProviderConnection_EntraCLIUnavailableTellsToLogIn(t *testing.T) {
	rt, _ := configureAzureWithFake(t, http.StatusOK, `{}`)
	t.Setenv("PATH", t.TempDir())
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "azure-foundry",
		`{"endpoint":"https://`+azureTestHost+`","auth":{"mode":"entra_cli"},"chatDeployment":"chat1"}`)
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	res, err := svc.TestProviderConnection(context.Background(), "u1", ai.AzureFoundryProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ok {
		t.Fatalf("result = %+v, want a failure", res)
	}
	if !strings.Contains(res.Error, "az login") || !strings.Contains(res.Error, "Azure CLI") {
		t.Errorf("error = %q, want the Azure CLI / az login instruction", res.Error)
	}
	if strings.Contains(strings.ToLower(res.Error), "invalid api key") {
		t.Errorf("error = %q: a missing CLI is not a bad key", res.Error)
	}
	if n := rt.calls.Load(); n != 0 {
		t.Errorf("the resource received %d request(s), want none (no token, no call)", n)
	}
}
