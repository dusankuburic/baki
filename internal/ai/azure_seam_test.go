package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/egress"
)

// azureSeam is an in-process Azure for the registry tests. Installed as the
// Azure client's transport (AzureFoundryOptions.Transport, tests only) with
// the .unit-test.invalid domain allow-listed (RFC 6761: it never resolves),
// it answers every request the client sends: resource and project hosts
// under that domain through handler, and Microsoft Entra ID's sign-in at
// login.microsoftonline.com through a fake authority. Every other destination
// is refused, so nothing leaves the process even if a code path bypassed the
// Azure client. No listener: each request is served through an
// httptest.ResponseRecorder, so the exported entry points — host validation,
// the throwaway or shared token sources, the guarded client — run exactly as
// in production.
type azureSeam struct {
	mu      sync.Mutex
	reqs    []azureSeamRequest
	signIns int
	handler func(w http.ResponseWriter, r *http.Request)
}

// azureSeamRequest is one request the seam served, as sent.
type azureSeamRequest struct {
	Method string
	Host   string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// JSON decodes the body as an object (nil when it is not one).
func (r azureSeamRequest) JSON() map[string]any {
	var m map[string]any
	if json.Unmarshal(r.Body, &m) != nil {
		return nil
	}
	return m
}

const (
	azureSeamSuffix    = ".unit-test.invalid"
	azureSeamAuthority = "login.microsoftonline.com"
	azureSeamClientID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// installAzureSeam routes the Azure client through a new seam for the rest
// of the test, with the process caches reset before and after.
func installAzureSeam(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *azureSeam {
	t.Helper()
	s := &azureSeam{handler: handler}
	ResetAzureFoundryCaches()
	saveAzureRuntime(t, AzureFoundryOptions{
		Egress:            egress.AllowLoopback,
		ExtraAllowedHosts: []string{"*" + azureSeamSuffix},
		Transport:         s,
	})
	t.Cleanup(ResetAzureFoundryCaches)
	return s
}

func (s *azureSeam) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	host := strings.ToLower(req.URL.Hostname())
	if req.URL.Scheme != "https" || (!strings.HasSuffix(host, azureSeamSuffix) && host != azureSeamAuthority) {
		return nil, fmt.Errorf("azure seam: refusing a request to %s", req.URL.Redacted())
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, azureSeamRequest{Method: req.Method, Host: host, Path: req.URL.Path,
		Query: req.URL.Query(), Header: req.Header.Clone(), Body: body})
	s.mu.Unlock()
	in := req.Clone(req.Context())
	in.Body = io.NopCloser(bytes.NewReader(body))
	rec := httptest.NewRecorder()
	if host == azureSeamAuthority {
		s.serveAuthority(rec, in)
	} else {
		s.handler(rec, in)
	}
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// requests returns the requests one host received (every host for ""), in
// order.
func (s *azureSeam) requests(host string) []azureSeamRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []azureSeamRequest
	for _, r := range s.reqs {
		if host == "" || r.Host == host {
			out = append(out, r)
		}
	}
	return out
}

// tokenRequests counts the client-credentials sign-ins the fake authority
// answered.
func (s *azureSeam) tokenRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signIns
}

// serveAuthority answers the sign-in azidentity (MSAL) performs against the
// public cloud's authority: instance discovery, the tenant's OpenID
// configuration, then the token endpoint, whose n-th token is
// "seam-token-<n>".
func (s *azureSeam) serveAuthority(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case strings.HasSuffix(r.URL.Path, "/discovery/instance"):
		tenant := "common"
		if ae := r.URL.Query().Get("authorization_endpoint"); ae != "" {
			if u, err := url.Parse(ae); err == nil {
				tenant = strings.Split(strings.Trim(u.Path, "/"), "/")[0]
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenant_discovery_endpoint": "https://" + azureSeamAuthority + "/" + tenant + "/v2.0/.well-known/openid-configuration",
			"api-version":               "1.1",
			"metadata": []map[string]any{{
				"preferred_network": azureSeamAuthority, "preferred_cache": "login.windows.net",
				"aliases": []string{azureSeamAuthority, "login.windows.net", "login.microsoft.com", "sts.windows.net"},
			}},
		})
	case strings.HasSuffix(r.URL.Path, "/v2.0/.well-known/openid-configuration") && len(parts) > 0:
		base := "https://" + azureSeamAuthority + "/" + parts[0]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_endpoint":                        base + "/oauth2/v2.0/token",
			"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
			"issuer":                                base + "/v2.0",
			"jwks_uri":                              base + "/discovery/v2.0/keys",
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "private_key_jwt", "client_secret_basic"},
			"response_types_supported":              []string{"code", "id_token", "code id_token", "id_token token"},
			"tenant_region_scope":                   "EU",
			"cloud_instance_name":                   "microsoftonline.com",
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		s.mu.Lock()
		s.signIns++
		n := s.signIns
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":     "Bearer",
			"expires_in":     3599,
			"ext_expires_in": 3599,
			"access_token":   "seam-token-" + strconv.Itoa(n),
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found","error_description":"AADSTS90002: the seam's authority has no such route"}`)
	}
}

// seamKeyDoc is an API-key credential document for resource host
// <name>.unit-test.invalid.
func seamKeyDoc(name string) string {
	return `{"endpoint":"https://` + name + azureSeamSuffix + `","auth":{"mode":"api_key","key":"seam-key"}}`
}

// seamEntraDoc is an Entra ID credential document for resource host
// <name>.unit-test.invalid with its project p1.
func seamEntraDoc(name string) string {
	host := "https://" + name + azureSeamSuffix
	return `{"endpoint":"` + host + `","projectEndpoint":"` + host + `/api/projects/p1","auth":{"mode":"entra","tenantId":"` +
		entraTestTenant + `","clientId":"` + azureSeamClientID + `","clientSecret":"seam-secret"}}`
}
