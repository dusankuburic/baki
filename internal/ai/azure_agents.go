package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

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
)

// projectExtras applies the project deployments metadata to the listing and
// appends the project's agents. Everything here is best effort: a project
// endpoint that answers 403/404 (missing role, classic hub project) leaves the
// plain deployments listing intact.
func (p *AzureFoundryProvider) projectExtras(ctx context.Context, models []ModelInfo) []ModelInfo {
	if p.cfg.ProjectEndpoint == "" || p.cfg.Auth.Mode != AzureAuthModeEntra {
		return models
	}
	deployments, err := p.fetchProjectDeployments(ctx)
	if err != nil {
		logger.Warn("azure ai foundry: project deployments unavailable", "error", err)
	} else {
		seen := map[string]bool{}
		for i := range models {
			seen[strings.ToLower(models[i].ID)] = true
			if d, ok := deployments[strings.ToLower(models[i].ID)]; ok && d.modelName != "" {
				applyServedModel(azureFoundryProviderID, &models[i], d.modelName)
			}
		}
		// Deployments the project knows that /openai/v1/models did not list.
		for _, d := range deployments {
			if seen[strings.ToLower(d.name)] || d.name == "" {
				continue
			}
			m := ModelInfo{ID: d.name, DisplayName: d.name, ContextLimit: p.ContextLimit()}
			if d.modelName != "" {
				applyServedModel(azureFoundryProviderID, &m, d.modelName)
			}
			models = append(models, m)
		}
	}
	agents, err := p.fetchAgents(ctx)
	if err != nil {
		logger.Warn("azure ai foundry: project agents unavailable", "error", err)
		return models
	}
	return append(models, agents...)
}

type projectDeployment struct {
	name      string
	modelName string
}

// fetchProjectDeployments calls GET {project}/deployments?api-version=v1 and
// returns the deployments keyed by lower-cased name.
func (p *AzureFoundryProvider) fetchProjectDeployments(ctx context.Context) (map[string]projectDeployment, error) {
	out := map[string]projectDeployment{}
	next := p.cfg.ProjectEndpoint + "/deployments?api-version=" + azureProjectAPIVersion
	for page := 0; next != "" && page < azureProjectMaxPages; page++ {
		body, err := p.projectGET(ctx, next)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Value []struct {
				Name      string `json:"name"`
				Type      string `json:"type"`
				ModelName string `json:"modelName"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parse deployments: %w", err)
		}
		for _, d := range parsed.Value {
			name := strings.TrimSpace(d.Name)
			if name == "" {
				continue
			}
			out[strings.ToLower(name)] = projectDeployment{name: name, modelName: strings.TrimSpace(d.ModelName)}
		}
		next = p.sameProjectHost(parsed.NextLink)
	}
	return out, nil
}

// fetchAgents calls GET {project}/agents?api-version=v1 (paginated) and maps
// each agent to a model-list entry.
func (p *AzureFoundryProvider) fetchAgents(ctx context.Context) ([]ModelInfo, error) {
	var out []ModelInfo
	next := p.cfg.ProjectEndpoint + "/agents?api-version=" + azureProjectAPIVersion
	for page := 0; next != "" && page < azureProjectMaxPages && len(out) < azureProjectMaxAgents; page++ {
		body, err := p.projectGET(ctx, next)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Data     []azureAgent `json:"data"`
			Value    []azureAgent `json:"value"`
			HasMore  bool         `json:"has_more"`
			LastID   string       `json:"last_id"`
			NextLink string       `json:"nextLink"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parse agents: %w", err)
		}
		agents := parsed.Data
		if len(agents) == 0 {
			agents = parsed.Value
		}
		for _, a := range agents {
			name := strings.TrimSpace(a.Name)
			if name == "" {
				name = strings.TrimSpace(a.ID)
			}
			if name == "" {
				continue
			}
			m := ModelInfo{
				ID:           agentModelPrefix + name,
				DisplayName:  "Agent · " + name,
				ContextLimit: p.ContextLimit(),
			}
			if model := a.model(); model != "" {
				m.ServedModel = model
				if entry, ok := catalogEntryFor(azureFoundryProviderID, model); ok {
					m.Pricing = entry.Pricing
					if entry.ContextLimit > 0 {
						m.ContextLimit = entry.ContextLimit
					}
				}
			}
			out = append(out, m)
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
	return out, nil
}

// azureAgent is the subset of the agent object the listing needs. The model
// sits on the latest version's definition; older shapes carry it at the top
// level, so both are read.
type azureAgent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	Versions struct {
		Latest struct {
			Definition struct {
				Model string `json:"model"`
			} `json:"definition"`
		} `json:"latest"`
	} `json:"versions"`
	Definition struct {
		Model string `json:"model"`
	} `json:"definition"`
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
// elsewhere.
func (p *AzureFoundryProvider) sameProjectHost(link string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	base, err := url.Parse(p.cfg.ProjectEndpoint)
	if err != nil || !strings.EqualFold(u.Host, base.Host) || u.Scheme != "https" {
		return ""
	}
	return link
}

// projectGET performs an authenticated GET against the project endpoint and
// returns the body of a 200; other statuses become classified errors.
func (p *AzureFoundryProvider) projectGET(ctx context.Context, rawURL string) ([]byte, error) {
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusUnauthorized:
		return nil, ErrApiKeyInvalid
	case http.StatusTooManyRequests:
		return nil, rateLimitErr(resp)
	}
	var apiErr openAIErrorResp
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, apiErr.Error.Message)
	}
	return nil, fmt.Errorf("status %d", resp.StatusCode)
}
