package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"pad-core/cache"
	"pad-core/logger"
)

// Foundry PROJECT endpoint extras (Entra ID only — the project data plane rejects API keys): the deployments
// listing, which reveals the model behind each deployment name, and the project's prompt agents, which the chat
// model picker offers as "Agent · <name>" entries with the id "agent:<name>".

// agentModelPrefix marks a model id that names a Foundry agent rather than a deployment: the chat service
// forces the tool loop for such ids and the provider routes them to the Responses API.
const agentModelPrefix = "agent:"

// IsAgentModel reports whether a model id names a Foundry agent.
func IsAgentModel(id string) bool { return strings.HasPrefix(id, agentModelPrefix) }

func agentNameOf(id string) string { return strings.TrimPrefix(id, agentModelPrefix) }

// azureProjectAPIVersion is the Foundry project data-plane version (a single
// identifier, passed on every non-OpenAI route).
const azureProjectAPIVersion = "v1"

const (
	azureProjectMaxPages  = 10
	azureProjectMaxAgents = 200
	// azureProjectAgentsPageSize is the List Agents `limit`: the operation's documented maximum (the default
	// page is 20 agents). A page of skipped kinds still counts against azureProjectMaxPages but not the cap.
	azureProjectAgentsPageSize = 100
	// azureProjectMaxBody caps one project page (a page of deployments or agents is a few KB).
	azureProjectMaxBody = 4 << 20
)

// azureProjectListing is one project listing attempt: the listing, and what of it the project answered.
type azureProjectListing struct {
	models []ModelInfo
	// deployments, agents: that listing answered in full, or with a definitive refusal (403, a 401 that survived
	// a fresh token, 404, 400) — an answer, not re-requested every turn. Both: the listing is complete.
	deployments, agents bool
	// retryAfter is the longest back-off a throttled listing asked for (0: none).
	retryAfter time.Duration
}

func (l azureProjectListing) complete() bool { return l.deployments && l.agents }

// projectExtras is projectExtrasListing's listing and whether it is complete.
func (p *AzureFoundryProvider) projectExtras(ctx context.Context, models []ModelInfo) ([]ModelInfo, bool) {
	l := p.projectExtrasListing(ctx, models)
	return l.models, l.complete()
}

// projectExtrasListing merges the project deployments listing into the document's deployments and appends the
// project's agents. Everything is best effort: a definitive refusal leaves the document's deployments and counts
// as answered; a transient failure returns what is known — the deployments pages read before it included — as
// incomplete (storeAzureListing decides what is cached). The two paginated listings run concurrently, each on
// its own context under ctx's deadline (the flight's budget), so neither listing's failure ends the other; a
// deployments failure that makes the agents call pointless (unobtainable token, 429 — the agents route shares
// its quota) cancels the agents fetch.
func (p *AzureFoundryProvider) projectExtrasListing(ctx context.Context, models []ModelInfo) azureProjectListing {
	if !p.cfg.listsProjectDeployments() {
		return azureProjectListing{models: models, deployments: true, agents: true}
	}
	dctx, cancelDeployments := context.WithCancel(ctx)
	defer cancelDeployments()
	actx, cancelAgents := context.WithCancel(ctx)
	defer cancelAgents()
	var (
		wg          sync.WaitGroup
		deployments []projectDeployment
		depErr      error
		agents      []azureAgent
		agentsErr   error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		deployments, depErr = p.fetchProjectDeployments(dctx)
		if depErr != nil && (errors.Is(depErr, errEntraToken) || errors.Is(depErr, ErrRateLimited)) {
			cancelAgents()
		}
	}()
	go func() {
		defer wg.Done()
		agents, agentsErr = p.fetchAgents(actx)
	}()
	wg.Wait()

	out := azureProjectListing{retryAfter: max(projectRetryAfter(depErr), projectRetryAfter(agentsErr))}
	var deployedModels map[string]string
	if len(deployments) > 0 {
		// Every page, or the pages read before a failure: the best decorations known for those deployments.
		models = p.mergeProjectDeployments(ctx, models, deployments)
		deployedModels = deployedModelNames(deployments)
	}
	switch {
	case depErr == nil:
		out.deployments = true
	case errors.Is(depErr, errEntraToken):
		logger.FromContext(ctx).Warn("azure ai foundry: project deployments unavailable; skipping the agents", "error", depErr)
		out.models = models
		return out
	case errors.Is(depErr, ErrApiKeyInvalid) && definitiveProjectFailure(depErr):
		// A fresh token was refused too: the principal has no role on the project, and the agents route too.
		logger.FromContext(ctx).Warn("azure ai foundry: project not accessible to the entra id principal; listing the configured deployments only", "error", depErr)
		out.models, out.deployments, out.agents = models, true, true
		return out
	case errors.Is(depErr, ErrRateLimited):
		logger.FromContext(ctx).Warn("azure ai foundry: project deployments throttled; the agents listing was abandoned", "error", depErr)
		out.models = models
		return out
	default:
		logger.FromContext(ctx).Warn("azure ai foundry: project deployments unavailable", "error", depErr, "listed", len(deployments))
		out.deployments = definitiveProjectFailure(depErr)
	}
	if agentsErr != nil {
		logger.FromContext(ctx).Warn("azure ai foundry: project agents unavailable", "error", agentsErr)
		out.models, out.agents = models, definitiveProjectFailure(agentsErr)
		return out
	}
	out.models, out.agents = append(models, p.agentModels(agents, deployedModels)...), true
	return out
}

