package ai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func parseClaudeSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType string
	var doneSent bool

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			if !doneSent {
				onChunk(Chunk{Done: true})
			}
			return nil
		}

		switch eventType {
		case "message_start":
			var parsed struct {
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				continue
			}
			if parsed.Message.Usage.InputTokens > 0 {
				onChunk(Chunk{TokensIn: parsed.Message.Usage.InputTokens})
			}

		case "content_block_delta":
			var parsed struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				continue
			}
			if parsed.Delta.Type == "text_delta" {
				onChunk(Chunk{Text: parsed.Delta.Text})
			}

		case "message_delta":
			var parsed struct {
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				continue
			}
			if parsed.Delta.StopReason != "" {
				doneSent = true
				onChunk(Chunk{Done: true, TokensOut: parsed.Usage.OutputTokens})
			}

		case "error":
			var parsed struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				continue
			}
			onChunk(Chunk{Error: fmt.Errorf("claude stream error: %s", parsed.Error.Message)})
			return nil
		}

		eventType = ""
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading claude SSE stream: %w", err)
	}

	if !doneSent {
		onChunk(Chunk{Done: true})
	}
	return nil
}

func parseOpenAISSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var doneSent bool

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			if !doneSent {
				onChunk(Chunk{Done: true})
			}
			return nil
		}

		var parsed struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			continue
		}

		for _, choice := range parsed.Choices {
			if choice.Delta.Content != "" {
				onChunk(Chunk{Text: choice.Delta.Content})
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				if !doneSent {
					doneSent = true
					tokensOut, tokensIn := 0, 0
					if parsed.Usage != nil {
						tokensOut = parsed.Usage.CompletionTokens
						tokensIn = parsed.Usage.PromptTokens
					}
					onChunk(Chunk{Done: true, TokensOut: tokensOut, TokensIn: tokensIn})
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading openai SSE stream: %w", err)
	}

	if !doneSent {
		onChunk(Chunk{Done: true})
	}
	return nil
}

func parseGeminiSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var doneSent bool

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		var parsed struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		}

		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			continue
		}

		for _, cand := range parsed.Candidates {
			for _, part := range cand.Content.Parts {
				if part.Text != "" {
					onChunk(Chunk{Text: part.Text})
				}
			}
			if cand.FinishReason == "STOP" {
				if !doneSent {
					doneSent = true
					tokensOut, tokensIn := 0, 0
					if parsed.UsageMetadata != nil {
						tokensOut = parsed.UsageMetadata.CandidatesTokenCount
						tokensIn = parsed.UsageMetadata.PromptTokenCount
					}
					onChunk(Chunk{Done: true, TokensOut: tokensOut, TokensIn: tokensIn})
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading gemini SSE stream: %w", err)
	}

	if !doneSent {
		onChunk(Chunk{Done: true})
	}
	return nil
}
