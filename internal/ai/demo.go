package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var DemoProxyURL string

type DemoLimiter struct {
	mu          sync.Mutex
	counterFile string
	dailyLimit  int
}

type demoState struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

func NewDemoLimiter(configDir string) *DemoLimiter {
	return &DemoLimiter{
		counterFile: filepath.Join(configDir, "demo.json"),
		dailyLimit:  5,
	}
}

func (l *DemoLimiter) Remaining() (remaining int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, err := l.loadState()
	if err != nil {
		return l.dailyLimit, err
	}

	today := time.Now().Format("2006-01-02")
	if state.Date != today {
		return l.dailyLimit, nil
	}

	rem := l.dailyLimit - state.Count
	if rem < 0 {
		rem = 0
	}
	return rem, nil
}

func (l *DemoLimiter) ReserveForDisplay() (remaining int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, err := l.loadState()
	if err != nil {
		return 0, err
	}

	today := time.Now().Format("2006-01-02")
	if state.Date != today {
		state.Date = today
		state.Count = 0
	}

	if state.Count >= l.dailyLimit {
		return 0, fmt.Errorf("daily demo limit reached (%d)", l.dailyLimit)
	}

	state.Count++
	if err := l.saveState(state); err != nil {
		return 0, err
	}

	return l.dailyLimit - state.Count, nil
}

func (l *DemoLimiter) loadState() (*demoState, error) {
	data, err := os.ReadFile(l.counterFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &demoState{}, nil
		}
		return nil, err
	}
	var state demoState
	if err := json.Unmarshal(data, &state); err != nil {
		// A corrupt counter file must NOT silently reset to an empty state —
		// that would grant the full daily limit again (effectively unlimited).
		// Fail closed: surface the error so the enforcement path (ReserveForDisplay)
		// denies rather than resets. A missing file (first run) is handled above.
		return nil, fmt.Errorf("demo state file corrupt: %w", err)
	}
	return &state, nil
}

func (l *DemoLimiter) saveState(state *demoState) error {
	dir := filepath.Dir(l.counterFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	// Atomic write: temp file then rename, so a crash mid-write can't leave a
	// truncated counter file (which loadState would then reject as corrupt).
	tmp := l.counterFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.counterFile); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

type DemoProvider struct {
	client *http.Client
}

func NewDemoProvider() *DemoProvider {
	return &DemoProvider{
		client: sharedHTTPClient,
	}
}

func (d *DemoProvider) ID() string          { return "demo" }
func (d *DemoProvider) Name() string        { return "Demo" }
func (d *DemoProvider) ContextLimit() int    { return 200000 }
func (d *DemoProvider) DefaultModel() string { return "demo" }
func (d *DemoProvider) FreeModel() string    { return "" }

func (d *DemoProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 0, OutputCostPerM: 0}
}

func (d *DemoProvider) EstimateTokens(text string) int { return EstimateTokens(text) }

func (d *DemoProvider) Models() []ModelInfo {
	return []ModelInfo{
		{ID: "demo", DisplayName: "Demo Mode", ContextLimit: 200000},
	}
}

func (d *DemoProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	if DemoProxyURL == "" {
		return nil, fmt.Errorf("demo mode not available")
	}

	payload := map[string]interface{}{
		"messages":      req.Messages,
		"systemPrompt":  req.SystemPrompt,
		"maxTokens":     orDefault(req.MaxTokens, 4096),
		"temperature":   req.Temperature,
	}
	jsonBody, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", DemoProxyURL+"/chat", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("demo proxy request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode == 429 {
		return nil, ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		var demoErr struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(respBody, &demoErr); err == nil {
			msg := demoErr.Error
			if msg == "" {
				msg = demoErr.Message
			}
			if msg != "" {
				return nil, fmt.Errorf("demo proxy error (status %d): %s", resp.StatusCode, msg)
			}
		}
		return nil, fmt.Errorf("demo proxy error (status %d)", resp.StatusCode)
	}

	var parsed Response
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse demo response: %w", err)
	}
	return &parsed, nil
}

func (d *DemoProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	if DemoProxyURL == "" {
		return fmt.Errorf("demo mode not available")
	}

	payload := map[string]interface{}{
		"messages":      req.Messages,
		"systemPrompt":  req.SystemPrompt,
		"maxTokens":     orDefault(req.MaxTokens, 4096),
		"temperature":   req.Temperature,
		"stream":        true,
	}
	jsonBody, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", DemoProxyURL+"/chat", bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("demo proxy stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var demoErr struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &demoErr); err == nil {
			msg := demoErr.Error
			if msg == "" {
				msg = demoErr.Message
			}
			if msg != "" {
				return fmt.Errorf("demo proxy stream error (status %d): %s", resp.StatusCode, msg)
			}
		}
		return fmt.Errorf("demo proxy stream error (status %d)", resp.StatusCode)
	}

	return parseOpenAISSE(resp.Body, onChunk)
}
