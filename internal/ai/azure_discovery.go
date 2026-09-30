package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/egress"
)

// Discovery (Part 9): what a SAVED resource's Foundry project offers — its
// deployments, each mapped to a registry kind, and its agents — for AI
// Accounts to suggest as models to add. The picker never shows a discovered
// model the user did not add; discovery only proposes. It needs the project
// data plane (a project endpoint and Microsoft Entra ID or Azure CLI
// sign-in), and it runs on an ISOLATED provider: nothing it learns reaches
// the process caches the chat path reads (the project listing cache, the
// remembered agent kinds, the served-model memo, the learned windows) — a
// suggestion is not a registered model.

// ErrAzureDiscoveryUnavailable marks a resource that cannot list its project:
// it has no project endpoint, or it signs in with an API key (the project
// routes reject keys). The API answers it as DISCOVERY_UNAVAILABLE; its
// message says which, and what to do.
var ErrAzureDiscoveryUnavailable = errors.New("azure ai foundry: project discovery is unavailable for this resource")

// azureDiscoveryTimeout bounds one discovery — the token, then both project
// listings, every page. A variable so tests can shorten it.
var azureDiscoveryTimeout = 30 * time.Second

// AzureDiscovery is what one resource's project offers. A listing that
// failed while the other succeeded is a warning, not an error; so is a
// listing a cap cut short, and agents skipped because baki cannot run them.
type AzureDiscovery struct {
	Deployments []AzureDiscoveredDeployment `json:"deployments"`
	Agents      []AzureDiscoveredAgent      `json:"agents"`
	Warnings    []string                    `json:"warnings"`
}

// AzureDiscoveredDeployment is one project deployment as a candidate entry.
type AzureDiscoveredDeployment struct {
	// Name is the deployment name (the entry's deployment).
	Name string `json:"name"`
	// Kind is the registry kind it maps to: AzureModelKindChat,
	// AzureModelKindClaude (published by Anthropic, or a "claude…" model) or
	// AzureModelKindEmbedding (its capabilities or model name say so).
	Kind string `json:"kind"`
	// Model is the model the deployment serves ("" when the listing does not
	// say).
	Model string `json:"model,omitempty"`
	// ContextWindow is the served model's catalogued window (0 = unknown, and
	// always for a model router or an embedding deployment).
	ContextWindow int `json:"contextWindow,omitempty"`
	// AlreadyAdded reports a registry entry with this deployment name on the
	// resource (any non-agent kind); ModelID is its "m/<entryId>" id.
	AlreadyAdded bool   `json:"alreadyAdded"`
	ModelID      string `json:"modelId,omitempty"`
}

