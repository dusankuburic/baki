package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type GeminiProvider struct {
	apiKey string
	client *http.Client
}

func NewGeminiProvider(apiKey string) *GeminiProvider {
	return &GeminiProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (g *GeminiProvider) ID() string          { return "gemini" }
func (g *GeminiProvider) Name() string        { return "Gemini" }
func (g *GeminiProvider) ContextLimit() int    { return 1048576 }
func (g *GeminiProvider) DefaultModel() string { return "gemini-2.5-pro" }
func (g *GeminiProvider) FreeModel() string    { return "" }

func (g *GeminiProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 1.25, OutputCostPerM: 10.0}
}

func (g *GeminiProvider) EstimateTokens(text string) int {
	return EstimateTokensGemini(text)
}

func (g *GeminiProvider) Models() []ModelInfo {
	return []ModelInfo{
		{
			ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro",
			ContextLimit: 1048576,
			Pricing:      Pricing{InputCostPerM: 1.25, OutputCostPerM: 10.0},
		},
		{
			ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash",
			ContextLimit: 1048576,
			Pricing:      Pricing{InputCostPerM: 0.15, OutputCostPerM: 0.6},
		},
	}
}

var geminiURL = defaultGeminiURL

func defaultGeminiURL(model, apiKey, action string) string {
	base := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:%s", model, action)
	if action == "streamGenerateContent" {
		return base + "?alt=sse&key=" + apiKey
	}
	return base + "?key=" + apiKey
}

type geminiRequest struct {
	Contents         []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig *geminiGenConfig  `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
			Role string `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type geminiErrorResp struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func toGeminiContents(msgs []Message) []geminiContent {
	out := make([]geminiContent, len(msgs))
	for i, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		out[i] = geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: m.Content}},
		}
	}
	return out
}

func (g *GeminiProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := geminiRequest{
		Contents: toGeminiContents(req.Messages),
		GenerationConfig: &geminiGenConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: orDefault(req.MaxTokens, 4096),
		},
	}
	if req.SystemPrompt != "" {
		body.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: req.SystemPrompt}},
		}
	}

	jsonBody, _ := json.Marshal(body)
	url := geminiURL(req.Model, g.apiKey, "generateContent")

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode != http.StatusOK {
		var apiErr geminiErrorResp
		if err := json.Unmarshal(respBody, &apiErr); err == nil {
			switch {
			case resp.StatusCode == 400 && strings.Contains(apiErr.Error.Message, "API key"):
				return nil, ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return nil, ErrRateLimited
			case resp.StatusCode >= 500:
				return nil, fmt.Errorf("gemini server error: %s", apiErr.Error.Message)
			}
			return nil, fmt.Errorf("gemini API: %s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("gemini API error (status %d)", resp.StatusCode)
	}

	var parsed geminiResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse gemini response: %w", err)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return nil, errors.New("gemini returned no content")
	}

	tokensIn, tokensOut := 0, 0
	if parsed.UsageMetadata != nil {
		tokensIn = parsed.UsageMetadata.PromptTokenCount
		tokensOut = parsed.UsageMetadata.CandidatesTokenCount
	}

	return &Response{
		Content:      parsed.Candidates[0].Content.Parts[0].Text,
		TokensIn:     tokensIn,
		TokensOut:    tokensOut,
		FinishReason: parsed.Candidates[0].FinishReason,
	}, nil
}

func (g *GeminiProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := geminiRequest{
		Contents: toGeminiContents(req.Messages),
		GenerationConfig: &geminiGenConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: orDefault(req.MaxTokens, 4096),
		},
	}
	if req.SystemPrompt != "" {
		body.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: req.SystemPrompt}},
		}
	}

	jsonBody, _ := json.Marshal(body)
	url := geminiURL(req.Model, g.apiKey, "streamGenerateContent")

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var apiErr geminiErrorResp
		if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
			return fmt.Errorf("gemini stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return fmt.Errorf("gemini stream error (status %d)", resp.StatusCode)
	}

	return parseGeminiSSE(resp.Body, onChunk)
}