// projectRetryAfter is the back-off a throttled project listing asked for (0: none, or not throttled).
func projectRetryAfter(err error) time.Duration {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl.RetryAfter
	}
	return 0
}

// deployedModelNames maps each listed deployment (lower-cased name) to the model it serves.
func deployedModelNames(deployments []projectDeployment) map[string]string {
	out := make(map[string]string, len(deployments))
	for _, d := range deployments {
		if d.modelName != "" {
			out[strings.ToLower(d.name)] = d.modelName
		}
	}
	return out
}

// mergeProjectDeployments applies the project listing to the document's entries (the model behind each one,
// the embedding flag) and appends the project's other deployments in listing order. A deployment the listing
// reports as Anthropic-published is a Claude deployment, listed under its "claude:<name>" id.
func (p *AzureFoundryProvider) mergeProjectDeployments(ctx context.Context, models []ModelInfo, deployments []projectDeployment) []ModelInfo {
	byID := make(map[string]projectDeployment, len(deployments))
	for _, d := range deployments {
		byID[strings.ToLower(d.listingID())] = d
	}
	seen := map[string]bool{}
	for i := range models {
		key := strings.ToLower(models[i].ID)
		d, ok := byID[key]
		if !ok && !models[i].Embedding && !IsClaudeModel(models[i].ID) {
			if cd, isClaude := byID[claudeModelPrefix+key]; isClaude {
				logger.FromContext(ctx).Debug("azure ai foundry: deployment is Anthropic-published; listing it as a Claude deployment", "deployment", cd.name)
				seen[key] = true
				models[i] = claudeDeploymentEntry(cd.name)
				key, d, ok = strings.ToLower(models[i].ID), cd, true
			}
		}
		seen[key] = true
		if !ok {
			continue
		}
		if d.claude {
			if d.modelName != "" {
				applyServedModel(azureFoundryProviderID, &models[i], d.modelName)
			}
			continue
		}
		embedding := models[i].Embedding || d.embedding
		if d.modelName != "" {
			models[i] = ModelInfo{ID: models[i].ID, DisplayName: models[i].ID, ContextLimit: p.ContextLimit()}
			applyServedModel(azureFoundryProviderID, &models[i], d.modelName)
		}
		models[i].Embedding = embedding
	}
	for _, d := range deployments {
		key := strings.ToLower(d.listingID())
		if seen[key] {
			continue
		}
		seen[key] = true
		var m ModelInfo
		switch {
		case d.claude:
			m = claudeDeploymentEntry(d.name)
			if d.modelName != "" {
				applyServedModel(azureFoundryProviderID, &m, d.modelName)
			}
		case d.modelName != "":
			m = ModelInfo{ID: d.name, DisplayName: d.name, ContextLimit: p.ContextLimit()}
			applyServedModel(azureFoundryProviderID, &m, d.modelName)
		default:
			m = p.deploymentEntry(d.name, false)
		}
		m.Embedding = d.embedding
		models = append(models, m)
	}
	return models
}

