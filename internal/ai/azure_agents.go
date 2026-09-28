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
	"sync"

	"pad-core/logger"
)

// Foundry PROJECT endpoint extras (Entra ID only — the project data plane
// rejects API keys): the deployments listing, which reveals the model behind
// each deployment name, and the project's prompt agents, which the chat model
// picker offers as "Agent · <name>" entries with the id "agent:<name>".

// agentModelPrefix marks a model id that names a Foundry agent rather than a
// deployment. The chat service forces the tool loop for such ids (an agent's
// function call must be answered) and the provider routes them to the
// Responses API.
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
	// azureProjectMaxBody caps one project page (a page of deployments or
	// agents is a few KB).
	azureProjectMaxBody = 4 << 20
)

// projectExtras merges the project deployments listing into the document's
// deployments and appends the project's agents, reporting whether the result
// is complete (cacheable for the full TTL). The project listing is
// authoritative for what a deployment serves and lists the deployments the
// document does not name. Everything here is best effort: a definitive
// refusal (403, or a 401 that survived a fresh token — a missing role; 404
// classic hub project; 400 unsupported api-version) leaves the document's
// deployments intact and still counts as complete, so it is not re-requested
// on every chat turn; a transient failure (network, 5xx, 429, a token that
// cannot be obtained, the caller's deadline) returns what is known as
// incomplete, which the caller serves only briefly (azureModelsRetryAfter).
//
// The two listings run concurrently (each is paginated, and both sit on the
// time-to-first-token path of an Entra ID + project user). A deployments
// failure that makes the agents call pointless — the token cannot be
// obtained, or the project asked for back-off (429: the agents route shares
// its quota, and the listing is incomplete either way) — cancels the agents
// fetch instead of letting it page on.
func (p *AzureFoundryProvider) projectExtras(ctx context.Context, models []ModelInfo) ([]ModelInfo, bool) {
	if !p.cfg.listsProjectDeployments() {
		return models, true
	}
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
		deployments, depErr = p.fetchProjectDeployments(ctx)
		if depErr != nil && (errors.Is(depErr, errEntraToken) || errors.Is(depErr, ErrRateLimited)) {
			cancelAgents()
		}
	}()
	go func() {
		defer wg.Done()
		agents, agentsErr = p.fetchAgents(actx)
	}()
	wg.Wait()

	complete := true
	var deployedModels map[string]string
	switch {
	case depErr == nil:
		models = p.mergeProjectDeployments(models, deployments)
		deployedModels = deployedModelNames(deployments)
	case errors.Is(depErr, errEntraToken) || ctx.Err() != nil:
		logger.Warn("azure ai foundry: project deployments unavailable; skipping the agents", "error", depErr)
		return models, false
	case errors.Is(depErr, ErrApiKeyInvalid) && definitiveProjectFailure(depErr):
		// A fresh token was refused too: the principal has no role on the
		// project, and the agents route answers the same way.
		logger.Warn("azure ai foundry: project not accessible to the entra id principal; listing the configured deployments only", "error", depErr)
		return models, true
	case errors.Is(depErr, ErrRateLimited):
		logger.Warn("azure ai foundry: project deployments throttled; the agents listing was abandoned", "error", depErr)
		return models, false
	default:
		logger.Warn("azure ai foundry: project deployments unavailable", "error", depErr)
		complete = definitiveProjectFailure(depErr)
	}
	if agentsErr != nil {
		logger.Warn("azure ai foundry: project agents unavailable", "error", agentsErr)
		return models, complete && definitiveProjectFailure(agentsErr) && ctx.Err() == nil
	}
	return append(models, p.agentModels(agents, deployedModels)...), complete && ctx.Err() == nil
}

// deployedModelNames maps each listed deployment (lower-cased name) to the
// model it serves, for resolving what an agent runs on.
func deployedModelNames(deployments []projectDeployment) map[string]string {
	out := make(map[string]string, len(deployments))
	for _, d := range deployments {
		if d.modelName != "" {
			out[strings.ToLower(d.name)] = d.modelName
		}
	}
	return out
}

