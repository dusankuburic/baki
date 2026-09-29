package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"golang.org/x/sync/singleflight"

	"pad-analyzer/internal/egress"
	"pad-core/logger"
)

// azureFoundryScope is the Entra ID token audience for every Foundry data
// plane (model inference, agents, project APIs).
const azureFoundryScope = "https://ai.azure.com/.default"

// errEntraToken marks a failure to OBTAIN an Entra ID token (as opposed to
// the service rejecting one): the model listing skips the project extras
// after it instead of asking for the token again per project request, and
// withTokenRetry does not invalidate and re-fetch after it.
var errEntraToken = errors.New("azure ai foundry: entra id token")

// ErrEntraCredentialsRejected marks a Microsoft Entra ID token request the
// identity platform refused — HTTP 400/401, or one of the AADSTS codes that
// name a fixable cause (aadExplanations): unknown tenant, wrong client ID,
// invalid or expired client secret, a disabled application, Conditional
// Access. The error's message names the cause. The error also matches
// ErrApiKeyInvalid, so every auth-failure rule applies (no retry, no
// circuit-breaker count, PROVIDER_AUTH); the chat copy and the connection
// test check this first to name the service principal instead of an API key.
var ErrEntraCredentialsRejected = errors.New("microsoft entra id rejected the service principal credentials")

// ErrEntraTokenUnavailable marks a token request that failed for a reason
// other than refused credentials — the identity service unreachable or
// answering 5xx or OAuth "temporarily_unavailable" (whatever the HTTP
// status), the fetch timed out: worth trying again later, unlike a
// refusal. Deliberately not ErrProviderDown:
// azidentity already retried the token endpoint, the retry wrapper would
// re-run 30 s fetches (and mask the cause behind the connection test's
// deadline), and the resource's circuit breaker must not open for the
// identity service's outage. The chat copy names it.
var ErrEntraTokenUnavailable = errors.New("microsoft entra id token unavailable")

// entraTokenFetchTimeout bounds one detached token fetch (azidentity retries
// transient token-endpoint failures within it).
const entraTokenFetchTimeout = 30 * time.Second

// entraTokenRefreshAhead is the remaining validity below which a token is
// refreshed; entraTokenMinRemaining the validity below which a caller WAITS
// for that refresh instead of using the cached token. Between the two the
// refresh runs in the background: the cached token is still good, so the
// refresh costs no caller a round trip, and a refresh that fails (an identity
// service blip) fails nobody while the cached token still works.
const (
	entraTokenRefreshAhead = 5 * time.Minute
	entraTokenMinRemaining = time.Minute
)

// entraRebuildCooldown bounds how often a refused-but-unexpired token makes
// the (process-wide, cross-user) source sign in again (see Invalidate).
const entraRebuildCooldown = time.Minute

// entraSourceIdleTTL reclaims a source nobody has built a provider from for a
// day (a deleted or rotated document); the LRU cap alone never fires on a
// small deployment, so the credential and its cached token stayed in memory
// for the life of the process.
const entraSourceIdleTTL = 24 * time.Hour

// entraTokenError is a failed token request reduced to one line. azidentity's
// own text for a refused request is a 15-line block (the token endpoint, the
// indented JSON response with trace and correlation ids, a troubleshooting
// link) that used to become the chat bubble and the connection-test result
// verbatim; it is logged for the operator instead (classifyEntraTokenError)
// and stays reachable through Unwrap.
type entraTokenError struct {
	msg      string
	rejected bool // 400/401: the credentials themselves were refused
	cause    error
}

func (e *entraTokenError) Error() string { return e.msg }

// EntraRejectionReason returns the one-line explanation of a refused
// Microsoft Entra ID sign-in anywhere in err's chain ("Microsoft Entra ID
// refused the sign-in: the client secret has expired — … (AADSTS7000222
// invalid_client)"), for chat copy that should name the fix. It never
// contains the secret. false when err is not a credential refusal.
func EntraRejectionReason(err error) (string, bool) {
	var e *entraTokenError
	if !errors.As(err, &e) || !e.rejected || e.msg == "" {
		return "", false
	}
	return e.msg, true
}

