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

// errEntraToken marks a failure to OBTAIN an Entra ID token (as opposed to the service rejecting one): the
// model listing skips the project extras after it, and withTokenRetry does not invalidate and re-fetch after it.
var errEntraToken = errors.New("azure ai foundry: entra id token")

// ErrEntraCredentialsRejected marks an Entra ID token request the identity platform refused — HTTP 400/401,
// or an AADSTS code naming a fixable cause (aadExplanations). Also matches ErrApiKeyInvalid, so every
// auth-failure rule applies (no retry, no breaker count, PROVIDER_AUTH).
var ErrEntraCredentialsRejected = errors.New("microsoft entra id rejected the service principal credentials")

// ErrEntraTokenUnavailable marks a token request that failed for a reason other than refused credentials
// (identity service unreachable/5xx/"temporarily_unavailable", or a timeout). Deliberately not ErrProviderDown:
// azidentity already retried the token endpoint, and the resource's breaker must not open for the identity service.
var ErrEntraTokenUnavailable = errors.New("microsoft entra id token unavailable")

// entraTokenFetchTimeout bounds one detached token fetch (azidentity retries transient token-endpoint failures within it).
const entraTokenFetchTimeout = 30 * time.Second

// entraTokenRefreshAhead is the validity below which a token is refreshed; entraTokenMinRemaining the validity
// below which a caller WAITS for the refresh — between the two the refresh runs in the background and a failed
// refresh (an identity service blip) fails nobody.
const (
	entraTokenRefreshAhead = 5 * time.Minute
	entraTokenMinRemaining = time.Minute
)

// entraRebuildCooldown bounds how often a refused-but-unexpired token makes the (process-wide, cross-user)
// source sign in again (Invalidate), and how often the background refresh retries after a failure — without
// it, every turn in the refresh-ahead window would start its own failed sign-in (AADSTS50196 loop detection).
const entraRebuildCooldown = time.Minute

// entraSourceIdleTTL reclaims a source nobody has built a provider from for a day (a deleted or rotated
// document); the LRU cap alone never fires on a small deployment.
const entraSourceIdleTTL = 24 * time.Hour

// entraTokenError is a failed token request reduced to one line (azidentity's refusal text is a 15-line
// block that used to become the chat bubble verbatim); the full text is logged and reachable through Unwrap.
type entraTokenError struct {
	msg      string
	rejected bool // 400/401: the credentials themselves were refused
	cause    error
}

func (e *entraTokenError) Error() string { return e.msg }

// EntraRejectionReason returns the one-line explanation of a refused Entra ID sign-in anywhere in err's chain
// ("…the client secret has expired — (AADSTS7000222 invalid_client)"), never containing the secret; false
// when err is not a credential refusal.
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

// classifyEntraTokenError maps a GetToken failure to a one-line entraTokenError (the caller logs the full
// azidentity text): OAuth "temporarily_unavailable" is ErrEntraTokenUnavailable whatever the status; an
// AADSTS code with a known cause is ErrEntraCredentialsRejected (and ErrApiKeyInvalid) naming the fix; any
// other 400/401 is ErrEntraCredentialsRejected with the code; anything else is ErrEntraTokenUnavailable. The
// messages carry the AADSTS code and OAuth error, never the secret.
func classifyEntraTokenError(err error) error {
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
	// The Azure CLI credential's "not signed in / not installed": azidentity marks it with the credential's
	// name (an unexported error type, matched by prefix). The fix is on the user's machine, not in the document.
	if strings.HasPrefix(oneLine(err.Error(), 64), "AzureCLICredential") {
		return &entraTokenError{cause: err,
			msg: "the Azure CLI is not available or not signed in — install it, run `az login` (and `az account set --tenant <tenant>` for a specific tenant), then try again"}
	}
	if what, ok := egress.DescribeFailure(err); ok {
		return &entraTokenError{cause: err, msg: "could not reach Microsoft Entra ID for a token: " + what}
	}
	return &entraTokenError{cause: err, msg: "the Microsoft Entra ID token request failed: " + oneLine(err.Error(), 300)}
}

// aadTemporarilyUnavailable is the OAuth error of a token endpoint that asks to be retried later (RFC 6749 §4.1.2.1).
const aadTemporarilyUnavailable = "temporarily_unavailable"

// aadExplanations are the AADSTS codes whose cause the user can fix, and what to do about each. Every one is a refusal (not retryable).
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

// aadExplanation is the explanation of the first known AADSTS code, "" when none is known.
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

// readAADRefusal parses a token-endpoint answer's OAuth error body; the zero value when there is none.
func readAADRefusal(resp *http.Response) aadRefusal {
	var body aadRefusal
	raw, err := azruntime.Payload(resp) // re-readable: the logged error keeps its body
	if err != nil || json.Unmarshal(raw, &body) != nil {
		return aadRefusal{}
	}
	body.Error = oneLine(body.Error, 64)
	return body
}

