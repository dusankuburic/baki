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

// Resource and model tests (Part 9). They never have side effects: a resource test is an authenticated probe
// with no model call; a chat/Claude model test is one chat capped at 16 output tokens; an embedding test one
// Embed of "ping"; an agent test a read-only lookup (a run is billed and may call hosted tools).
//
// Both run on an isolated provider with a THROWAWAY Entra ID token source, saved credentials or not: an
// unsaved (draft) credential must never enter — or be served a token from — the process-wide registry, and a
// fresh sign-in tells a saved credential's truth too. Nothing is written to the chat path's caches, no usage
// row is recorded, and every request — the sign-in included — goes through the Azure client (egress guard).
// The one memo a model test can teach is the request parameters its model refuses (learnedRepairs).

var (
	// ErrAzureEndpointNotFound marks a resource endpoint that answered 404 on its own v1 model route: not a
	// resource root — a project/deployment URL, a gateway path.
	ErrAzureEndpointNotFound = errors.New("azure ai foundry: the endpoint was not found")
	// ErrAzureProjectNotFound marks a project endpoint that answered 404: a wrong project name, or a classic
	// hub-based project without the v1 routes.
	ErrAzureProjectNotFound = errors.New("azure ai foundry: the project was not found")
	// ErrAzureAgentNotFound marks an agent lookup the project answered with
	// 404: no agent of that name.
	ErrAzureAgentNotFound = errors.New("azure ai foundry: the agent was not found")
	// ErrAzureAgentNotRunnable marks an agent that exists but cannot chat in baki: a hosted/external/voice
	// agent, a disabled one, or one whose latest version is not active.
	ErrAzureAgentNotRunnable = errors.New("azure ai foundry: the agent cannot chat in baki")
)

// AzureProjectRefusalError is the project data plane refusing a request of discovery or of a test: 401/403
// (both ErrProviderPermissionDenied and ErrApiKeyInvalid) or 404 (ErrAzureProjectNotFound, or
// ErrAzureAgentNotFound for an agent lookup).
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

// classifyProjectFailure turns a project route's definitive 401, 403 or 404 (azureProjectStatusError in
// err's chain) into an *AzureProjectRefusalError; agent names the agent a lookup asked for. Else err as is.
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

// azureProbeBodyMax caps what the resource probe reads of an answer: a 200's body (the model catalogue) is
// drained only this far; an error body only this far for its explanation.
const azureProbeBodyMax = 64 << 10

// The model test's request: one short prompt, at most azureProbeMaxTokens of
// output — the connection test's cap.
const (
	azureProbePrompt    = "ping"
	azureProbeMaxTokens = 16
)

// throwawayEntraSources builds a private Entra ID token source per provider (entraSourceFactory): never in,
// or served from, the process-wide registries, gone with the provider. The credential is built exactly as the
// shared one is (same authority, same Azure client), so the sign-in passes the egress guard.
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

// ProbeAzureFoundryResource tests one resource's credential document — saved, or a draft whose stored secret
// the caller carried over — without calling a model: sign in, GET {endpoint}/openai/v1/models as an
// authentication probe (body discarded), then, with a project endpoint and Entra ID, the first page of the
// project's deployments. nil means reachable. Errors: ErrProviderConfigInvalid (no request made), the
// sign-in's own failure, 401/403 permission denials with Azure's explanation (after one fresh-token retry),
// 404 ErrAzureEndpointNotFound, 429 *RateLimitError, *AzureProjectRefusalError or the listing's failure from
// the project, a connection failure as is.
func ProbeAzureFoundryResource(ctx context.Context, raw string) error {
	return newAzureProbeProvider(raw, nil, "").probeResource(ctx)
}

// ProbeAzureFoundryModel tests one model of a resource (raw = the document, m = a saved entry or a draft
// {Kind, Deployment}): a chat/Claude model answers one "ping" capped at 16 output tokens (a reply cut at the
// cap — a reasoning model thinking — passes: the call succeeded); an embedding model embeds "ping"; an agent
// is looked up with one GET, never run — it must exist and be runnable. An invalid m is an *AzureConfigError;
// no usage row is recorded and no memo learns.
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
	// A sign-in Entra ID refuses is the answer — before any request to the resource (asking first keeps the
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

// probeModelsRoute GETs {endpoint}/openai/v1/models — the catalogue, which needs an authenticated caller —
// and classifies the answer as a chat request (handleChatError), except a 404: not a resource root.
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

// probeProjectRoute reads the first page of the project's deployments — one GET, with projectGET's single
// fresh-token retry on a 401 — to prove the project endpoint and the principal's role on it.
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

// probeAgent looks the agent up (GET {project}/agents/{name}, read-only) and checks it can chat in baki as
// the listing would. It never POSTs: running an agent is billed and may call its hosted tools.
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
