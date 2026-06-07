package ai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// errStreamTruncated wraps io.ErrUnexpectedEOF for SSE bodies that ended
// cleanly without sending a provider-specific terminal marker (Claude's
// `message_stop` / `[DONE]`, OpenAI's `[DONE]` / `finish_reason`, Gemini's
// non-empty `finishReason`). Previously each parser synthesized a `Done`
// chunk in this case, so a network-truncated response was indistinguishable
// from a complete one. Returning an error here lets the chat service emit
// a stream-error event so the client can show "incomplete response."
func errStreamTruncated(provider string) error {
	return fmt.Errorf("%s stream truncated before terminal marker: %w", provider, io.ErrUnexpectedEOF)
}

// errStreamMalformed reports a stream that ended without a terminal marker AND
// had one or more undecodable `data:` events. Previously each parser silently
// `continue`d past JSON-unmarshal failures, so a fully malformed stream looked
// like a clean empty response; surfacing it lets the chat service show an error.
func errStreamMalformed(provider string, n int) error {
	return fmt.Errorf("%s stream had %d undecodable event(s) and no terminal marker", provider, n)
}

// Note on cancellation: every provider builds its request with
// http.NewRequestWithContext, so cancelling the caller's context aborts the body
// read and ends the scanner loop (surfacing via scanner.Err()). No explicit
// per-line context check is needed in these parsers.

func parseClaudeSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType string
	var doneSent bool
	var parseErrors int

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
				parseErrors++
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
				parseErrors++
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
				parseErrors++
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
				parseErrors++
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
		if parseErrors > 0 {
			return errStreamMalformed("claude", parseErrors)
		}
		return errStreamTruncated("claude")
	}
	return nil
}

func parseOpenAISSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		doneSent    bool
		sawTerminal bool
		tokensIn    int
		tokensOut   int
		parseErrors int
	)

	// Usage may arrive on the finish chunk or — when stream_options.include_usage
	// is set — in a trailing chunk whose `choices` array is empty (which the old
	// code dropped). Stash whatever usage we see and attach it to the single Done
	// chunk emitted at [DONE]/stream end, so the audited provider can record cost.
	emitDone := func() {
		if doneSent {
			return
		}
		doneSent = true
		onChunk(Chunk{Done: true, TokensIn: tokensIn, TokensOut: tokensOut})
	}

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			emitDone()
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
			parseErrors++
			continue
		}

		if parsed.Usage != nil {
			tokensIn = parsed.Usage.PromptTokens
			tokensOut = parsed.Usage.CompletionTokens
		}

		for _, choice := range parsed.Choices {
			if choice.Delta.Content != "" {
				onChunk(Chunk{Text: choice.Delta.Content})
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				sawTerminal = true
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading openai SSE stream: %w", err)
	}

	// A clean end is either an explicit [DONE] (handled above) or a terminal
	// finish_reason for servers that omit [DONE]. Anything else is a truncation.
	if sawTerminal {
		emitDone()
		return nil
	}
	if !doneSent {
		if parseErrors > 0 {
			return errStreamMalformed("openai", parseErrors)
		}
		return errStreamTruncated("openai")
	}
	return nil
}

func parseGeminiSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var doneSent bool
	var parseErrors int

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
			parseErrors++
			continue
		}

		for _, cand := range parsed.Candidates {
			for _, part := range cand.Content.Parts {
				if part.Text != "" {
					onChunk(Chunk{Text: part.Text})
				}
			}
			// Any non-empty FinishReason is terminal — STOP, MAX_TOKENS,
			// SAFETY, RECITATION, OTHER. Previously only STOP was treated
			// as terminal, so a MAX_TOKENS-capped response was reported as
			// truncated even though it was a clean end from the provider.
			if cand.FinishReason != "" {
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
		if parseErrors > 0 {
			return errStreamMalformed("gemini", parseErrors)
		}
		return errStreamTruncated("gemini")
	}
	return nil
}
