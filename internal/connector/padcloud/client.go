package padcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

	// maxRetries caps retry attempts on transient (429/5xx) errors. The
	// connector issues sequential requests; without retry, a single 429 from
	// Dataverse throttling marks that flow as failed for the entire sweep.
	maxRetries int
}

// maxDefinitionBytes bounds a flow-definition response body. Definitions are
// JSON action trees — even huge flows stay well under this; an unbounded read
// would let one malformed/hostile response balloon memory mid-sweep.
const maxDefinitionBytes = 32 << 20 // 32 MiB

// NewHTTPClient builds the client for a Dataverse environment.
func NewHTTPClient(auth *Authenticator, dataverseBase string) *HTTPClient {
	return &HTTPClient{
		auth:           auth,
		http:           &http.Client{Timeout: 60 * time.Second},
		dataverse:      strings.TrimRight(dataverseBase, "/"),
		listFlowsPath:  "workflows?$filter=category eq 6&$select=name,workflowid,modifiedon,_solutionid_value",
		flowDefPathFmt: "workflows(%s)/exportworkflowdefinition",
		maxRetries:     3,
	}
}

// retryableStatus reports whether a Dataverse HTTP status warrants a retry.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// retryAfter parses the Retry-After header (seconds or HTTP-date) and returns
// the delay to wait before the next attempt. Falls back to exponential backoff
// with jitter when the header is absent or unparseable.
func retryAfter(resp *http.Response, attempt int) time.Duration {
	// resp is nil on the network-error retry path (no response was received);
	// only consult Retry-After when there's an actual response to read.
	if resp != nil {
		if h := resp.Header.Get("Retry-After"); h != "" {
			if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
				return time.Duration(secs) * time.Second
			}
			if t, err := http.ParseTime(h); err == nil {
				return time.Until(t)
			}
		}
	}
	// Exponential backoff with full jitter: 1s, 2s, 4s (capped at 30s).
	base := time.Second << attempt
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	return time.Duration(rand.Int63n(int64(base) + 1)) // #nosec G404 -- backoff jitter, not security-sensitive
}

// doWithRetry executes a GET request, retrying on 429/5xx with exponential
// backoff (respecting Retry-After). The buildReq function lets each caller
// construct its own request (with the right path/headers). Returns the final
// response (caller must close Body) or the last error.
func (c *HTTPClient) doWithRetry(ctx context.Context, buildReq func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := buildReq()
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if attempt >= c.maxRetries || ctx.Err() != nil {
				return nil, lastErr
			}
			if sleepErr := sleepCtx(ctx, retryAfter(nil, attempt)); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}
		if retryableStatus(resp.StatusCode) && attempt < c.maxRetries {
			delay := retryAfter(resp, attempt)
			resp.Body.Close()
			if sleepErr := sleepCtx(ctx, delay); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}
		return resp, nil
	}
}

// sleepCtx sleeps for d, returning ctx.Err() if the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ListDesktopFlows enumerates desktop flows in the environment, following
// Dataverse OData pagination (@odata.nextLink) so environments with >5000
// flows are fully ingested rather than silently truncated to the first page.
func (c *HTTPClient) ListDesktopFlows(ctx context.Context) ([]DesktopFlowRef, error) {
	tok := c.auth.AccessToken()
	if tok == "" {
		return nil, fmt.Errorf("no access token — complete device-flow auth first")
	}

	var flows []DesktopFlowRef
	nextURL := c.dataverse + "/" + c.listFlowsPath

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		reqURL := nextURL
		resp, err := c.doWithRetry(ctx, func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("Accept", "application/json")
			return req, nil
		})
		if err != nil {
			return nil, fmt.Errorf("list flows: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("list flows: status %d: %s", resp.StatusCode, body)
		}

		var out struct {
			Value []struct {
				Name       string `json:"name"`
				WorkflowID string `json:"workflowid"`
				ModifiedOn string `json:"modifiedon"`
				Solution   string `json:"_solutionid_value"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode flows: %w", err)
		}
		resp.Body.Close()

		for _, f := range out.Value {
			mod, _ := time.Parse(time.RFC3339, f.ModifiedOn)
			flows = append(flows, DesktopFlowRef{ID: f.WorkflowID, Name: f.Name, Solution: f.Solution, Modified: mod})
		}

		// Follow pagination: @odata.nextLink is a full URL for the next page.
		// Absent or empty → no more pages.
		if out.NextLink == "" {
			break
		}
		nextURL = out.NextLink
	}

	return flows, nil
}

// GetFlowDefinition fetches a desktop flow's action-tree definition (the JSON
// the Converter turns into a FlowDocument). Retries on 429/5xx.
func (c *HTTPClient) GetFlowDefinition(ctx context.Context, flowID string) (json.RawMessage, error) {
	tok := c.auth.AccessToken()
	if tok == "" {
		return nil, fmt.Errorf("no access token — complete device-flow auth first")
	}
	path := fmt.Sprintf(c.flowDefPathFmt, url.PathEscape(flowID))
	reqURL := c.dataverse + "/" + path

	resp, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get definition: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("get definition: status %d: %s", resp.StatusCode, body)
	}
	// Bounded read: reject a definition that exceeds the cap instead of
	// silently truncating it (truncated JSON would fail in the converter with
	// a misleading parse error).
	def, err := io.ReadAll(io.LimitReader(resp.Body, maxDefinitionBytes+1))
	if err != nil {
		return nil, fmt.Errorf("get definition: read body: %w", err)
	}
	if len(def) > maxDefinitionBytes {
		return nil, fmt.Errorf("get definition: response exceeds %d bytes", maxDefinitionBytes)
	}
	return def, nil
}
