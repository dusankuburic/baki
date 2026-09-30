package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// Resource and model tests (Part 9, "Testing a resource or a model"). They
// never have side effects and cost almost nothing:
//   - a resource test is an authenticated probe with no model call — it works
//     on a resource with no models, and costs nothing;
//   - a chat or Claude model test is one chat capped at 16 output tokens, an
//     embedding model test one Embed of "ping": one tiny billed request;
//   - an agent test is a read-only lookup of the agent — it is never run (a
//     run is billed and may call the agent's hosted tools).
//
// Both run on an isolated provider with a THROWAWAY Entra ID token source,
// saved credentials or not: an unsaved (draft) credential must never enter —
// or be served a token from — the process-wide source registry, and a fresh
// sign-in tells a saved credential's truth too (an expired secret keeps
// working while a cached token lives). Nothing is written to the process
// caches the chat path reads about a user's resource (the project listing
// cache, the agent kinds, the served-model memo, the learned windows), no
// usage row is recorded (no audited wrapper), and every request — the
// sign-in included — goes through the Azure client, so the egress dial guard
// applies. The one memo a model test can teach is the one any call to the
// deployment would: the request parameters its model refuses (learnedRepairs,
// keyed by endpoint and deployment, no secret).

var (
	// ErrAzureEndpointNotFound marks a resource endpoint that answered 404 on
	// its own v1 model route (GET /openai/v1/models): not a resource root — a
	// project or deployment URL, a gateway path — so no deployment on it
	// could answer either.
	ErrAzureEndpointNotFound = errors.New("azure ai foundry: the endpoint was not found")
	// ErrAzureProjectNotFound marks a project endpoint that answered 404: a
	// wrong project name, or a classic hub-based project without the v1
	// routes.
	ErrAzureProjectNotFound = errors.New("azure ai foundry: the project was not found")
	// ErrAzureAgentNotFound marks an agent lookup the project answered with
	// 404: no agent of that name.
	ErrAzureAgentNotFound = errors.New("azure ai foundry: the agent was not found")
	// ErrAzureAgentNotRunnable marks an agent that exists but cannot chat in
	// baki: a hosted, external or voice agent (not runnable by
	// agent_reference), a disabled one, or one whose latest version is not
	// active.
	ErrAzureAgentNotRunnable = errors.New("azure ai foundry: the agent cannot chat in baki")
)

// AzureProjectRefusalError is the project data plane refusing a request of
// discovery or of a test: 401 (the Entra ID principal is not authorized on
// the project, a fresh token refused too), 403 (a missing role, the project's
// network rules) — both ErrProviderPermissionDenied and ErrApiKeyInvalid, as
// a resource's permission denial is — or 404 (ErrAzureProjectNotFound, or
// ErrAzureAgentNotFound for an agent lookup). Message is one line for the
// user.
type AzureProjectRefusalError struct {
	Status  int
	Message string
	agent   bool
}

func (e *AzureProjectRefusalError) Error() string { return e.Message }

func (e *AzureProjectRefusalError) Unwrap() []error {
	if e.Status == http.StatusNotFound {
		if e.agent {
			return []error{ErrAzureAgentNotFound}
		}
		return []error{ErrAzureProjectNotFound}
	}
	return []error{ErrProviderPermissionDenied, ErrApiKeyInvalid}
}

// classifyProjectFailure turns a project route's definitive 401, 403 or 404
// (azureProjectStatusError anywhere in err's chain) into an
// *AzureProjectRefusalError; agent names the agent a lookup asked for ("" for
// the project listings). Anything else is returned as is.
func classifyProjectFailure(err error, agent string) error {
	var se *azureProjectStatusError
	if !errors.As(err, &se) {
		return err
	}
	switch se.status {
	case http.StatusUnauthorized:
		return &AzureProjectRefusalError{Status: se.status,
			Message: "the Microsoft Entra ID principal is not authorized on the Foundry project (HTTP 401): give it a role on the project, such as Azure AI User"}
	case http.StatusForbidden:
		msg := se.message
		if msg == "" {
			msg = "the Foundry project refused the request (HTTP 403): check the project's networking rules and the role the principal holds on it"
		}
		return &AzureProjectRefusalError{Status: se.status, Message: oneLine(msg, azureAuthMessageMax)}
	case http.StatusNotFound:
		if agent != "" {
			return &AzureProjectRefusalError{Status: se.status, agent: true,
				Message: fmt.Sprintf("the Foundry project has no agent named %q", agent)}
		}
		return &AzureProjectRefusalError{Status: se.status,
			Message: "the Foundry project was not found (HTTP 404): check the project endpoint (https://<resource>.services.ai.azure.com/api/projects/<project>) — a classic hub-based project has no v1 routes"}
	}
	return err
}

// azureProbeTimeout bounds one test as a whole (a sign-in, the probe, the
// project probe; or the one model call). A variable so tests can shorten it.
var azureProbeTimeout = 30 * time.Second