func (e *entraTokenError) Unwrap() []error {
	errs := []error{errEntraToken}
	if e.rejected {
		errs = append(errs, ErrEntraCredentialsRejected, ErrApiKeyInvalid)
	} else {
		errs = append(errs, ErrEntraTokenUnavailable)
	}
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	return errs
}

// classifyEntraTokenError maps a GetToken failure to an entraTokenError, one
// line without the response dump (the full azidentity text goes to the
// server log):
//   - OAuth "temporarily_unavailable" is ErrEntraTokenUnavailable whatever
//     the HTTP status: the identity service asks to be asked again;
//   - an AADSTS code with a known cause (aadExplanations) is
//     ErrEntraCredentialsRejected (and ErrApiKeyInvalid) whose message says
//     what to fix — an expired secret, the secret ID pasted for its value, an
//     unknown tenant or application, Conditional Access;
//   - any other 400/401 is ErrEntraCredentialsRejected with the AADSTS code;
//   - anything else is ErrEntraTokenUnavailable — a connection failure in
//     fixed copy (egress.DescribeFailure: the text of a failed dial names the
//     proxy's or the resolved address, which must not reach a client).
//
// The messages carry the AADSTS code and OAuth error, never the secret.
func classifyEntraTokenError(err error) error {
	logger.Warn("azure ai foundry: entra id token request failed", "error", err)
	var afe *azidentity.AuthenticationFailedError
	if errors.As(err, &afe) && afe.RawResponse != nil {
		status := afe.RawResponse.StatusCode
		body := readAADRefusal(afe.RawResponse)
		detail := aadErrorDetail(status, body)
		if body.Error == aadTemporarilyUnavailable {
			return &entraTokenError{cause: err,
				msg: "Microsoft Entra ID is temporarily unavailable — try again shortly (" + detail + ")"}
		}
		if why := aadExplanation(body.ErrorCodes); why != "" {
			return &entraTokenError{rejected: true, cause: err,
				msg: "Microsoft Entra ID refused the sign-in: " + why + " (" + detail + ")"}
		}
		if status == http.StatusBadRequest || status == http.StatusUnauthorized {
			return &entraTokenError{rejected: true, cause: err,
				msg: "Microsoft Entra ID rejected the tenant ID, client ID or client secret (" + detail + ")"}
		}
		return &entraTokenError{cause: err, msg: "the Microsoft Entra ID token request failed (" + detail + ")"}
	}
	if what, ok := egress.DescribeFailure(err); ok {
		return &entraTokenError{cause: err, msg: "could not reach Microsoft Entra ID for a token: " + what}
	}
	return &entraTokenError{cause: err, msg: "the Microsoft Entra ID token request failed: " + oneLine(err.Error(), 300)}
}

// aadTemporarilyUnavailable is the OAuth error of a token endpoint that asks
// to be retried later (RFC 6749 section 4.1.2.1).
const aadTemporarilyUnavailable = "temporarily_unavailable"

// aadExplanations are the AADSTS codes whose cause the user can fix, and
// what to do about each. Every one of them is a refusal (not retryable).
var aadExplanations = map[int]string{
	7000222: "the client secret has expired — create a new secret under Certificates & secrets in the app registration and save it here",
	7000215: "the client secret is not valid — paste the secret's Value, not its Secret ID",
	700016:  "the application (client) ID was not found in the tenant — check the client ID, and that the app is registered in or consented to by this tenant",
	90002:   "the tenant was not found — check the tenant ID (the directory ID or a domain such as contoso.onmicrosoft.com)",
	900023:  "the tenant ID is not a valid tenant — use the directory ID or a domain such as contoso.onmicrosoft.com",
	500011:  "the Azure AI Foundry resource principal (https://ai.azure.com) was not found in the tenant — check that the tenant ID is the resource's own tenant and that the resource is in the Azure public cloud (the only cloud supported)",
	53003:   "a Conditional Access policy blocked the sign-in — a tenant administrator must allow this service principal",
	7000112: "the application is disabled in the tenant — a tenant administrator must enable it",
}