// aadErrorDetail names a token-endpoint refusal by its first AADSTS code and OAuth error ("AADSTS7000215 invalid_client"), else by its HTTP status.
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

// oneLine reduces s to its first line, trimmed and cut on a rune boundary to at most maxLen bytes.
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

// entraTokenSource caches one service principal's access token and refreshes it before expiry: the cache is
// read under a mutex, the fetch runs outside it under singleflight, so a burst at a token boundary costs one
// token request, not one per turn.
type entraTokenSource struct {
	// newCred rebuilds the credential — and with it azidentity's per-credential in-memory MSAL cache — when
	// the service refuses a token MSAL still considers valid (see Invalidate). nil (tests) keeps the credential.
	newCred func() (azcore.TokenCredential, error)

	mu        sync.Mutex
	cred      azcore.TokenCredential
	token     string
	expires   time.Time
	rebuiltAt time.Time
	// refreshFailedAt is when the last background refresh failed; while the cached token still has more than
	// entraTokenMinRemaining left, a fresh refresh is not started before entraRebuildCooldown has passed.
	refreshFailedAt time.Time
	sf              singleflight.Group
}

func newEntraTokenSource(cred azcore.TokenCredential) *entraTokenSource {
	return &entraTokenSource{cred: cred}
}

// Token returns a valid access token, fetching or refreshing when the cached one is missing or within
// entraTokenRefreshAhead of expiry; a token with more than entraTokenMinRemaining left returns at once and
// refreshes in the background. The fetch is shared with every caller that arrives while it runs (sources are
// process-wide per principal) and runs detached and bounded: a caller that gives up leaves it to fill the cache.
func (s *entraTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	tok, exp, failedAt := s.token, s.expires, s.refreshFailedAt
	s.mu.Unlock()
	left := time.Until(exp)
	if tok != "" && left >= entraTokenRefreshAhead {
		return tok, nil
	}
	// A refresh that just failed is not retried by every caller in the refresh-ahead window — an outage or an
	// expired secret would otherwise cost one failed sign-in per chat turn across every user of the principal.
	if tok != "" && left >= entraTokenMinRemaining && time.Since(failedAt) < entraRebuildCooldown {
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
			s.mu.Lock()
			s.refreshFailedAt = time.Now()
			s.mu.Unlock()
			recordEntraTokenEvent(entraTokenRefreshFailed)
			// Logged here rather than in the classifier: fctx keeps the attributes of the request that started the
			// fetch, so the full azidentity text can be joined with that request's lines.
			logger.FromContext(fctx).Warn("azure ai foundry: entra id token request failed", "error", err)
			return "", classifyEntraTokenError(err)
		}
		s.mu.Lock()
		s.token = at.Token
		s.expires = at.ExpiresOn
		s.refreshFailedAt = time.Time{}
		s.mu.Unlock()
		recordEntraTokenEvent(entraTokenRefreshed)
		logger.FromContext(fctx).Debug("azure ai foundry: refreshed entra id token", "expires", at.ExpiresOn)
		return at.Token, nil
	})
	if tok != "" && left >= entraTokenMinRemaining {
		// Refresh ahead in the background: the cached token is still good, so no caller pays the round trip
		// and a failed refresh fails nobody.
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

// Invalidate drops the cached token if it is still the one that just failed, so the next Token call fetches
// fresh; a newer token another goroutine already refreshed is left alone. Clearing the cache alone is not
// enough — azidentity serves MSAL's cached token until 5 minutes before expiry — so the credential is rebuilt
// (a fresh MSAL cache, no network), at most once per entraRebuildCooldown.
func (s *entraTokenSource) Invalidate(failed string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != failed {
		return
	}
	s.token = ""
	s.expires = time.Time{}
	recordEntraTokenEvent(entraTokenInvalidated)
	if s.newCred == nil || time.Since(s.rebuiltAt) < entraRebuildCooldown {
		return
	}
	if c, err := s.newCred(); err == nil {
		s.cred = c
		s.rebuiltAt = time.Now()
	}
}

// entraSources keeps one token source per service principal, process-wide — the factory builds a provider per
// request, and without this every chat turn would pay a client-credentials round trip. Keyed by a hash of the
// credential, so a rotated secret gets its own source (never a token minted for the old one).
type entraSourceEntry struct {
	src *entraTokenSource
	// lastUsed is the registry's eviction clock, touched on every hit — the token source itself carries no timestamp.
	lastUsed time.Time
}

var entraSources = struct {
	mu sync.Mutex
	m  map[string]entraSourceEntry
}{m: map[string]entraSourceEntry{}}

// entraSourcesMax bounds the registry; a deployment realistically holds a handful of service principals.
const entraSourcesMax = 256

// entraCredentialOptions wires the Entra ID credential to the package's runtime client — azidentity's default
// pipeline bypasses the egress guard and follows redirects — and pins the authority to the Azure public cloud:
// with no cloud set, azidentity would take AZURE_AUTHORITY_HOST from the environment and could silently send
// every user's client secret to that authority.
func entraCredentialOptions() *azidentity.ClientSecretCredentialOptions {
	opts := &azidentity.ClientSecretCredentialOptions{}
	opts.Cloud = cloud.AzurePublic
	opts.Transport = azureHTTPClient()
	return opts
}

// entraSourceKey is the registry key of one service principal: a hash, so the map holds no secret in its keys.
func entraSourceKey(tenantID, clientID, clientSecret string) string {
	h := sha256.Sum256([]byte(tenantID + "\x00" + clientID + "\x00" + clientSecret))
	return hex.EncodeToString(h[:])
}

// pruneIdleEntraEntries deletes m's entries idle past entraSourceIdleTTL. The caller holds only the mutex
// guarding m — taking any other registry's here would invert resetEntraSources' lock order.
func pruneIdleEntraEntries(m map[string]entraSourceEntry, now time.Time) {
	for k, e := range m {
		if now.Sub(e.lastUsed) > entraSourceIdleTTL {
			delete(m, k)
		}
	}
}

// evictOldestEntraEntryLocked drops the least recently used entry of a full registry (caller holds its mutex)
// — dropping one entry, not wiping the registry, which also discarded the sources active turns were about to hit.
func evictOldestEntraEntryLocked(m map[string]entraSourceEntry) {
	oldest, oldestUsed := "", time.Time{}
	for k, e := range m {
		if oldest == "" || e.lastUsed.Before(oldestUsed) {
			oldest, oldestUsed = k, e.lastUsed
		}
	}
	delete(m, oldest)
}

// forgetEntraSource drops one principal's registry entry so a removed configuration's credential object and
// cached token do not outlive it; a source a running request still holds keeps working.
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
	pruneIdleEntraEntries(entraSources.m, now)
	if e, ok := entraSources.m[key]; ok {
		e.lastUsed = now
		entraSources.m[key] = e
		return e.src, nil
	}
	// The credential must fetch through the same client as every other Azure hop (entraCredentialOptions).
	// The constructor is kept so Invalidate can rebuild the credential (a fresh MSAL cache).
	newCred := func() (azcore.TokenCredential, error) {
		return azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, entraCredentialOptions())
	}
	cred, err := newCred()
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry: entra id credential: %w", err)
	}
	if len(entraSources.m) >= entraSourcesMax {
		evictOldestEntraEntryLocked(entraSources.m)
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
	entraCLISources.mu.Lock()
	entraCLISources.m = map[string]entraSourceEntry{}
	entraCLISources.mu.Unlock()
}