// azureProbeBodyMax caps what the resource probe reads of an answer: a 200's
// body (the resource's model catalogue, which the probe does not need) is
// drained only this far before the connection is dropped, an error body only
// this far for its explanation.
const azureProbeBodyMax = 64 << 10

// The model test's request: one short prompt, at most azureProbeMaxTokens of
// output — the connection test's cap.
const (
	azureProbePrompt    = "ping"
	azureProbeMaxTokens = 16
)

// throwawayEntraSources builds a private Entra ID token source per provider
// (see entraSourceFactory): never inserted into, or served from, the
// process-wide registries, and gone with the provider. The credential is
// built exactly as the shared one is — the same public-cloud authority and
// the same Azure client (entraCredentialOptions), so the sign-in passes the
// egress guard like every other Azure hop.
var throwawayEntraSources = entraSourceFactory{secret: newThrowawayEntraSource, cli: newThrowawayEntraCLISource}

func newThrowawayEntraSource(tenantID, clientID, clientSecret string) (*entraTokenSource, error) {
	newCred := func() (azcore.TokenCredential, error) {
		return azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, entraCredentialOptions())
	}
	cred, err := newCred()
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry: entra id credential: %w", err)
	}
	s := newEntraTokenSource(cred)
	s.newCred = newCred
	return s, nil
}

func newThrowawayEntraCLISource(tenantID string) (*entraTokenSource, error) {
	opts := &azidentity.AzureCLICredentialOptions{}
	if tenantID != "" {
		opts.TenantID = tenantID
	}
	cred, err := azidentity.NewAzureCLICredential(opts)
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry: azure cli credential: %w", err)
	}
	return newEntraTokenSource(cred), nil
}

// newAzureProbeProvider builds the isolated, throwaway-credential registry
// provider a test runs on (see the file comment).
func newAzureProbeProvider(raw string, models []AzureRegisteredModel, embeddingDeployment string) *AzureFoundryProvider {
	p := newAzureFoundryConnectionProvider(raw, models, embeddingDeployment, azureHTTPClient(), throwawayEntraSources)
	p.isolated = true
	return p
}

// ProbeAzureFoundryResource tests one resource's credential document —
// saved, or a draft with the stored secret already carried over by the
// caller — without calling a model: it signs in (Entra ID or Azure CLI: a
// token first), then GETs {endpoint}/openai/v1/models as an authentication
// probe (the body is discarded), then, with a project endpoint and Entra ID,
// reads the first page of the project's deployments. nil means the resource
// is reachable with this credential. The errors:
//   - ErrProviderConfigInvalid (with the *AzureConfigError) — the document
//     does not validate (no request is made);
//   - the sign-in's own failure (ErrEntraCredentialsRejected,
//     ErrEntraTokenUnavailable);
//   - 401: ErrApiKeyInvalid for an API key; ErrEntraTokenRejected (after one
//     retry with a fresh token) for Entra ID; ErrProviderPermissionDenied
//     with Azure's explanation for a PermissionDenied code;
//   - 403: ErrProviderPermissionDenied with Azure's explanation (network
//     rules, a missing role);
//   - 404: ErrAzureEndpointNotFound;
//   - 429: a *RateLimitError (the resource answered, throttled);
//   - the project: *AzureProjectRefusalError (401, 403, 404), else the
//     listing's failure;
//   - a connection failure as is (egress.DescribeFailure names it).
func ProbeAzureFoundryResource(ctx context.Context, raw string) error {
	return newAzureProbeProvider(raw, nil, "").probeResource(ctx)
}

// ProbeAzureFoundryModel tests one model of a resource: raw is the resource's
// credential document (saved or draft, as for ProbeAzureFoundryResource), m
// the model — a saved entry, or a draft {Kind, Deployment}. A chat or Claude
// model answers one "ping" capped at 16 output tokens (a reply cut at that
// cap — a reasoning model thinking — passes: the call succeeded); an
// embedding model embeds "ping"; an agent is looked up with one GET
// ({project}/agents/{name}), never run: it must exist
// (ErrAzureAgentNotFound) and be runnable in baki (ErrAzureAgentNotRunnable),
// and it needs a project endpoint and Entra ID (ErrProviderConfigInvalid). An
// invalid m is an *AzureConfigError (INVALID_KIND, INVALID_DEPLOYMENT). The
// model's own errors come back as the chat path classifies them
// (ErrApiKeyInvalid, ErrProviderPermissionDenied, a rate limit, a 404
// "DeploymentNotFound" answer, …). No usage row is recorded and no memo
// learns from the call.
func ProbeAzureFoundryModel(ctx context.Context, raw string, m AzureRegisteredModel) error {
	m.Kind, m.Deployment = strings.TrimSpace(m.Kind), strings.TrimSpace(m.Deployment)
	check := m
	check.ContextWindow, check.MaxOutputTokens = 0, 0
	if err := ValidateAzureRegisteredModel(check); err != nil {
		return err
	}
	embedding := ""
	if m.Kind == AzureModelKindEmbedding {
		embedding = m.Deployment
	}
	return newAzureProbeProvider(raw, []AzureRegisteredModel{m}, embedding).probeModel(ctx, m)
}