// aadExplanation is the explanation of the first known AADSTS code, "" when
// none is known.
func aadExplanation(codes []int) string {
	for _, c := range codes {
		if why, ok := aadExplanations[c]; ok {
			return why
		}
	}
	return ""
}

// aadRefusal is the OAuth error body of a token-endpoint answer.
type aadRefusal struct {
	Error      string `json:"error"`
	ErrorCodes []int  `json:"error_codes"`
}

// readAADRefusal parses a token-endpoint answer's OAuth error body; the zero
// value when there is none.
func readAADRefusal(resp *http.Response) aadRefusal {
	var body aadRefusal
	raw, err := azruntime.Payload(resp) // re-readable: the logged error keeps its body
	if err != nil || json.Unmarshal(raw, &body) != nil {
		return aadRefusal{}
	}
	body.Error = oneLine(body.Error, 64)
	return body
}

// aadErrorDetail names a token-endpoint refusal by its first AADSTS code and
// OAuth error ("AADSTS7000215 invalid_client"), else by its HTTP status.
func aadErrorDetail(status int, body aadRefusal) string {
	httpStatus := fmt.Sprintf("HTTP %d", status)
	switch {
	case len(body.ErrorCodes) > 0 && body.Error != "":
		return fmt.Sprintf("AADSTS%d %s", body.ErrorCodes[0], body.Error)
	case len(body.ErrorCodes) > 0:
		return fmt.Sprintf("AADSTS%d", body.ErrorCodes[0])
	case body.Error != "":
		return httpStatus + " " + body.Error
	}
	return httpStatus
}