type projectDeployment struct {
	name      string
	modelName string
	// embedding: the deployment serves an embedding model (its capabilities say embeddings without chat completion, or its model name says so).
	embedding bool
	// claude: the deployment's model is published by Anthropic, so it answers on the Anthropic Messages route.
	claude bool
}

// listingID is the model-list id the deployment is offered under: the deployment name, prefixed for a Claude deployment.
func (d projectDeployment) listingID() string {
	if d.claude {
		return claudeModelPrefix + d.name
	}
	return d.name
}

// fetchProjectDeployments calls GET {project}/deployments?api-version=v1 and
// returns the deployments in listing order, deduplicated by lower-cased name. On an error it returns the
// deployments of the pages read before it, with the error: the caller decides what a partial listing is worth.
func (p *AzureFoundryProvider) fetchProjectDeployments(ctx context.Context) ([]projectDeployment, error) {
	var out []projectDeployment
	seen := map[string]bool{}
	next := p.cfg.ProjectEndpoint + "/deployments?api-version=" + azureProjectAPIVersion
	for page := 0; next != "" && page < azureProjectMaxPages; page++ {
		body, err := p.projectGET(ctx, next)
		if err != nil {
			return out, err
		}
		// Each item is decoded on its own: an undecodable one costs that entry, not the whole page (which used
		// to make the listing incomplete, hence uncached and re-fetched every turn).
		var parsed struct {
			Value    []json.RawMessage `json:"value"`
			NextLink string            `json:"nextLink"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return out, fmt.Errorf("parse deployments: %w", err)
		}
		for _, raw := range parsed.Value {
			var d struct {
				Name           string         `json:"name"`
				Type           string         `json:"type"`
				ModelName      string         `json:"modelName"`
				ModelPublisher string         `json:"modelPublisher"`
				ConnectionName string         `json:"connectionName"`
				Capabilities   map[string]any `json:"capabilities"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				logger.FromContext(ctx).Debug("azure ai foundry: skipping undecodable project deployment item", "error", err)
				continue
			}
			name := strings.TrimSpace(d.Name)
			if name == "" || seen[strings.ToLower(name)] {
				continue
			}
			if conn := strings.TrimSpace(d.ConnectionName); conn != "" {
				// A deployment that comes from a project CONNECTION (an AI gateway / external model endpoint) is
				// reachable through that connection only — /openai/v1 answers 404 for its name.
				logger.FromContext(ctx).Debug("azure ai foundry: skipping a connection deployment not callable on /openai/v1",
					"deployment", name, "connection", conn)
				continue
			}
			seen[strings.ToLower(name)] = true
			modelName := strings.TrimSpace(d.ModelName)
			claude := p.isClaudeDeployment(name, modelName, d.ModelPublisher)
			embedding := !claude && (isEmbeddingModelName(modelName) ||
				(capabilityTrue(d.Capabilities, "embeddings") && !capabilityTrue(d.Capabilities, "chat_completion")))
			out = append(out, projectDeployment{name: name, modelName: modelName, embedding: embedding, claude: claude})
		}
		next = p.sameProjectHost(ctx, parsed.NextLink)
	}
	if next != "" {
		// The listing was cut by the page cap with more pages pending — say so, or a missing deployment looks like a merge bug.
		logger.FromContext(ctx).Warn("azure ai foundry: project deployments listing stopped at the page cap; deployments beyond it are not listed",
			"pages", azureProjectMaxPages, "listed", len(out))
	}
	return out, nil
}