// probeResource is ProbeAzureFoundryResource on a built provider.
func (p *AzureFoundryProvider) probeResource(ctx context.Context) error {
	if p.configErr != nil {
		return p.configErr
	}
	ctx, cancel := context.WithTimeout(ctx, azureProbeTimeout)
	defer cancel()
	// A sign-in Entra ID refuses is the answer — before any request to the
	// resource (authorize would fetch the same token; asking first keeps the
	// refusal from reading as the resource's).
	if p.tokens != nil {
		if _, err := p.entraToken(ctx); err != nil {
			return err
		}
	}
	if err := p.withTokenRetry(ctx, p.probeModelsRoute); err != nil {
		return err
	}
	if !p.cfg.listsProjectDeployments() {
		return nil
	}
	return p.probeProjectRoute(ctx)
}

// probeModelsRoute GETs {endpoint}/openai/v1/models — the resource's model
// catalogue, which needs an authenticated caller — and classifies the answer
// as a chat request's (handleChatError: the Entra ID token rejection, the
// permission denial, the rate limit), except that a 404 says the endpoint is
// not a resource root. The body is never used.
func (p *AzureFoundryProvider) probeModelsRoute(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.Endpoint+"/openai/v1/models", nil)
	if err != nil {
		return err
	}
	if err := p.authorize(ctx, req); err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("azure ai foundry: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, azureProbeBodyMax))
	if resp.StatusCode == http.StatusOK {
		// Drained up to the cap only: the catalogue can run to megabytes, and
		// a body cut short costs nothing but the connection.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the probe's answer (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w (HTTP 404 on %s): use the resource endpoint — https://<resource>.openai.azure.com or https://<resource>.services.ai.azure.com, without a path",
			ErrAzureEndpointNotFound, p.cfg.Endpoint)
	}
	return p.handleChatError(resp, body)
}

// probeProjectRoute reads the first page of the project's deployments — one
// GET, with projectGET's single fresh-token retry on a 401 — to prove the
// project endpoint and the principal's role on it.
func (p *AzureFoundryProvider) probeProjectRoute(ctx context.Context) error {
	if _, err := p.projectGET(ctx, p.cfg.ProjectEndpoint+"/deployments?api-version="+azureProjectAPIVersion); err != nil {
		return classifyProjectFailure(err, "")
	}
	return nil
}

// probeModel is ProbeAzureFoundryModel on a built provider (m validated).
func (p *AzureFoundryProvider) probeModel(ctx context.Context, m AzureRegisteredModel) error {
	if p.configErr != nil {
		return p.configErr
	}
	ctx, cancel := context.WithTimeout(ctx, azureProbeTimeout)
	defer cancel()
	switch m.Kind {
	case AzureModelKindChat, AzureModelKindClaude:
		target, _ := AzureModelTarget(m.Kind, m.Deployment)
		_, err := p.Chat(ctx, Request{
			Model:     target,
			Messages:  []Message{{Role: "user", Content: azureProbePrompt}},
			MaxTokens: azureProbeMaxTokens,
		})
		if errors.Is(err, ErrOutputLimit) {
			// The model answered and was billed; it spent the tiny cap on
			// reasoning before any text.
			return nil
		}
		return err
	case AzureModelKindEmbedding:
		_, err := p.Embed(ctx, []string{azureProbePrompt})
		return err
	case AzureModelKindAgent:
		return p.probeAgent(ctx, m.Deployment)
	}
	return azureCfgErr(AzureCfgErrInvalidKind, "unknown model kind %q", m.Kind)
}

// probeAgent looks the agent up — GET {project}/agents/{name}, read-only —
// and checks it can chat in baki as the listing would (runsViaAgentReference,
// usable). It never POSTs: running an agent is billed and may call its hosted
// tools.
func (p *AzureFoundryProvider) probeAgent(ctx context.Context, name string) error {
	if err := p.agentConfigError(); err != nil {
		return err
	}
	body, err := p.projectGET(ctx, p.cfg.ProjectEndpoint+"/agents/"+url.PathEscape(name)+"?api-version="+azureProjectAPIVersion)
	if err != nil {
		return classifyProjectFailure(err, name)
	}
	var a azureAgent
	if err := json.Unmarshal(body, &a); err != nil {
		return fmt.Errorf("azure ai foundry: read agent %q: %w", name, err)
	}
	if !a.runsViaAgentReference() {
		return fmt.Errorf("%w: %q is a %s agent — only prompt and workflow agents run on the project's Responses route", ErrAzureAgentNotRunnable, name, oneLine(a.kind(), 32))
	}
	if !a.usable() {
		state, status := a.State, a.Versions.Latest.Status
		if state == "" {
			state = "enabled"
		}
		if status == "" {
			status = "unknown"
		}
		return fmt.Errorf("%w: %q is not ready to answer (state %s, latest version %s) — enable it, or wait until its latest version is active", ErrAzureAgentNotRunnable, name, oneLine(state, 32), oneLine(status, 32))
	}
	return nil
}