// oneLine reduces s to its first line, trimmed and cut on a rune boundary to
// at most maxLen bytes (plus an ellipsis when cut).
func oneLine(s string, maxLen int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// entraTokenSource caches one service principal's access token and refreshes
// it shortly before expiry. It mirrors the Postgres managed-identity
// connector (storage/database/azure.go): the cache is read under a mutex, the
// network fetch runs outside it under singleflight, so a refresh never
// serialises concurrent chat turns and a burst of turns at a token boundary
// costs one token request, not one per turn.
type entraTokenSource struct {
	// newCred rebuilds the credential — and with it azidentity's per-credential
	// in-memory MSAL cache — when the service refuses a token MSAL still
	// considers valid (see Invalidate). nil (tests) keeps the credential.
	newCred func() (azcore.TokenCredential, error)

	mu        sync.Mutex
	cred      azcore.TokenCredential
	token     string
	expires   time.Time
	rebuiltAt time.Time
	sf        singleflight.Group
}

func newEntraTokenSource(cred azcore.TokenCredential) *entraTokenSource {
	return &entraTokenSource{cred: cred}
}

// Token returns a valid access token, fetching or refreshing when the cached
// one is missing or within entraTokenRefreshAhead of expiry. A token with more
// than entraTokenMinRemaining left is returned at once and refreshed in the
// background; below that the caller waits for the refresh.
//
// The fetch is shared with every caller that arrives while it runs — sources
// are process-wide per service principal, so across users — and therefore
// never runs under one caller's ctx: the first caller's stop or short
// deadline (the RAG lookup's 800 ms) used to fail every goroutine that joined
// it. It runs detached from the caller's cancellation and bounded by
// entraTokenFetchTimeout; each caller waits under its own ctx, and a caller
// that gives up leaves the fetch to finish and fill the cache for the next.
func (s *entraTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	tok, exp := s.token, s.expires
	s.mu.Unlock()
	left := time.Until(exp)
	if tok != "" && left >= entraTokenRefreshAhead {
		return tok, nil
	}
	ch := s.sf.DoChan("token", func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), entraTokenFetchTimeout)
		defer cancel()
		// The credential is read under the mutex: Invalidate may swap it.
		s.mu.Lock()
		cred := s.cred
		s.mu.Unlock()
		at, err := cred.GetToken(fctx, policy.TokenRequestOptions{Scopes: []string{azureFoundryScope}})
		if err != nil {
			return "", classifyEntraTokenError(err)
		}
		s.mu.Lock()
		s.token = at.Token
		s.expires = at.ExpiresOn
		s.mu.Unlock()
		logger.Debug("azure ai foundry: refreshed entra id token", "expires", at.ExpiresOn)
		return at.Token, nil
	})
	if tok != "" && left >= entraTokenMinRemaining {
		// Refresh ahead in the background: the cached token is still good, so
		// no caller pays the round trip, and a failed refresh (logged by the
		// fetch) fails nobody while the cached token works. DoChan's channel
		// is buffered and the fetch detached, so nothing blocks on the result.
		return tok, nil
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Invalidate drops the cached token if it is still the one that just failed
// (401 from the service), so the next Token call fetches a fresh one. A newer
// token another goroutine already refreshed is left alone.
//
// Clearing the cache alone is not enough: azidentity serves MSAL's cached
// access token until 5 minutes before expiry whatever this cache says, so the
// next GetToken handed back the very token the service refused and the
// "retry with a fresh token" never fired. The credential is rebuilt instead (a
// fresh MSAL cache, no network) so the next Token() signs in again — at most
// once per entraRebuildCooldown, so a resource that refuses every token cannot
// turn each user's turn into a sign-in.
func (s *entraTokenSource) Invalidate(failed string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != failed {
		return
	}
	s.token = ""
	s.expires = time.Time{}
	if s.newCred == nil || time.Since(s.rebuiltAt) < entraRebuildCooldown {
		return
	}
	if c, err := s.newCred(); err == nil {
		s.cred = c
		s.rebuiltAt = time.Now()
	}
}

// entraSources keeps one token source per service principal, process-wide.
// The factory builds a fresh provider per request; without this registry every
// chat turn would create a new credential and pay a client-credentials round
// trip to login.microsoftonline.com. Keyed by a hash of the credential so a
// rotated secret gets its own source (and never reuses a token minted for the
// old one).
type entraSourceEntry struct {
	src *entraTokenSource
	// lastUsed is the registry's eviction clock, touched on every hit — the
	// token source itself carries no timestamp to prune by.
	lastUsed time.Time
}

var entraSources = struct {
	mu sync.Mutex
	m  map[string]entraSourceEntry
}{m: map[string]entraSourceEntry{}}

// entraSourcesMax bounds the registry; a deployment realistically holds a
// handful of service principals.
const entraSourcesMax = 256

// entraCredentialOptions wires the Entra ID credential to the package's
// runtime client (azureHTTPClient): azidentity's default pipeline would use
// azcore's own transport, which bypasses the egress guard and follows
// redirects.
//
// The authority is pinned to the Azure public cloud
// (login.microsoftonline.com): with no cloud set, azidentity takes a
// process-wide AZURE_AUTHORITY_HOST from the environment — set for some
// other Azure SDK client, it would silently send every user's client secret
// to that authority. Azure AI Foundry support is public-cloud only (the
// token scope and the host allow-list are the public cloud's), so there is
// nothing to switch to. The authority is a fixed, Microsoft-owned public
// host, so the cloud-mode dial-time guard passes it without any allow-list
// entry, and desktop/local mode's loopback allowance keeps local fakes and
// proxies working.
func entraCredentialOptions() *azidentity.ClientSecretCredentialOptions {
	opts := &azidentity.ClientSecretCredentialOptions{}
	opts.Cloud = cloud.AzurePublic
	opts.Transport = azureHTTPClient()
	return opts
}

// entraSourceKey is the registry key of one service principal: a hash, so the
// map holds no secret in its keys.
func entraSourceKey(tenantID, clientID, clientSecret string) string {
	h := sha256.Sum256([]byte(tenantID + "\x00" + clientID + "\x00" + clientSecret))
	return hex.EncodeToString(h[:])
}

// pruneEntraSourcesLocked drops the sources idle for longer than
// entraSourceIdleTTL (caller holds the mutex): one O(n ≤ entraSourcesMax)
// pass per provider construction.
func pruneEntraSourcesLocked(now time.Time) {
	for k, e := range entraSources.m {
		if now.Sub(e.lastUsed) > entraSourceIdleTTL {
			delete(entraSources.m, k)
		}
	}
}

// forgetEntraSource drops one principal's registry entry so a removed or
// replaced configuration's credential object and cached token do not outlive
// it. A source a running request still holds keeps working; the next
// entraSourceFor for the same tuple builds a fresh one (one extra sign-in).
func forgetEntraSource(tenantID, clientID, clientSecret string) {
	entraSources.mu.Lock()
	delete(entraSources.m, entraSourceKey(tenantID, clientID, clientSecret))
	entraSources.mu.Unlock()
}

func entraSourceFor(tenantID, clientID, clientSecret string) (*entraTokenSource, error) {
	key := entraSourceKey(tenantID, clientID, clientSecret)

	entraSources.mu.Lock()
	defer entraSources.mu.Unlock()
	now := time.Now()
	pruneEntraSourcesLocked(now)
	if e, ok := entraSources.m[key]; ok {
		e.lastUsed = now
		entraSources.m[key] = e
		return e.src, nil
	}
	// The credential must fetch through the same client as every other Azure
	// hop: azidentity's default (nil options) pipeline uses azcore's own
	// transport — no egress guard, redirects followed. *http.Client satisfies
	// azcore's policy.Transporter, and azureHTTPClient() is the guarded,
	// no-redirects client in cloud mode. The authority host is pinned to the
	// public cloud's login.microsoftonline.com (entraCredentialOptions), a
	// fixed, Microsoft-owned host that resolves publicly, so the
	// BlockInternal dial-time guard passes it unchanged; in desktop/local mode nothing is guarded and loopback is
	// allowed, so nothing regresses there either. The constructor is kept so
	// Invalidate can rebuild the credential (a fresh MSAL cache).
	newCred := func() (azcore.TokenCredential, error) {
		return azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, entraCredentialOptions())
	}
	cred, err := newCred()
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry: entra id credential: %w", err)
	}
	if len(entraSources.m) >= entraSourcesMax {
		// Rare (hundreds of distinct principals). Make room for this entry by
		// dropping the least recently used one, not by wiping the registry:
		// the wipe also discarded the sources the active chat turns were
		// about to hit, forcing every one of them through a fresh
		// client-credentials sign-in. Dropped sources just re-fetch on next
		// use. One O(n) scan per insert while full, n ≤ entraSourcesMax.
		oldest, oldestUsed, first := "", time.Time{}, true
		for k, e := range entraSources.m {
			if first || e.lastUsed.Before(oldestUsed) {
				oldest, oldestUsed, first = k, e.lastUsed, false
			}
		}
		delete(entraSources.m, oldest)
	}
	s := newEntraTokenSource(cred)
	s.newCred = newCred
	entraSources.m[key] = entraSourceEntry{src: s, lastUsed: now}
	return s, nil
}

// resetEntraSources clears the registry (tests).
func resetEntraSources() {
	entraSources.mu.Lock()
	defer entraSources.mu.Unlock()
	entraSources.m = map[string]entraSourceEntry{}
}