// isClaudeDeployment reports whether a listed deployment is a Claude deployment (the Anthropic Messages
// route): published by Anthropic, a model named "claude…" (a listing that misspells the publisher), or a name
// the document lists as a Claude deployment.
func (p *AzureFoundryProvider) isClaudeDeployment(name, modelName, publisher string) bool {
	if strings.EqualFold(strings.TrimSpace(publisher), anthropicModelPublisher) || isClaudeModelName(modelName) {
		return true
	}
	for _, d := range p.cfg.ClaudeDeployments {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// isEmbeddingModelName reports whether a model name denotes an embedding model (text-embedding-3-small, embed-v-4-0, …).
func isEmbeddingModelName(name string) bool {
	return strings.Contains(strings.ToLower(name), "embed")
}

// capabilityTrue reads one flag of a project deployment's capabilities map, whose values the service sends as strings ("true").
func capabilityTrue(caps map[string]any, key string) bool {
	switch v := caps[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// azureProjectStatusError is a project route's answer other than 200, 401 (ErrApiKeyInvalid) and 429 (RateLimitError).
type azureProjectStatusError struct {
	status  int
	message string
}

func (e *azureProjectStatusError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("status %d: %s", e.status, e.message)
	}
	return fmt.Sprintf("status %d", e.status)
}

// Unwrap keeps a definitive 401 an ErrApiKeyInvalid for errors.Is callers.
func (e *azureProjectStatusError) Unwrap() error {
	if e.status == http.StatusUnauthorized {
		return ErrApiKeyInvalid
	}
	return nil
}

// definitiveProjectFailure reports whether a project listing failure is an answer that will not change on a
// retry — a 4xx status error other than 408 (403/no-role 401, a classic hub project's 404, api-version 400).
func definitiveProjectFailure(err error) bool {
	var se *azureProjectStatusError
	return errors.As(err, &se) && se.status >= 400 && se.status < 500 && se.status != http.StatusRequestTimeout
}

// fetchAgents calls GET {project}/agents?api-version=v1&limit=<page size> (paginated) and returns each agent
// the shared Responses route can run (prompt and workflow agents, or no kind — older shapes); hosted, external
// (A2A) and voice agents are skipped, as are disabled ones. Each kind is remembered (rememberAgentKind).
func (p *AzureFoundryProvider) fetchAgents(ctx context.Context) ([]azureAgent, error) {
	agents, _, err := p.fetchAgentsReport(ctx)
	return agents, err
}

// azureAgentsReport is what fetchAgentsReport reports beyond the agents, for discovery's warnings: how many
// listed agents it skipped as not runnable here (kind or state), and whether a cap stopped the listing early.
type azureAgentsReport struct {
	skipped   int
	truncated bool
}

// fetchAgentsReport is fetchAgents with its report. An isolated provider (discovery) remembers no agent kind.
func (p *AzureFoundryProvider) fetchAgentsReport(ctx context.Context) ([]azureAgent, azureAgentsReport, error) {
	var out []azureAgent
	var report azureAgentsReport
	next := p.cfg.ProjectEndpoint + "/agents?api-version=" + azureProjectAPIVersion + "&limit=" + strconv.Itoa(azureProjectAgentsPageSize)
	for page := 0; next != "" && page < azureProjectMaxPages && len(out) < azureProjectMaxAgents; page++ {
		body, err := p.projectGET(ctx, next)
		if err != nil {
			return nil, report, err
		}
		var parsed struct {
			Data     []json.RawMessage `json:"data"`
			Value    []json.RawMessage `json:"value"`
			HasMore  bool              `json:"has_more"`
			LastID   string            `json:"last_id"`
			NextLink string            `json:"nextLink"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, report, fmt.Errorf("parse agents: %w", err)
		}
		items := parsed.Data
		if len(items) == 0 {
			items = parsed.Value
		}
		for _, raw := range items {
			// Per item, as for deployments: one agent of an unexpected shape must not make the whole listing incomplete.
			var a azureAgent
			if err := json.Unmarshal(raw, &a); err != nil {
				logger.FromContext(ctx).Debug("azure ai foundry: skipping undecodable project agent item", "error", err)
				continue
			}
			name := a.label()
			if name == "" {
				continue
			}
			if !a.runsViaAgentReference() {
				logger.FromContext(ctx).Debug("azure ai foundry: skipping agent not callable via agent_reference", "agent", name, "kind", a.kind())
				report.skipped++
				continue
			}
			if !a.usable() {
				logger.FromContext(ctx).Debug("azure ai foundry: skipping an agent that is disabled or whose latest version is not active",
					"agent", name, "state", a.State, "status", a.Versions.Latest.Status)
				report.skipped++
				continue
			}
			if !p.isolated {
				rememberAgentKind(p.cfg.ProjectEndpoint, name, a.kind())
			}
			out = append(out, a)
			if len(out) >= azureProjectMaxAgents {
				break
			}
		}
		switch {
		case parsed.NextLink != "":
			next = p.sameProjectHost(ctx, parsed.NextLink)
		case parsed.HasMore && parsed.LastID != "":
			next = p.cfg.ProjectEndpoint + "/agents?api-version=" + azureProjectAPIVersion + "&limit=" + strconv.Itoa(azureProjectAgentsPageSize) + "&after=" + url.QueryEscape(parsed.LastID)
		default:
			next = ""
		}
	}
	// Both caps truncate silently otherwise; a missing agent in the picker then looks like a merge bug.
	if next != "" {
		logger.FromContext(ctx).Warn("azure ai foundry: project agents listing stopped at the page cap; agents beyond it are not listed",
			"pages", azureProjectMaxPages, "listed", len(out))
	}
	if len(out) >= azureProjectMaxAgents {
		logger.FromContext(ctx).Warn("azure ai foundry: project agents listing stopped at the agent cap; agents beyond it are not listed",
			"cap", azureProjectMaxAgents)
	}
	report.truncated = next != "" || len(out) >= azureProjectMaxAgents
	return out, report, nil
}

// agentModels maps the listed agents to model-list entries. The definition's model is the PROJECT DEPLOYMENT
// the agent runs on, not a model name: deployedModels (authoritative) or the catalogue resolve it; otherwise
// ServedModel stays EMPTY so enrichServedModels can fill it from the memo once the agent has answered.
func (p *AzureFoundryProvider) agentModels(agents []azureAgent, deployedModels map[string]string) []ModelInfo {
	out := make([]ModelInfo, 0, len(agents))
	for _, a := range agents {
		name := a.label()
		display := "Agent · " + name
		if a.kind() == agentKindWorkflow {
			// A preview kind that retires on December 1, 2026: say what it is.
			display += " (workflow)"
		}
		m := ModelInfo{
			ID:           agentModelPrefix + name,
			DisplayName:  display,
			ContextLimit: p.ContextLimit(),
		}
		if dep := a.model(); dep != "" {
			served := ""
			if s, ok := deployedModels[strings.ToLower(dep)]; ok {
				served = s
			} else if _, ok := catalogEntryFor(azureFoundryProviderID, dep); ok {
				served = dep
			}
			if served != "" {
				applyServedModel(azureFoundryProviderID, &m, served)
				// annotateServedModel leaves a display name that already
				// carries " · " alone ("Agent · name"): name the model here.
				if !strings.HasSuffix(m.DisplayName, " · "+served) {
					m.DisplayName += " · " + served
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// azureAgent is the subset of the agent object the listing needs. The model and kind sit on the latest
// version's definition; older shapes carry them at the top level, so both are read.
type azureAgent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
	// State is the agent's operational state (enabled | disabled).
	State    string `json:"state"`
	Versions struct {
		Latest struct {
			// Status is the latest version's provisioning status (creating | active | failed | deleting | deleted); optional.
			Status     string `json:"status"`
			Definition struct {
				Kind  string `json:"kind"`
				Model string `json:"model"`
			} `json:"definition"`
		} `json:"latest"`
	} `json:"versions"`
	Definition struct {
		Kind  string `json:"kind"`
		Model string `json:"model"`
	} `json:"definition"`
}

// label is the agent's name (the id when the listing carries no name).
func (a azureAgent) label() string {
	if name := strings.TrimSpace(a.Name); name != "" {
		return name
	}
	return strings.TrimSpace(a.ID)
}

// kind is the agent kind (prompt | workflow | hosted | external | voice), lower-cased; "" when the listing does not say.
func (a azureAgent) kind() string {
	for _, k := range []string{a.Versions.Latest.Definition.Kind, a.Definition.Kind} {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			return k
		}
	}
	return ""
}

// runsViaAgentReference reports whether the shared {project}/openai/v1/responses route can run the agent by
// agent_reference: prompt and workflow agents, or an absent kind (older shapes). Hosted, external (A2A) and
// voice agents are not Responses chat targets.
func (a azureAgent) runsViaAgentReference() bool {
	switch a.kind() {
	case "", "prompt", agentKindWorkflow:
		return true
	}
	return false
}

// usable reports whether the agent can answer now: state "enabled" and, when the listing reports a status, the
// latest version "active". An absent state is an older listing shape that predates the field and is kept.
func (a azureAgent) usable() bool {
	if s := strings.TrimSpace(a.State); s != "" && !strings.EqualFold(s, "enabled") {
		return false
	}
	if s := strings.TrimSpace(a.Versions.Latest.Status); s != "" && !strings.EqualFold(s, "active") {
		return false
	}
	return true
}

// agentKindWorkflow is the workflow agent kind — a preview gated by the Foundry-Features opt-in header on
// every call (workflowFeatureHeader) that retires on December 1, 2026; they stay listed, marked "(workflow)", until then.
const agentKindWorkflow = "workflow"

// Workflow agents' preview opt-in: the Foundry-Features header and value the project data plane requires.
const (
	foundryFeaturesHeader = "Foundry-Features"
	workflowFeatureValue  = "WorkflowAgents=V1Preview"
)

// agentKinds remembers each listed agent's kind per project (the listing is the only place it is reported), so
// the chat path sends the workflow opt-in header without a listing call. Bounded; keyed by project + agent name.
var agentKinds = cache.New[agentKindEntry](agentKindsSpec, servedMemoMax, cache.WithMaxEntries(servedMemoMax), cache.WithTTL(servedMemoTTL))

type agentKindEntry struct {
	kind string
	seen time.Time
}

func agentKindKey(project, name string) cache.Key {
	return memoKey(agentKindsSpec, project+"\x00"+strings.ToLower(name))
}

// rememberAgentKind records a listed agent's kind ("" forgets it).
func rememberAgentKind(project, name, kind string) {
	key := agentKindKey(project, name)
	if kind == "" {
		agentKinds.Delete(key)
		return
	}
	agentKinds.Set(key, agentKindEntry{kind: kind, seen: time.Now()}, 1, 0)
}

// agentKindFor returns the remembered kind of an agent, "" when unknown.
func agentKindFor(project, name string) string {
	key := agentKindKey(project, name)
	e, ok := agentKinds.Get(key)
	if !ok {
		return ""
	}
	if time.Since(e.seen) > servedMemoTTL {
		agentKinds.Delete(key)
		return ""
	}
	return e.kind
}

// resetAgentKinds clears the memo (tests, ResetAzureFoundryCaches).
func resetAgentKinds() { agentKinds.Clear() }

// deploymentModelResolver maps a deployment name to the model the cached project listing says it serves: an
// agent run's response.model is the agent's DEPLOYMENT — a custom name prices at $0 and teaches the memo
// nothing — so the agent paths resolve it first. Unknown names pass through; the listing is read, never fetched.
func (p *AzureFoundryProvider) deploymentModelResolver() func(string) string {
	identity := func(s string) string { return s }
	models, ok := p.cachedListing()
	if !ok {
		return identity
	}
	served := map[string]string{}
	for _, m := range models {
		if m.ServedModel == "" || m.Embedding || IsAgentModel(m.ID) || IsClaudeModel(m.ID) {
			continue
		}
		served[strings.ToLower(m.ID)] = m.ServedModel
	}
	if len(served) == 0 {
		return identity
	}
	return func(s string) string {
		if m, ok := served[strings.ToLower(strings.TrimSpace(s))]; ok {
			return m
		}
		return s
	}
}

func (a azureAgent) model() string {
	for _, m := range []string{a.Versions.Latest.Definition.Model, a.Definition.Model, a.Model} {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	return ""
}

// sameProjectHost accepts a continuation link only when it stays on the configured project host — a listing
// must not redirect the credential elsewhere; a rejected link is logged (dropping it silently truncated listings).
func (p *AzureFoundryProvider) sameProjectHost(ctx context.Context, link string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	base, baseErr := url.Parse(p.cfg.ProjectEndpoint)
	if err != nil || baseErr != nil || !strings.EqualFold(u.Host, base.Host) || u.Scheme != "https" {
		logger.FromContext(ctx).Warn("azure ai foundry: rejected a listing continuation link that does not stay on the project host; the listing ends here",
			"link", oneLine(link, 200), "project_host", base.Host)
		return ""
	}
	return link
}

// projectGET performs an authenticated project GET and returns a 200's body; other statuses become classified
// errors. A 401 gets one retry with a fresh Entra token; a 401 that survives is a definitive
// *azureProjectStatusError still unwrapping to ErrApiKeyInvalid, so the listing is cached instead of re-asked
// every turn.
func (p *AzureFoundryProvider) projectGET(ctx context.Context, rawURL string) ([]byte, error) {
	// The two listings run concurrently on one provider: each call records the token IT sent, so a 401
	// invalidates that one (see entraTokenUse).
	callCtx, use := withEntraTokenUse(ctx)
	body, err := p.projectGETOnce(callCtx, rawURL)
	// A token Entra ID refused to issue (errEntraToken, also ErrApiKeyInvalid) is not the project rejecting
	// one: asking again is another failed sign-in.
	if err == nil || p.tokens == nil || !errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, errEntraToken) {
		return body, err
	}
	failed := use.get()
	p.tokens.Invalidate(failed)
	retryCtx, _ := withEntraTokenUse(ctx)
	fresh, terr := p.entraToken(retryCtx)
	if terr != nil {
		return nil, terr
	}
	if fresh != failed {
		if body, err = p.projectGETOnce(retryCtx, rawURL); !errors.Is(err, ErrApiKeyInvalid) {
			return body, err
		}
	}
	return nil, &azureProjectStatusError{status: http.StatusUnauthorized, message: "the Entra ID principal is not authorized on the project"}
}

func (p *AzureFoundryProvider) projectGETOnce(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := p.authorize(ctx, req); err != nil {
		return nil, err
	}
	resp, err := doProviderRequest(p.client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// The 200 path depends on the body, so a read error is reported as itself (a connection reset mid-body
	// used to surface as a parse error). Both it and an over-cap body stay non-definitive.
	body, err := io.ReadAll(io.LimitReader(resp.Body, azureProjectMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read project response (status %d): %w", resp.StatusCode, err)
	}
	if len(body) > azureProjectMaxBody {
		return nil, fmt.Errorf("project response exceeded %d MB", azureProjectMaxBody>>20)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusUnauthorized:
		return nil, ErrApiKeyInvalid
	case http.StatusTooManyRequests:
		return nil, rateLimitErr(resp)
	}
	se := &azureProjectStatusError{status: resp.StatusCode}
	var apiErr openAIErrorResp
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
		se.message = apiErr.Error.Message
	}
	return nil, se
}