// mergeProjectDeployments applies the project listing to the document's
// entries — the model behind each one (price, window and output cap from the
// catalogue, replacing any guess made from the deployment name) and the
// embedding flag — then appends the project's other deployments in listing
// order. A deployment the listing reports as Anthropic-published is a Claude
// deployment: it is listed under its "claude:<name>" id (the Anthropic
// Messages route), and a document entry that named it as a chat-completions
// deployment is re-listed under that id, since /openai/v1 cannot serve it.
func (p *AzureFoundryProvider) mergeProjectDeployments(models []ModelInfo, deployments []projectDeployment) []ModelInfo {
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
				logger.Debug("azure ai foundry: deployment is Anthropic-published; listing it as a Claude deployment", "deployment", cd.name)
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
	// embedding: the deployment serves an embedding model (its capabilities
	// say embeddings without chat completion, or its model name says so).
	embedding bool
	// claude: the deployment's model is published by Anthropic, so it
	// answers on the Anthropic Messages route (azure_claude.go).
	claude bool
}

// listingID is the model-list id the deployment is offered under: the
// deployment name, prefixed for a Claude deployment.
func (d projectDeployment) listingID() string {
	if d.claude {
		return claudeModelPrefix + d.name
	}
	return d.name
}

// fetchProjectDeployments calls GET {project}/deployments?api-version=v1 and
// returns the deployments in listing order, deduplicated by lower-cased name.
func (p *AzureFoundryProvider) fetchProjectDeployments(ctx context.Context) ([]projectDeployment, error) {
	var out []projectDeployment
	seen := map[string]bool{}
	next := p.cfg.ProjectEndpoint + "/deployments?api-version=" + azureProjectAPIVersion
	for page := 0; next != "" && page < azureProjectMaxPages; page++ {
		body, err := p.projectGET(ctx, next)
		if err != nil {
			return nil, err
		}
		// The page envelope (value / nextLink) is stable; item bodies vary by
		// deployment kind, so each is decoded on its own and an undecodable
		// one (a future kind whose capabilities are not a string map) costs
		// that entry, not the whole page — which used to make the listing
		// incomplete, hence uncached and re-fetched on every turn, for as
		// long as that deployment existed.
		var parsed struct {
			Value    []json.RawMessage `json:"value"`
			NextLink string            `json:"nextLink"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parse deployments: %w", err)
		}
		for _, raw := range parsed.Value {
			var d struct {
				Name           string         `json:"name"`
				Type           string         `json:"type"`
				ModelName      string         `json:"modelName"`
				ModelPublisher string         `json:"modelPublisher"`
				Capabilities   map[string]any `json:"capabilities"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				logger.Debug("azure ai foundry: skipping undecodable project deployment item", "error", err)
				continue
			}
			name := strings.TrimSpace(d.Name)
			if name == "" || seen[strings.ToLower(name)] {
				continue
			}
			seen[strings.ToLower(name)] = true
			modelName := strings.TrimSpace(d.ModelName)
			claude := strings.EqualFold(strings.TrimSpace(d.ModelPublisher), anthropicModelPublisher)
			embedding := !claude && (isEmbeddingModelName(modelName) ||
				(capabilityTrue(d.Capabilities, "embeddings") && !capabilityTrue(d.Capabilities, "chat_completion")))
			out = append(out, projectDeployment{name: name, modelName: modelName, embedding: embedding, claude: claude})
		}
		next = p.sameProjectHost(parsed.NextLink)
	}
	if next != "" {
		// The listing was cut by the page cap with more pages pending — say
		// so, or a deployment the model picker is missing looks like a bug in
		// the merge instead of a truncation nobody reported.
		logger.Warn("azure ai foundry: project deployments listing stopped at the page cap; deployments beyond it are not listed",
			"pages", azureProjectMaxPages, "listed", len(out))
	}
	return out, nil
}

