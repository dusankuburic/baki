package padcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HTTPClient is the concrete Power Platform / Dataverse client. It enumerates
// desktop flows and fetches their definitions using an Authenticator's bearer
// token. The endpoint paths below are the documented candidates against the
// Dataverse Web API; they MUST be validated against a real tenant before this
// connector is enabled in production (the converter is the other de-risking
// piece — see Converter).
//
// All endpoints are overridable via the client's fields so the request shape is
// unit-testable without a live environment.
type HTTPClient struct {
	auth      *Authenticator
	http      *http.Client
	dataverse string // e.g. https://{env}.crm.dynamics.com/api/data/v9.2

	// Overridable endpoint templates (defaulted in NewHTTPClient). Validating
	// these against a real tenant is the remaining unknown for the client.
	listFlowsPath  string // relative to dataverse; defaults to "workflows"
	flowDefPathFmt string // flowID → definition path; defaults to a documented candidate
}

// NewHTTPClient builds the client for a Dataverse environment.
func NewHTTPClient(auth *Authenticator, dataverseBase string) *HTTPClient {
	return &HTTPClient{
		auth:           auth,
		http:           &http.Client{Timeout: 60 * time.Second},
		dataverse:      dataverseBase,
		listFlowsPath:  "workflows?$filter=category eq 6&$select=name,workflowid,modifiedon,_solutionid_value", // category 6 = desktop flow (validate)
		flowDefPathFmt: "workflows(%s)/exportworkflowdefinition",                                               // candidate endpoint for the action-tree JSON (validate)
	}
}

// ListDesktopFlows enumerates desktop flows in the environment.
func (c *HTTPClient) ListDesktopFlows(ctx context.Context) ([]DesktopFlowRef, error) {
	tok := c.auth.AccessToken()
	if tok == "" {
		return nil, fmt.Errorf("no access token — complete device-flow auth first")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.dataverse+"/"+c.listFlowsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list flows: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list flows: status %d: %s", resp.StatusCode, body)
	}

	// Dataverse OData response: { value: [ { name, workflowid, modifiedon, ... } ] }
	var out struct {
		Value []struct {
			Name       string `json:"name"`
			WorkflowID string `json:"workflowid"`
			ModifiedOn string `json:"modifiedon"`
			Solution   string `json:"_solutionid_value"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode flows: %w", err)
	}
	flows := make([]DesktopFlowRef, 0, len(out.Value))
	for _, f := range out.Value {
		mod, _ := time.Parse(time.RFC3339, f.ModifiedOn)
		flows = append(flows, DesktopFlowRef{ID: f.WorkflowID, Name: f.Name, Solution: f.Solution, Modified: mod})
	}
	return flows, nil
}

// GetFlowDefinition fetches a desktop flow's action-tree definition (the JSON
// the Converter turns into a FlowDocument).
func (c *HTTPClient) GetFlowDefinition(ctx context.Context, flowID string) (json.RawMessage, error) {
	tok := c.auth.AccessToken()
	if tok == "" {
		return nil, fmt.Errorf("no access token — complete device-flow auth first")
	}
	path := fmt.Sprintf(c.flowDefPathFmt, url.PathEscape(flowID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.dataverse+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get definition: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("get definition: status %d: %s", resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}