// AzureDiscoveredAgent is one project agent baki can run (a prompt or
// workflow agent, enabled, its latest version active) as a candidate entry.
type AzureDiscoveredAgent struct {
	Name string `json:"name"`
	// Kind is always AzureModelKindAgent (the registry kind), so a row reads
	// like a deployment's.
	Kind string `json:"kind"`
	// Workflow marks a workflow agent: a preview that retires on December 1,
	// 2026.
	Workflow bool `json:"workflow,omitempty"`
	// Deployment is the project deployment the agent runs on, Model the model
	// behind it when the deployments listing or the catalogue knows it, and
	// ContextWindow that model's catalogued window (0 = unknown).
	Deployment    string `json:"deployment,omitempty"`
	Model         string `json:"model,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	// AlreadyAdded reports an agent entry with this name on the resource;
	// ModelID is its "m/<entryId>" id.
	AlreadyAdded bool   `json:"alreadyAdded"`
	ModelID      string `json:"modelId,omitempty"`
}

// DiscoverAzureFoundryProject lists what a saved resource's project offers:
// raw is the resource's credential document, registered its registry entries
// (marked AlreadyAdded). The project's deployments and agents are listed
// concurrently; one failing while the other succeeds becomes a warning. The
// errors:
//   - ErrAzureDiscoveryUnavailable — no project endpoint, or an API key;
//   - ErrProviderConfigInvalid (with the *AzureConfigError) — the document
//     does not validate;
//   - the Entra ID sign-in's own failure (ErrEntraCredentialsRejected,
//     ErrEntraTokenUnavailable) — no project request is made without a token;
//   - *AzureProjectRefusalError — both listings refused: 401 or 403
//     (ErrProviderPermissionDenied: the principal has no role on the project,
//     or its network rules refuse the caller) or 404 (ErrAzureProjectNotFound);
//   - anything else both listings failed with (a rate limit, a network
//     failure, a 5xx), as is.
//
// It never writes the process caches: the provider is isolated.
func DiscoverAzureFoundryProject(ctx context.Context, raw string, registered []AzureRegisteredModel) (AzureDiscovery, error) {
	cfg, err := ParseAzureFoundryConfig(raw)
	if err == nil {
		// The project is listed as it is: the default resource's legacy
		// deployment fields (a Claude list in particular) must not colour the
		// kinds.
		cfg = cfg.withoutDeployments()
		err = cfg.Validate(currentAzureHostPolicy())
	}
	if err != nil {
		return AzureDiscovery{}, fmt.Errorf("%w: %w", ErrProviderConfigInvalid, err)
	}
	if !cfg.ProjectAccess() {
		return AzureDiscovery{}, azureDiscoveryUnavailable(cfg)
	}
	// A saved credential: the process-wide token source serves it, as it
	// serves the chat path (the Entra ID source registry is not a listing
	// cache; the resource and model tests of UNSAVED credentials use a
	// throwaway one instead).
	p := newAzureFoundryProvider(cfg, nil, azureHTTPClient())
	p.isolated = true
	return p.discoverProject(ctx, registered)
}

// azureDiscoveryUnavailable says why a document cannot list its project.
func azureDiscoveryUnavailable(cfg AzureFoundryConfig) error {
	if cfg.ProjectEndpoint == "" {
		return fmt.Errorf("%w: the resource has no project endpoint — add the Foundry project endpoint (https://<resource>.services.ai.azure.com/api/projects/<project>) to discover its deployments and agents", ErrAzureDiscoveryUnavailable)
	}
	return fmt.Errorf("%w: an API key cannot list a project's deployments and agents — sign in with Microsoft Entra ID to discover them, or add the deployments by name", ErrAzureDiscoveryUnavailable)
}

// discoverProject lists the project on an isolated provider (see
// DiscoverAzureFoundryProject).
func (p *AzureFoundryProvider) discoverProject(ctx context.Context, registered []AzureRegisteredModel) (AzureDiscovery, error) {
	if p.configErr != nil {
		return AzureDiscovery{}, p.configErr
	}
	if !p.cfg.ProjectAccess() {
		return AzureDiscovery{}, azureDiscoveryUnavailable(p.cfg)
	}
	ctx, cancel := context.WithTimeout(ctx, azureDiscoveryTimeout)
	defer cancel()
	// The token first: a sign-in Entra ID refuses is the answer, and asking
	// for it again from both listings would only repeat the refusal.
	if _, err := p.entraToken(ctx); err != nil {
		return AzureDiscovery{}, err
	}

	actx, cancelAgents := context.WithCancel(ctx)
	defer cancelAgents()
	var (
		wg          sync.WaitGroup
		deployments []projectDeployment
		depErr      error
		agents      []azureAgent
		report      azureAgentsReport
		agentsErr   error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		deployments, depErr = p.fetchProjectDeployments(ctx)
		if depErr != nil && (errors.Is(depErr, errEntraToken) || errors.Is(depErr, ErrRateLimited)) {
			// The agents route shares the token and the project's quota.
			cancelAgents()
		}
	}()
	go func() {
		defer wg.Done()
		agents, report, agentsErr = p.fetchAgentsReport(actx)
	}()
	wg.Wait()

	if depErr != nil && agentsErr != nil {
		return AzureDiscovery{}, classifyProjectFailure(depErr, "")
	}
	out := AzureDiscovery{Deployments: []AzureDiscoveredDeployment{}, Agents: []AzureDiscoveredAgent{}, Warnings: []string{}}
	var deployedModels map[string]string
	if depErr == nil {
		for _, d := range deployments {
			out.Deployments = append(out.Deployments, discoveredDeployment(d, registered))
		}
		deployedModels = deployedModelNames(deployments)
	} else {
		out.Warnings = append(out.Warnings, "The project's deployments could not be listed: "+describeProjectFailure(depErr))
	}
	if agentsErr == nil {
		for _, a := range agents {
			out.Agents = append(out.Agents, discoveredAgent(a, deployedModels, registered))
		}
		if report.skipped > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%d of the project's agents cannot chat in baki and are not listed: only enabled prompt and workflow agents whose latest version is active can (hosted, external and voice agents cannot).", report.skipped))
		}
		if report.truncated {
			out.Warnings = append(out.Warnings, fmt.Sprintf("The project has more agents than discovery lists (at most %d): add the others by name.", azureProjectMaxAgents))
		}
	} else {
		out.Warnings = append(out.Warnings, "The project's agents could not be listed: "+describeProjectFailure(agentsErr))
	}
	return out, nil
}

// discoveredDeployment maps one listed deployment to a candidate entry.
func discoveredDeployment(d projectDeployment, registered []AzureRegisteredModel) AzureDiscoveredDeployment {
	out := AzureDiscoveredDeployment{Name: d.name, Kind: AzureModelKindChat, Model: d.modelName}
	switch {
	case d.claude:
		out.Kind = AzureModelKindClaude
	case d.embedding:
		out.Kind = AzureModelKindEmbedding
	}
	if out.Kind != AzureModelKindEmbedding {
		// The model behind it, else a deployment named after its model (the
		// portal's default names), Claude's by the Claude catalogue.
		sizedBy := d.modelName
		if sizedBy == "" {
			sizedBy = d.name
			if d.claude {
				sizedBy = claudeModelPrefix + d.name
			}
		}
		out.ContextWindow = catalogWindow(sizedBy, d.name)
	}
	if e, ok := registeredEntry(registered, false, d.name); ok {
		out.AlreadyAdded, out.ModelID = true, registeredModelID(e)
	}
	return out
}

// discoveredAgent maps one listed agent to a candidate entry: the deployment
// it runs on, and the model behind that deployment when the deployments
// listing (deployedModels) or the catalogue knows it.
func discoveredAgent(a azureAgent, deployedModels map[string]string, registered []AzureRegisteredModel) AzureDiscoveredAgent {
	name := a.label()
	out := AzureDiscoveredAgent{Name: name, Kind: AzureModelKindAgent, Workflow: a.kind() == agentKindWorkflow, Deployment: a.model()}
	if dep := out.Deployment; dep != "" {
		if s, ok := deployedModels[strings.ToLower(dep)]; ok {
			out.Model = s
		} else if _, ok := catalogEntryFor(azureFoundryProviderID, dep); ok {
			out.Model = dep
		}
		if out.Model != "" {
			out.ContextWindow = catalogWindow(out.Model, dep)
		}
	}
	if e, ok := registeredEntry(registered, true, name); ok {
		out.AlreadyAdded, out.ModelID = true, registeredModelID(e)
	}
	return out
}

// catalogWindow is the catalogued context window of model, 0 when the
// catalogue does not know it or either name denotes a model router (each
// call may be routed to a model with another window).
func catalogWindow(model, deployment string) int {
	if isModelRouterAlias(model) || isModelRouterAlias(deployment) {
		return 0
	}
	if entry, ok := catalogEntryFor(azureFoundryProviderID, model); ok {
		return entry.ContextLimit
	}
	return 0
}

// registeredEntry finds the registry entry a discovered name is already
// added as: an agent entry for an agent, any other entry for a deployment
// (a deployment registered under another kind than discovery would suggest
// is still added), compared case-insensitively as the registry's unique index
// does.
func registeredEntry(registered []AzureRegisteredModel, agent bool, name string) (AzureRegisteredModel, bool) {
	for _, e := range registered {
		if (e.Kind == AzureModelKindAgent) != agent {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(e.Deployment), name) {
			return e, true
		}
	}
	return AzureRegisteredModel{}, false
}

// registeredModelID is an entry's "m/<entryId>" id, "" when its id is not a
// registry id.
func registeredModelID(e AzureRegisteredModel) string {
	if !azureRegistryID.MatchString(e.EntryID) {
		return ""
	}
	return RegistryModelID(e.EntryID)
}

// describeProjectFailure is the one-line reason a project listing failed, for
// a warning: a refusal's own explanation, fixed copy for a connection failure
// (whose raw text names resolved addresses and the operator's proxy) and a
// timeout, else the error's first line.
func describeProjectFailure(err error) string {
	var refusal *AzureProjectRefusalError
	switch classified := classifyProjectFailure(err, ""); {
	case errors.As(classified, &refusal):
		return refusal.Message
	case errors.Is(err, context.DeadlineExceeded):
		return "the project did not answer in time"
	case errors.Is(err, ErrRateLimited):
		return "the project is throttling requests — try again shortly"
	}
	if what, ok := egress.DescribeFailure(err); ok {
		return "could not connect to the project: " + what
	}
	return oneLine(err.Error(), azureAuthMessageMax)
}