// isEmbeddingModelName reports whether a model name denotes an embedding
// model (text-embedding-3-small, embed-v-4-0, Cohere-embed-v3-english, …).
func isEmbeddingModelName(name string) bool {
	return strings.Contains(strings.ToLower(name), "embed")
}

// capabilityTrue reads one flag of a project deployment's capabilities map,
// whose values the service sends as strings ("true") — booleans accepted too.
func capabilityTrue(caps map[string]any, key string) bool {
	switch v := caps[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// azureProjectStatusError is a project route's answer other than 200, 401
// (ErrApiKeyInvalid) and 429 (RateLimitError).
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

// definitiveProjectFailure reports whether a project listing failure is an
// answer that will not change on a retry — a 4xx status error other than 408
// (429 and a first 401 never become one): 403 or a 401 that survived a fresh
// token (the principal lacks the role), 404 (a classic hub project without
// the v1 routes), 400 (api-version not supported).
func definitiveProjectFailure(err error) bool {
	var se *azureProjectStatusError
	return errors.As(err, &se) && se.status >= 400 && se.status < 500 && se.status != http.StatusRequestTimeout
}

// fetchAgents calls GET {project}/agents?api-version=v1 (paginated) and
// returns each agent the shared Responses route can run — prompt and workflow
// agents, or no kind (older shapes). Hosted agents answer only on their own
// endpoint, external (A2A) and voice agents are not Responses chat targets;
// they are skipped and do not count toward the cap. agentModels turns the
// result into model-list entries once the deployments listing is known.
func (p *AzureFoundryProvider) fetchAgents(ctx context.Context) ([]azureAgent, error) {
	var out []azureAgent
	next := p.cfg.ProjectEndpoint + "/agents?api-version=" + azureProjectAPIVersion
	for page := 0; next != "" && page < azureProjectMaxPages && len(out) < azureProjectMaxAgents; page++ {
		body, err := p.projectGET(ctx, next)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Data     []json.RawMessage `json:"data"`
			Value    []json.RawMessage `json:"value"`
			HasMore  bool              `json:"has_more"`
			LastID   string            `json:"last_id"`
			NextLink string            `json:"nextLink"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parse agents: %w", err)
		}
		items := parsed.Data
		if len(items) == 0 {
			items = parsed.Value
		}
		for _, raw := range items {
			// Per item, as for deployments: one agent of an unexpected shape
			// must not make the whole listing incomplete.
			var a azureAgent
			if err := json.Unmarshal(raw, &a); err != nil {
				logger.Debug("azure ai foundry: skipping undecodable project agent item", "error", err)
				continue
			}
			name := a.label()
			if name == "" {
				continue
			}
			if !a.runsViaAgentReference() {
				logger.Debug("azure ai foundry: skipping agent not callable via agent_reference", "agent", name, "kind", a.kind())
				continue
			}
			out = append(out, a)
			if len(out) >= azureProjectMaxAgents {
				break
			}
		}
		switch {
		case parsed.NextLink != "":
			next = p.sameProjectHost(parsed.NextLink)
		case parsed.HasMore && parsed.LastID != "":
			next = p.cfg.ProjectEndpoint + "/agents?api-version=" + azureProjectAPIVersion + "&after=" + url.QueryEscape(parsed.LastID)
		default:
			next = ""
		}
	}
	// Both caps truncate silently otherwise; a missing agent in the picker
	// then looks like a merge bug instead of a listing that was cut short.
	if next != "" {
		logger.Warn("azure ai foundry: project agents listing stopped at the page cap; agents beyond it are not listed",
			"pages", azureProjectMaxPages, "listed", len(out))
	}
	if len(out) >= azureProjectMaxAgents {
		logger.Warn("azure ai foundry: project agents listing stopped at the agent cap; agents beyond it are not listed",
			"cap", azureProjectMaxAgents)
	}
	return out, nil
}

// agentModels maps the listed agents to model-list entries ("agent:<name>",
// "Agent · <name>"). The definition's model is the PROJECT DEPLOYMENT the
// agent runs on, not a model name: the project listing (deployedModels,
// authoritative) names the model behind it, and a deployment named after its
// model (the portal default) resolves through the catalogue. Otherwise
// ServedModel stays EMPTY so enrichServedModels can fill it from the memo
// once the agent has answered — a pre-set deployment name blocked that
// forever, leaving a custom-named deployment's agent with the default window
// and no output cap.
func (p *AzureFoundryProvider) agentModels(agents []azureAgent, deployedModels map[string]string) []ModelInfo {
	out := make([]ModelInfo, 0, len(agents))
	for _, a := range agents {
		name := a.label()
		m := ModelInfo{
			ID:           agentModelPrefix + name,
			DisplayName:  "Agent · " + name,
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

// azureAgent is the subset of the agent object the listing needs. The model
// and kind sit on the latest version's definition; older shapes carry them at
// the top level, so both are read.
type azureAgent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	Versions struct {
		Latest struct {
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

// kind is the agent kind (prompt | workflow | hosted | external | voice),
// lower-cased; "" when the listing does not say.
func (a azureAgent) kind() string {
	for _, k := range []string{a.Versions.Latest.Definition.Kind, a.Definition.Kind} {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			return k
		}
	}
	return ""
}

// runsViaAgentReference reports whether the shared
// {project}/openai/v1/responses route can run the agent by agent_reference:
// prompt and workflow agents can, and an absent kind (older shapes) is kept.
// Hosted agents need their dedicated endpoint; external (A2A) and voice
// agents are not Responses chat targets.
func (a azureAgent) runsViaAgentReference() bool {
	switch a.kind() {
	case "", "prompt", "workflow":
		return true
	}
	return false
}

func (a azureAgent) model() string {
	for _, m := range []string{a.Versions.Latest.Definition.Model, a.Definition.Model, a.Model} {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	return ""
}

// sameProjectHost accepts a continuation link only when it stays on the
// configured project host — a listing must not redirect the credential
// elsewhere. A non-empty link that fails the check is logged: dropping it
// silently truncated the listing with nothing in the log to show why.
func (p *AzureFoundryProvider) sameProjectHost(link string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	base, baseErr := url.Parse(p.cfg.ProjectEndpoint)
	if err != nil || baseErr != nil || !strings.EqualFold(u.Host, base.Host) || u.Scheme != "https" {
		logger.Warn("azure ai foundry: rejected a listing continuation link that does not stay on the project host; the listing ends here",
			"link", oneLine(link, 200), "project_host", base.Host)
		return ""
	}
	return link
}

// projectGET performs an authenticated GET against the project endpoint and
// returns the body of a 200; other statuses become classified errors. A 401
// gets one retry with a fresh Entra token (a cached token can be revoked out
// of band); a 401 that survives it is the principal's real permission (a
// missing project role) and comes back as a definitive
// *azureProjectStatusError that still unwraps to ErrApiKeyInvalid, so the
// listing without project data is cached instead of re-asked every turn.
func (p *AzureFoundryProvider) projectGET(ctx context.Context, rawURL string) ([]byte, error) {
	body, err := p.projectGETOnce(ctx, rawURL)
	// A token Entra ID refused to issue (errEntraToken, also ErrApiKeyInvalid)
	// is not the project rejecting one: asking again is another failed sign-in.
	if err == nil || p.tokens == nil || !errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, errEntraToken) {
		return body, err
	}
	p.mu.Lock()
	failed := p.lastToken
	p.mu.Unlock()
	p.tokens.Invalidate(failed)
	fresh, terr := p.entraToken(ctx)
	if terr != nil {
		return nil, terr
	}
	if fresh != failed {
		if body, err = p.projectGETOnce(ctx, rawURL); !errors.Is(err, ErrApiKeyInvalid) {
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
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// The 200 path depends on the body, so a read error is reported as
	// itself (a connection reset or the caller's deadline mid-body used to
	// surface as "parse deployments: unexpected end of JSON input", and a
	// body over the cap was cut without a word). Both stay non-definitive.
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
