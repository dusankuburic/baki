package ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"pad-analyzer/internal/egress"
)

// aadFailure is a token-endpoint answer as azidentity reports it.
func aadFailure(status int, body string) error {
	return &azidentity.AuthenticationFailedError{RawResponse: &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}}
}

// The token endpoint's AADSTS codes name the fix in the message, one line,
// while the sentinels — and so retry, breaker and chat classification — stay
// those of a refusal; "temporarily_unavailable" is an outage whatever its
// HTTP status.
func TestClassifyEntraTokenError_AADSTSCodes(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		rejected bool
		want     []string
	}{
		{"expired secret", 401, `{"error":"invalid_client","error_codes":[7000222]}`, true,
			[]string{"client secret has expired", "AADSTS7000222 invalid_client"}},
		{"secret id pasted", 401, `{"error":"invalid_client","error_codes":[7000215]}`, true,
			[]string{"Value, not its Secret ID", "AADSTS7000215"}},
		{"unknown application", 400, `{"error":"unauthorized_client","error_codes":[700016]}`, true,
			[]string{"application (client) ID was not found", "AADSTS700016"}},
		{"unknown tenant", 400, `{"error":"invalid_request","error_codes":[90002]}`, true,
			[]string{"tenant was not found", "AADSTS90002"}},
		{"resource principal missing", 400, `{"error":"invalid_resource","error_codes":[500011]}`, true,
			[]string{"resource principal", "public cloud", "AADSTS500011"}},
		{"conditional access", 400, `{"error":"access_denied","error_codes":[53003]}`, true,
			[]string{"Conditional Access", "AADSTS53003"}},
		{"application disabled", 401, `{"error":"unauthorized_client","error_codes":[7000112]}`, true,
			[]string{"application is disabled", "AADSTS7000112"}},
		{"known code, odd status", 403, `{"error":"invalid_client","error_codes":[7000222]}`, true,
			[]string{"client secret has expired"}},
		{"unknown refusal", 401, `{"error":"invalid_client","error_codes":[7000999]}`, true,
			[]string{"rejected the tenant ID, client ID or client secret", "AADSTS7000999 invalid_client"}},
		{"temporarily unavailable as 400", 400, `{"error":"temporarily_unavailable","error_codes":[50001]}`, false,
			[]string{"temporarily unavailable", "AADSTS50001 temporarily_unavailable"}},
		{"temporarily unavailable as 503", 503, `{"error":"temporarily_unavailable"}`, false,
			[]string{"temporarily unavailable", "HTTP 503 temporarily_unavailable"}},
		{"server error", 500, `not json`, false, []string{"token request failed (HTTP 500)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := classifyEntraTokenError(aadFailure(c.status, c.body))
			if !errors.Is(err, errEntraToken) {
				t.Fatalf("want an entra token error, got %v", err)
			}
			if got := errors.Is(err, ErrEntraCredentialsRejected); got != c.rejected {
				t.Errorf("rejected = %v, want %v (%v)", got, c.rejected, err)
			}
			if got := errors.Is(err, ErrApiKeyInvalid); got != c.rejected {
				t.Errorf("ErrApiKeyInvalid = %v, want %v", got, c.rejected)
			}
			if got := errors.Is(err, ErrEntraTokenUnavailable); got == c.rejected {
				t.Errorf("ErrEntraTokenUnavailable = %v, want %v", got, !c.rejected)
			}
			if isRetryable(err) || errors.Is(err, ErrProviderDown) {
				t.Errorf("a token-endpoint answer is never retried by the wrapper: %v", err)
			}
			msg := err.Error()
			if strings.ContainsAny(msg, "\r\n") {
				t.Errorf("message must be one line: %q", msg)
			}
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
		})
	}
}

// A token request that never got an answer — the proxy refused, the name
// did not resolve — is reported in fixed copy: azidentity flattens the dial
// error into text that names the proxy's address.
func TestClassifyEntraTokenError_TransportFailureHidesAddresses(t *testing.T) {
	flattened := errors.New(`ClientSecretCredential: Post "https://login.microsoftonline.com/t/oauth2/v2.0/token": proxyconnect tcp: dial tcp 10.9.8.7:3128: connect: connection refused`)
	err := classifyEntraTokenError(flattened)
	if !errors.Is(err, ErrEntraTokenUnavailable) || errors.Is(err, ErrEntraCredentialsRejected) {
		t.Fatalf("want ErrEntraTokenUnavailable, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "outbound proxy") || strings.Contains(msg, "10.9.8.7") || strings.Contains(msg, "3128") {
		t.Errorf("want fixed proxy copy without the address, got %q", msg)
	}
	if !errors.Is(err, flattened) {
		t.Error("the original error must stay reachable for the log")
	}
}

// recordingTransport answers nothing: it records each request's host and
// fails it, so a credential's authority can be observed without a network.
type recordingTransport struct {
	mu    sync.Mutex
	hosts []string
}

func (r *recordingTransport) Do(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, req.URL.Host)
	r.mu.Unlock()
	return nil, errors.New("recording transport: no network in unit tests")
}

var _ policy.Transporter = (*recordingTransport)(nil)

// The credential's authority is pinned to the Azure public cloud: a
// process-wide AZURE_AUTHORITY_HOST (set for some other Azure SDK client)
// must not redirect every user's client secret to another authority.
func TestEntraCredentialOptions_PinnedToPublicCloud(t *testing.T) {
	saveAzureRuntime(t, AzureFoundryOptions{Egress: egress.AllowLoopback})
	t.Setenv("AZURE_AUTHORITY_HOST", "https://login.attacker.example/")
	opts := entraCredentialOptions()
	if opts.Cloud.ActiveDirectoryAuthorityHost != cloud.AzurePublic.ActiveDirectoryAuthorityHost {
		t.Fatalf("authority = %q, want the public cloud's %q", opts.Cloud.ActiveDirectoryAuthorityHost, cloud.AzurePublic.ActiveDirectoryAuthorityHost)
	}
	rec := &recordingTransport{}
	opts.Transport = rec
	opts.Retry.MaxRetries = -1 // one attempt: the transport never answers
	cred, err := azidentity.NewClientSecretCredential(entraTestTenant, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "s", opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{azureFoundryScope}}); err == nil {
		t.Fatal("the recording transport answers nothing; GetToken must fail")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.hosts) == 0 {
		t.Fatal("the credential sent no request")
	}
	for _, h := range rec.hosts {
		if h != "login.microsoftonline.com" {
			t.Errorf("the credential contacted %q, want only login.microsoftonline.com", h)
		}
	}
}