// entraCLISources keeps one token source per tenant for the Azure CLI credential: the credential itself holds
// no secret, so the tenant is the whole identity. Same registry shape, bounds and idle TTL as entraSources.
var entraCLISources = struct {
	mu sync.Mutex
	m  map[string]entraSourceEntry
}{m: map[string]entraSourceEntry{}}

// entraCLISourceFor returns the CLI credential's token source for tenantID ("" = the CLI's current tenant).
// The credential shells out to `az` on every refresh — the CLI owns its token cache — so no Invalidate-style
// rebuild exists; a refused token surfaces as the sign-in message.
func entraCLISourceFor(tenantID string) (*entraTokenSource, error) {
	key := entraSourceKey(tenantID, "entra-cli", "")
	entraCLISources.mu.Lock()
	defer entraCLISources.mu.Unlock()
	now := time.Now()
	pruneIdleEntraEntries(entraCLISources.m, now) // same idle policy, its own map
	if e, ok := entraCLISources.m[key]; ok {
		e.lastUsed = now
		entraCLISources.m[key] = e
		return e.src, nil
	}
	opts := &azidentity.AzureCLICredentialOptions{}
	if tenantID != "" {
		opts.TenantID = tenantID
	}
	cred, err := azidentity.NewAzureCLICredential(opts)
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry: azure cli credential: %w", err)
	}
	if len(entraCLISources.m) >= entraSourcesMax {
		evictOldestEntraEntryLocked(entraCLISources.m)
	}
	s := newEntraTokenSource(cred)
	entraCLISources.m[key] = entraSourceEntry{src: s, lastUsed: now}
	return s, nil
}

// forgetEntraCLISource drops one tenant's CLI source (document deleted or replaced).
func forgetEntraCLISource(tenantID string) {
	entraCLISources.mu.Lock()
	delete(entraCLISources.m, entraSourceKey(tenantID, "entra-cli", ""))
	entraCLISources.mu.Unlock()
}
